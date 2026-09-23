package breaker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/syedjafri06193/microservices-api-framework/resilience"
)

// fakeClock is the reason these tests run in microseconds instead of
// minutes. Every breaker property is time-dependent, and a suite built on
// real sleeps is both slow and flaky on the loaded machine CI runs on.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func serverFault() error { return connect.NewError(connect.CodeUnavailable, errors.New("down")) }
func clientFault() error { return connect.NewError(connect.CodeInvalidArgument, errors.New("bad")) }

// call runs one Allow/record cycle and reports whether it was admitted.
func call(t *testing.T, b *Breaker, err error) bool {
	t.Helper()
	done, allowErr := b.Allow()
	if allowErr != nil {
		require.ErrorIs(t, allowErr, resilience.ErrBreakerOpen)
		return false
	}
	done(err)
	return true
}

func testBreaker(t *testing.T, cfg Config, clock resilience.Clock) *Breaker {
	t.Helper()
	b, err := New("test", cfg, clock, nil)
	require.NoError(t, err)
	return b
}

func TestClosedBreakerAdmitsEverything(t *testing.T) {
	b := testBreaker(t, DefaultConfig(), newFakeClock())
	for i := 0; i < 100; i++ {
		require.True(t, call(t, b, nil))
	}
	require.Equal(t, StateClosed, b.State())
}

func TestTripsOnFailureRatioOverMinimumVolume(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 20
	cfg.FailureRatio = 0.5
	b := testBreaker(t, cfg, newFakeClock())

	// 19 failures is below the volume gate, so the ratio is not evaluated
	// even though it is 100%.
	for i := 0; i < 19; i++ {
		require.True(t, call(t, b, serverFault()))
	}
	require.Equal(t, StateClosed, b.State(), "tripped before reaching minimum volume")

	require.True(t, call(t, b, serverFault()))
	require.Equal(t, StateOpen, b.State())
}

func TestLowTrafficMethodDoesNotTrip(t *testing.T) {
	// The property the minimum-volume gate exists for: a method serving a
	// handful of requests should not go dark because two of them failed.
	cfg := DefaultConfig()
	b := testBreaker(t, cfg, newFakeClock())

	for i := 0; i < 5; i++ {
		require.True(t, call(t, b, serverFault()))
	}
	require.Equal(t, StateClosed, b.State())
}

func TestClientFaultsNeverTripTheBreaker(t *testing.T) {
	// The most important property in the package. If this regresses, one
	// caller sending malformed requests takes a healthy service offline
	// for every other caller.
	cfg := DefaultConfig()
	b := testBreaker(t, cfg, newFakeClock())

	for i := 0; i < 500; i++ {
		require.True(t, call(t, b, clientFault()))
	}
	require.Equal(t, StateClosed, b.State(), "InvalidArgument tripped the breaker")

	total, failures := b.Counts()
	require.Equal(t, 500, total)
	require.Equal(t, 0, failures, "client faults were counted as failures")
}

func TestCancellationDoesNotTripTheBreaker(t *testing.T) {
	// A user closing a browser tab is not evidence that a dependency is
	// sick.
	cfg := DefaultConfig()
	b := testBreaker(t, cfg, newFakeClock())
	cancelled := connect.NewError(connect.CodeCanceled, errors.New("client went away"))

	for i := 0; i < 200; i++ {
		require.True(t, call(t, b, cancelled))
	}
	require.Equal(t, StateClosed, b.State())
}

func TestSustainedFailuresBelowTheRatioStayClosed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 10
	cfg.FailureRatio = 0.5
	b := testBreaker(t, cfg, newFakeClock())

	// One failure in five, spread evenly: 20%, comfortably below the ratio.
	for i := 0; i < 100; i++ {
		if i%5 == 0 {
			call(t, b, serverFault())
		} else {
			call(t, b, nil)
		}
	}
	require.Equal(t, StateClosed, b.State())
}

func TestBurstyFailuresCanTripBelowTheAverageRatio(t *testing.T) {
	// Worth pinning down, because it surprised the first draft of these
	// tests. The ratio is evaluated on every call against the window so
	// far, not against the eventual average. Two failures at the head of
	// every group of five averages 40%, but the *running* ratio crosses
	// 50% as soon as the volume gate opens, and the breaker trips.
	//
	// That is the intended behaviour — a breaker that waited for a
	// long-run average would be useless at reacting to a burst — but it
	// means "average failure rate below the threshold" is not the same
	// claim as "will not trip".
	cfg := DefaultConfig()
	cfg.MinimumRequests = 10
	cfg.FailureRatio = 0.5
	b := testBreaker(t, cfg, newFakeClock())

	tripped := false
	for i := 0; i < 100; i++ {
		if b.State() != StateClosed {
			tripped = true
			break
		}
		if i%5 < 2 {
			call(t, b, serverFault())
		} else {
			call(t, b, nil)
		}
	}
	require.True(t, tripped, "a bursty 40% failure pattern never tripped the breaker")
}

func TestOpenBreakerRejectsWithoutCalling(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	clock := newFakeClock()
	b := testBreaker(t, cfg, clock)

	call(t, b, serverFault())
	call(t, b, serverFault())
	require.Equal(t, StateOpen, b.State())

	done, err := b.Allow()
	require.Nil(t, done)
	require.ErrorIs(t, err, resilience.ErrBreakerOpen)
}

func TestHalfOpenAfterOpenDurationThenClosesOnSuccesses(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	cfg.HalfOpenSuccesses = 3
	cfg.HalfOpenMaxCalls = 3
	clock := newFakeClock()
	b := testBreaker(t, cfg, clock)

	call(t, b, serverFault())
	call(t, b, serverFault())
	require.Equal(t, StateOpen, b.State())

	// Still open a moment before the duration elapses.
	clock.Advance(cfg.OpenDuration - time.Millisecond)
	_, err := b.Allow()
	require.ErrorIs(t, err, resilience.ErrBreakerOpen)

	clock.Advance(2 * time.Millisecond)
	for i := 0; i < cfg.HalfOpenSuccesses; i++ {
		require.True(t, call(t, b, nil), "probe %d was rejected", i)
	}
	require.Equal(t, StateClosed, b.State())
}

func TestHalfOpenReopensOnASingleFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	clock := newFakeClock()
	b := testBreaker(t, cfg, clock)

	call(t, b, serverFault())
	call(t, b, serverFault())
	clock.Advance(cfg.OpenDuration)

	require.True(t, call(t, b, nil))           // one good probe
	require.True(t, call(t, b, serverFault())) // then a bad one
	require.Equal(t, StateOpen, b.State(),
		"a failed probe must reopen immediately, not wait for the full ratio again")
}

func TestHalfOpenLimitsConcurrentProbes(t *testing.T) {
	// Letting full load through on recovery is how a struggling dependency
	// gets knocked back over.
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	cfg.HalfOpenMaxCalls = 2
	clock := newFakeClock()
	b := testBreaker(t, cfg, clock)

	call(t, b, serverFault())
	call(t, b, serverFault())
	clock.Advance(cfg.OpenDuration)

	// Take both probe slots without reporting results.
	d1, err := b.Allow()
	require.NoError(t, err)
	d2, err := b.Allow()
	require.NoError(t, err)

	_, err = b.Allow()
	require.ErrorIs(t, err, resilience.ErrBreakerOpen, "a third concurrent probe was admitted")

	d1(nil)
	d2(nil)
}

func TestProbeSlotIsReleasedExactlyOnce(t *testing.T) {
	// The callback is documented as call-once. A caller that calls it twice
	// must not be able to hand the breaker a spare probe slot.
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	cfg.HalfOpenMaxCalls = 1
	clock := newFakeClock()
	b := testBreaker(t, cfg, clock)

	call(t, b, serverFault())
	call(t, b, serverFault())
	clock.Advance(cfg.OpenDuration)

	before, _ := b.Counts()

	done, err := b.Allow()
	require.NoError(t, err)
	done(nil)
	done(nil) // ignored

	after, _ := b.Counts()
	require.Equal(t, before+1, after, "a double done() recorded the call twice")

	// And the probe slot came back exactly once, so the next probe is
	// admitted rather than the breaker being wedged half-open.
	next, err := b.Allow()
	require.NoError(t, err)
	next(nil)
}

func TestOldFailuresLeaveTheWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 4
	cfg.Window = 10 * time.Second
	cfg.Buckets = 10
	clock := newFakeClock()
	b := testBreaker(t, cfg, clock)

	// Three failures, then wait out the window.
	for i := 0; i < 3; i++ {
		call(t, b, serverFault())
		clock.Advance(time.Second)
	}
	clock.Advance(cfg.Window)

	total, failures := b.Counts()
	require.Zero(t, total, "expired buckets still counted")
	require.Zero(t, failures)

	// A fresh failure does not trip, because the old ones are gone.
	call(t, b, serverFault())
	require.Equal(t, StateClosed, b.State())
}

func TestBreakerOpenErrorDoesNotCountAsAFailure(t *testing.T) {
	// Otherwise an open breaker feeds its own rejections back into its
	// window and keeps itself open forever.
	require.False(t, resilience.CountsAsFailure(resilience.ErrBreakerOpen))
	require.False(t, resilience.IsRetryable(resilience.ErrBreakerOpen),
		"retrying an open breaker burns the retry budget for nothing")
}

func TestStateChangesAreReported(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	clock := newFakeClock()

	var changes []StateChange
	var mu sync.Mutex
	b, err := New("svc|/pkg.S/M", cfg, clock, func(c StateChange) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, c)
	})
	require.NoError(t, err)

	call(t, b, serverFault())
	call(t, b, serverFault())
	clock.Advance(cfg.OpenDuration)
	for i := 0; i < cfg.HalfOpenSuccesses; i++ {
		call(t, b, nil)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, changes, 3)
	require.Equal(t, StateClosed, changes[0].From)
	require.Equal(t, StateOpen, changes[0].To)
	require.Equal(t, "svc|/pkg.S/M", changes[0].Key)
	require.Equal(t, StateHalfOpen, changes[1].To)
	require.Equal(t, StateClosed, changes[2].To)
	// The report should say why, not just that.
	require.Positive(t, changes[0].Failures)
}

func TestGroupKeysPerMethod(t *testing.T) {
	// One slow method must not black-hole every other method on the target.
	cfg := DefaultConfig()
	cfg.MinimumRequests = 2
	clock := newFakeClock()
	g, err := NewGroup(cfg, clock, nil)
	require.NoError(t, err)

	slow := g.Get(Key("users", "/user.v1.UserService/Search"))
	fast := g.Get(Key("users", "/user.v1.UserService/GetUser"))

	call(t, slow, serverFault())
	call(t, slow, serverFault())

	require.Equal(t, StateOpen, slow.State())
	require.Equal(t, StateClosed, fast.State(), "one method's failures tripped another's breaker")
}

func TestGroupReturnsTheSameBreakerForAKey(t *testing.T) {
	g, err := NewGroup(DefaultConfig(), newFakeClock(), nil)
	require.NoError(t, err)
	require.Same(t, g.Get("a|b"), g.Get("a|b"))
	require.NotSame(t, g.Get("a|b"), g.Get("a|c"))
}

func TestGroupIsSafeUnderConcurrency(t *testing.T) {
	g, err := NewGroup(DefaultConfig(), newFakeClock(), nil)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b := g.Get(Key("target", "/pkg.S/M"))
				if done, err := b.Allow(); err == nil {
					done(nil)
				}
			}
		}()
	}
	wg.Wait()

	total, _ := g.Get(Key("target", "/pkg.S/M")).Counts()
	require.Equal(t, 5000, total)
}

func TestInvalidConfigIsRejected(t *testing.T) {
	// A breaker with a ratio of 0 would trip on the first evaluation; one
	// with a ratio above 1 would never trip. Both are silent in production
	// and obvious at construction.
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"zero minimum", func(c *Config) { c.MinimumRequests = 0 }},
		{"zero ratio", func(c *Config) { c.FailureRatio = 0 }},
		{"ratio above one", func(c *Config) { c.FailureRatio = 1.5 }},
		{"zero window", func(c *Config) { c.Window = 0 }},
		{"zero open duration", func(c *Config) { c.OpenDuration = 0 }},
		{"zero half-open calls", func(c *Config) { c.HalfOpenMaxCalls = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mut(&cfg)
			_, err := New("k", cfg, nil, nil)
			require.Error(t, err)
		})
	}
}
