package deadline

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
)

func TestDeriveLeavesABufferForThisHop(t *testing.T) {
	// The arithmetic nobody does: a service that passes its full remaining
	// deadline downstream leaves itself no time to handle the response, so
	// the work completes and the answer arrives too late to use.
	b := Budget{Buffer: 50 * time.Millisecond, Minimum: 10 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	child, cancelChild, err := b.Derive(ctx, "/pkg.S/M")
	require.NoError(t, err)
	defer cancelChild()

	outer, _ := ctx.Deadline()
	inner, ok := child.Deadline()
	require.True(t, ok)

	gap := outer.Sub(inner)
	require.InDelta(t, float64(50*time.Millisecond), float64(gap), float64(10*time.Millisecond))
}

func TestDeriveFailsFastWhenTooLittleRemains(t *testing.T) {
	// The valuable branch. Under load, a system that declines to start
	// doomed work recovers; one that starts it anyway spends its capacity
	// on requests nobody is waiting for.
	b := Budget{Buffer: 50 * time.Millisecond, Minimum: 100 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	child, cancelChild, err := b.Derive(ctx, "/pkg.S/M")
	require.Error(t, err)
	require.Nil(t, child)
	require.Nil(t, cancelChild)

	// Classified as a deadline problem, so callers and breakers treat it
	// exactly as they would an actual timeout — which is what it is, just
	// detected before the work rather than after it.
	require.Equal(t, connect.CodeDeadlineExceeded, connect.CodeOf(err))

	var insufficient *ErrInsufficientBudget
	require.True(t, errors.As(err, &insufficient))
	require.Equal(t, 100*time.Millisecond, insufficient.Minimum)
}

func TestContextWithoutDeadlineIsReportedNotInvented(t *testing.T) {
	var reported []string
	b := Budget{
		Buffer:    50 * time.Millisecond,
		Minimum:   10 * time.Millisecond,
		OnMissing: func(p string) { reported = append(reported, p) },
	}

	child, cancel, err := b.Derive(context.Background(), "/pkg.S/M")
	require.NoError(t, err)
	defer cancel()

	_, ok := child.Deadline()
	require.False(t, ok, "a deadline was invented for an unbounded call")
	require.Equal(t, []string{"/pkg.S/M"}, reported)
}

func TestRemainingReportsWhetherADeadlineExists(t *testing.T) {
	_, ok := Remaining(context.Background())
	require.False(t, ok)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, ok := Remaining(ctx)
	require.True(t, ok)
	require.Positive(t, d)
}

func TestBudgetShrinksAcrossHops(t *testing.T) {
	// Three hops, each reserving its buffer: the deadline must strictly
	// decrease, so the last hop cannot outlive the original caller.
	b := DefaultBudget()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	prev, _ := ctx.Deadline()
	for hop := 0; hop < 3; hop++ {
		next, cancelNext, err := b.Derive(ctx, "/pkg.S/M")
		require.NoError(t, err, "hop %d", hop)
		dl, ok := next.Deadline()
		require.True(t, ok)
		require.True(t, dl.Before(prev), "hop %d did not shrink the deadline", hop)
		prev = dl
		ctx = next
		defer cancelNext()
	}
}
