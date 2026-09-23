// Package deadline derives a per-hop deadline from the remaining request
// budget, and refuses calls that cannot finish in time.
//
// The problem it solves: a client sets a 1s deadline. Service A spends
// 300ms, then calls B with a fresh 1s timeout. B spends 800ms and succeeds
// — but A's caller gave up 100ms ago, so all of B's work was wasted and B
// never knew.
//
// gRPC propagates deadlines automatically; plain HTTP does not, and almost
// nobody does the arithmetic. The fail-fast branch is the valuable part:
// under load, a system that declines to start doomed work recovers, and one
// that starts it anyway spends all its capacity on requests nobody is
// waiting for.
package deadline

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
)

// Budget computes per-attempt deadlines.
type Budget struct {
	// Buffer is time reserved for this hop's own processing and the
	// response trip, subtracted before the downstream deadline is set.
	Buffer time.Duration
	// Minimum is the point below which a call is not worth starting.
	Minimum time.Duration
	// OnMissing is called once per procedure when an inbound request
	// carries no deadline at all. That is a smell worth surfacing: an
	// unbounded call path is how a slow dependency turns into an
	// exhausted connection pool.
	OnMissing func(procedure string)
}

// DefaultBudget returns the framework's opinion.
func DefaultBudget() Budget {
	return Budget{Buffer: 50 * time.Millisecond, Minimum: 20 * time.Millisecond}
}

// ErrInsufficientBudget is returned when too little of the caller's
// deadline remains to be worth making a call. It carries
// CodeDeadlineExceeded so that callers and breakers classify it exactly as
// they would an actual timeout — which is what it is, just detected before
// the work rather than after it.
type ErrInsufficientBudget struct {
	Remaining time.Duration
	Minimum   time.Duration
}

func (e *ErrInsufficientBudget) Error() string {
	return fmt.Sprintf("insufficient deadline budget: %v remaining, need at least %v",
		e.Remaining, e.Minimum)
}

// Derive returns a context whose deadline leaves room for this hop.
//
// A context with no deadline is passed through unchanged rather than being
// given one: inventing a deadline here would silently cap call paths the
// operator never configured, and the resulting timeouts would be blamed on
// the dependency. The missing deadline is reported instead.
func (b Budget) Derive(ctx context.Context, procedure string) (context.Context, context.CancelFunc, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		if b.OnMissing != nil {
			b.OnMissing(procedure)
		}
		return ctx, func() {}, nil
	}

	remaining := time.Until(dl)
	budget := remaining - b.Buffer

	if budget < b.Minimum {
		err := &ErrInsufficientBudget{Remaining: remaining, Minimum: b.Minimum}
		return nil, nil, connect.NewError(connect.CodeDeadlineExceeded, err)
	}

	child, cancel := context.WithTimeout(ctx, budget)
	return child, cancel, nil
}

// Remaining reports how long is left on a context's deadline, and whether
// it had one.
func Remaining(ctx context.Context) (time.Duration, bool) {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(dl), true
}
