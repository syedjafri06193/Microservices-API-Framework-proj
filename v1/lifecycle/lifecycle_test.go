package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// --------------------------------------------------------------- health

func TestReadinessStartsFalse(t *testing.T) {
	// Flipping readiness before the listener is accepting sends traffic to
	// a connection refused.
	h := NewHealth()
	ready, _ := h.Ready()
	require.False(t, ready)
	require.True(t, h.Live(), "a running process is alive even before it is ready")
}

func TestHealthEndpointsAnswerIndependently(t *testing.T) {
	h := NewHealth()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	status := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Alive but not ready and not started: exactly the state during
	// startup, and the three endpoints must say three different things.
	require.Equal(t, http.StatusOK, status("/healthz"))
	require.Equal(t, http.StatusServiceUnavailable, status("/readyz"))
	require.Equal(t, http.StatusServiceUnavailable, status("/startupz"))

	h.SetStarted(true)
	h.SetReady(true)
	require.Equal(t, http.StatusOK, status("/readyz"))
	require.Equal(t, http.StatusOK, status("/startupz"))
}

func TestLocalCheckFailureMakesTheInstanceUnready(t *testing.T) {
	h := NewHealth()
	h.SetReady(true)

	var failing bool
	h.AddLocalCheck("pool", func() error {
		if failing {
			return errors.New("connection pool exhausted")
		}
		return nil
	})

	ready, _ := h.Ready()
	require.True(t, ready)

	failing = true
	ready, reasons := h.Ready()
	require.False(t, ready)
	require.Contains(t, reasons["pool"], "exhausted")
}

func TestFailingReadinessDoesNotAffectLiveness(t *testing.T) {
	// The cascading-failure trap in miniature. If readiness failure also
	// failed liveness, a shared dependency blip would restart every replica
	// simultaneously — and restarting does not fix a dependency.
	h := NewHealth()
	h.SetReady(false)
	require.True(t, h.Live())
}

// ------------------------------------------------------------- shutdown

func TestStepsRunInOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	step := func(name string) Step {
		return Step{Name: name, Run: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}}
	}

	seq := NewSequencer(discardLogger(), step("readiness"), step("drain"), step("deps"), step("flush"))
	require.NoError(t, seq.Run(context.Background(), time.Second))
	require.Equal(t, []string{"readiness", "drain", "deps", "flush"}, order)
}

func TestShutdownRunsEvenWhenTriggeredByACancelledContext(t *testing.T) {
	// The reason for context.WithoutCancel. Shutdown is usually triggered
	// by a cancelled context, and using it directly would cancel every step
	// immediately — losing the telemetry flush that explains why you shut
	// down in the first place.
	var ran bool
	seq := NewSequencer(discardLogger(), Step{
		Name: "flush telemetry",
		Run: func(ctx context.Context) error {
			require.NoError(t, ctx.Err(), "the shutdown step received a cancelled context")
			ran = true
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, seq.Run(ctx, time.Second))
	require.True(t, ran)
}

func TestContinueOnErrorKeepsTheSequenceGoing(t *testing.T) {
	// A failure closing a connection pool must not prevent telemetry from
	// flushing; the flush is frequently what explains the failure.
	var flushed bool
	seq := NewSequencer(discardLogger(),
		Step{Name: "deps", ContinueOnError: true, Run: func(context.Context) error {
			return errors.New("pool close failed")
		}},
		Step{Name: "flush", Run: func(context.Context) error {
			flushed = true
			return nil
		}},
	)

	err := seq.Run(context.Background(), time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pool close failed")
	require.True(t, flushed, "a non-fatal step's failure stopped the sequence")
}

func TestAFatalStepStopsTheSequence(t *testing.T) {
	var reached bool
	seq := NewSequencer(discardLogger(),
		Step{Name: "fatal", Run: func(context.Context) error { return errors.New("no") }},
		Step{Name: "after", Run: func(context.Context) error { reached = true; return nil }},
	)
	require.Error(t, seq.Run(context.Background(), time.Second))
	require.False(t, reached)
}

// --------------------------------------------------------------- timing

func TestTimingMustFitInsideTheGracePeriod(t *testing.T) {
	// If it does not, the platform sends SIGKILL mid-drain and all the
	// careful sequencing is wasted. The arithmetic is easy to get wrong
	// because the two halves live in different files — the framework's
	// config and a Kubernetes manifest.
	err := Timing{
		PreStopDelay: 5 * time.Second,
		DrainTimeout: 25 * time.Second,
		GracePeriod:  30 * time.Second,
	}.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "killed mid-drain")

	require.NoError(t, Timing{
		PreStopDelay: 5 * time.Second,
		DrainTimeout: 20 * time.Second,
		GracePeriod:  30 * time.Second,
	}.Validate())
}

func TestTimingWithoutAKnownGracePeriodIsAccepted(t *testing.T) {
	// Not every platform exposes it, and refusing to start because the
	// framework could not check would be worse than not checking.
	require.NoError(t, Timing{
		PreStopDelay: time.Hour,
		DrainTimeout: time.Hour,
	}.Validate())
}

func TestZeroDrainTimeoutIsRejected(t *testing.T) {
	require.Error(t, Timing{DrainTimeout: 0}.Validate())
}
