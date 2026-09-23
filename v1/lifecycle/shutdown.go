package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Step is one stage of the shutdown sequence.
type Step struct {
	Name string
	// Run is given a context whose deadline covers the whole sequence.
	Run func(context.Context) error
	// ContinueOnError keeps the sequence going when this step fails. Most
	// steps set it: a failure to close a connection pool should not prevent
	// telemetry from flushing, and the flush is often what explains the
	// failure.
	ContinueOnError bool
}

// Sequencer runs shutdown steps in order.
//
// Exported and usable on its own, with nothing else from this framework —
// the escape-hatch rule. A team that wants only correct shutdown ordering
// can take this type and leave the rest.
//
// The order is fixed because it is the part people get wrong. Two facts
// drive it:
//
// **SIGTERM arrives before traffic stops.** Kubernetes sends SIGTERM and
// removes the pod's endpoint concurrently, and endpoint removal propagates
// through kube-proxy, every ingress and every sidecar asynchronously, over
// seconds. A process that exits immediately on SIGTERM drops in-flight
// requests *and* receives new ones after it has decided to die.
//
// **Telemetry must flush after the server stops, not before.** Flushing
// first loses every span from the requests still draining — which are
// exactly the spans that explain a shutdown-time incident.
type Sequencer struct {
	steps  []Step
	logger *slog.Logger
}

// NewSequencer returns a sequencer.
func NewSequencer(logger *slog.Logger, steps ...Step) *Sequencer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sequencer{steps: steps, logger: logger}
}

// Append adds a step to the end.
func (s *Sequencer) Append(step Step) { s.steps = append(s.steps, step) }

// Run executes every step in order, returning the joined errors.
//
// The context is detached from cancellation before use. If shutdown was
// triggered by a cancelled context — which is the common case, since that
// is usually how the signal handler tells the service to stop — then using
// it directly would cancel every step immediately, and the telemetry flush
// would export nothing. Losing the spans that explain why you shut down is
// a particularly unhelpful way to lose spans.
func (s *Sequencer) Run(ctx context.Context, total time.Duration) error {
	base := context.WithoutCancel(ctx)
	if total > 0 {
		var cancel context.CancelFunc
		base, cancel = context.WithTimeout(base, total)
		defer cancel()
	}

	var errs []error
	for _, step := range s.steps {
		start := time.Now()
		s.logger.InfoContext(base, "shutdown step starting", slog.String("step", step.Name))

		err := step.Run(base)
		elapsed := time.Since(start)

		if err != nil {
			s.logger.ErrorContext(base, "shutdown step failed",
				slog.String("step", step.Name),
				slog.Duration("duration", elapsed),
				slog.String("error", err.Error()))
			errs = append(errs, fmt.Errorf("%s: %w", step.Name, err))
			if !step.ContinueOnError {
				return errors.Join(errs...)
			}
			continue
		}

		s.logger.InfoContext(base, "shutdown step complete",
			slog.String("step", step.Name),
			slog.Duration("duration", elapsed))
	}
	return errors.Join(errs...)
}

// Timing holds the durations that have to add up.
type Timing struct {
	// PreStopDelay is how long to keep serving after readiness is turned
	// off, so that endpoint removal can propagate.
	//
	// This sleep is not a hack and it is not optional. It is the only way
	// to bridge eventually-consistent endpoint removal: there is no signal
	// that says "every load balancer has stopped sending you traffic", so
	// you wait for as long as your platform takes.
	PreStopDelay time.Duration
	// DrainTimeout is how long to let in-flight requests finish.
	DrainTimeout time.Duration
	// GracePeriod is the platform's terminationGracePeriodSeconds, when it
	// is known. Zero means unknown.
	GracePeriod time.Duration
}

// Validate checks that the timings fit inside the platform's grace period.
//
// The relationship that must hold is:
//
//	PreStopDelay + DrainTimeout < GracePeriod
//
// If it does not, the platform sends SIGKILL in the middle of the drain,
// and all the careful sequencing above is wasted: connections are cut,
// in-flight requests fail, and telemetry never flushes. The arithmetic is
// easy to get wrong because the two halves live in different files — the
// framework's config and a Kubernetes manifest — so it is checked at
// startup where both are visible.
func (t Timing) Validate() error {
	if t.PreStopDelay < 0 {
		return fmt.Errorf("PreStopDelay must not be negative, got %v", t.PreStopDelay)
	}
	if t.DrainTimeout <= 0 {
		return fmt.Errorf("DrainTimeout must be positive, got %v", t.DrainTimeout)
	}
	if t.GracePeriod > 0 {
		needed := t.PreStopDelay + t.DrainTimeout
		if needed >= t.GracePeriod {
			return fmt.Errorf(
				"PreStopDelay (%v) + DrainTimeout (%v) = %v, which does not fit in the "+
					"platform's grace period of %v; the process will be killed mid-drain",
				t.PreStopDelay, t.DrainTimeout, needed, t.GracePeriod)
		}
	}
	return nil
}

// Total returns the time the whole sequence may take.
func (t Timing) Total() time.Duration { return t.PreStopDelay + t.DrainTimeout }
