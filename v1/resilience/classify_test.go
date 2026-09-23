package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
)

func TestServerFaultsCount(t *testing.T) {
	for _, code := range []connect.Code{
		connect.CodeUnavailable,
		connect.CodeDeadlineExceeded,
		connect.CodeResourceExhausted,
		connect.CodeInternal,
		connect.CodeDataLoss,
		connect.CodeUnknown,
	} {
		require.True(t, CountsAsFailure(connect.NewError(code, errors.New("x"))),
			"%v should count as a server fault", code)
	}
}

func TestClientFaultsDoNotCount(t *testing.T) {
	// The property that stops one buggy caller taking down a healthy
	// service for everyone else.
	for _, code := range []connect.Code{
		connect.CodeInvalidArgument,
		connect.CodeNotFound,
		connect.CodeAlreadyExists,
		connect.CodePermissionDenied,
		connect.CodeUnauthenticated,
		connect.CodeFailedPrecondition,
		connect.CodeOutOfRange,
		connect.CodeCanceled,
		connect.CodeUnimplemented,
	} {
		require.False(t, CountsAsFailure(connect.NewError(code, errors.New("x"))),
			"%v must not count as a server fault", code)
	}
}

func TestNilAndBreakerOpenNeverCount(t *testing.T) {
	require.False(t, CountsAsFailure(nil))
	// Otherwise an open breaker feeds its own rejections back into its
	// window and keeps itself open indefinitely.
	require.False(t, CountsAsFailure(ErrBreakerOpen))
	require.False(t, CountsAsFailure(connect.NewError(connect.CodeUnavailable, ErrBreakerOpen)))
}

func TestUnclassifiedErrorsFailSafe(t *testing.T) {
	// A plain error carries CodeUnknown. An error we cannot classify is
	// more likely a broken dependency than a broken caller, so the breaker
	// should protect rather than ignore.
	require.True(t, CountsAsFailure(errors.New("something went wrong")))
}

func TestRetryableIsNarrowerThanFailure(t *testing.T) {
	// Internal is a server fault and should open a breaker, but repeating
	// the same request hits the same bug while adding load.
	internal := connect.NewError(connect.CodeInternal, errors.New("bug"))
	require.True(t, CountsAsFailure(internal))
	require.False(t, IsRetryable(internal))

	unavailable := connect.NewError(connect.CodeUnavailable, errors.New("down"))
	require.True(t, CountsAsFailure(unavailable))
	require.True(t, IsRetryable(unavailable))
}

func TestCancelledCallerIsNotRetried(t *testing.T) {
	require.False(t, IsRetryable(context.Canceled))
	require.False(t, IsRetryable(nil))
}

type stubClock struct{ t time.Time }

func (c stubClock) Now() time.Time { return c.t }

func TestSlidingCounterExpiresOldBuckets(t *testing.T) {
	clock := &stubClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := NewSlidingCounter(clock, 10*time.Second, 10)

	c.Add(1)
	c.Add(1)
	require.Equal(t, float64(2), c.Sum())

	clock.t = clock.t.Add(11 * time.Second)
	require.Zero(t, c.Sum(), "expired buckets were still counted")

	c.Add(5)
	require.Equal(t, float64(5), c.Sum())
}

func TestSlidingCounterSlidesRatherThanResetting(t *testing.T) {
	clock := &stubClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := NewSlidingCounter(clock, 10*time.Second, 10)

	for i := 0; i < 10; i++ {
		c.Add(1)
		clock.t = clock.t.Add(time.Second)
	}
	// The oldest entry has just aged out; the rest are still in.
	sum := c.Sum()
	require.Greater(t, sum, float64(5))
	require.LessOrEqual(t, sum, float64(10))
}
