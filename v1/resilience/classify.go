// Package resilience holds the error classification that the circuit
// breaker and the retrier both depend on, plus the shared sliding-window
// counter they are built from.
//
// Classification lives here rather than inside either of them because the
// two must agree. A breaker that counts an error as a server fault while
// the retrier treats it as non-retryable is a system that opens a breaker
// on errors it never retried, and the disagreement is invisible until an
// incident.
package resilience

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
)

// ErrBreakerOpen is returned when a call is rejected because the breaker
// for its target is open. It is defined here, not in the breaker package,
// because the retrier has to recognise it (see IsRetryable) and a
// dependency from resilience/retry to resilience/breaker purely to import
// a sentinel would make the two packages inseparable.
var ErrBreakerOpen = errors.New("circuit breaker is open")

// CountsAsFailure reports whether an error is evidence that the *callee* is
// unhealthy, which is the only thing a circuit breaker should react to.
//
// This is the most consequential decision in the resilience layer, and
// getting it wrong is actively harmful rather than merely useless. A
// breaker that counts every error will trip on InvalidArgument — so one
// caller shipping a bug takes a perfectly healthy service offline for
// everybody else, and the service's own dashboards show it failing.
//
// The rule: count server faults, ignore client faults.
func CountsAsFailure(err error) bool {
	if err == nil {
		return false
	}
	// A breaker-open error is the breaker's own output. Feeding it back in
	// would be self-reinforcing: an open breaker would keep itself open
	// forever on the strength of its own rejections.
	if errors.Is(err, ErrBreakerOpen) {
		return false
	}

	switch connect.CodeOf(err) {
	// Server faults. The dependency is unhealthy.
	case connect.CodeUnavailable, // cannot reach it
		connect.CodeDeadlineExceeded,  // too slow
		connect.CodeResourceExhausted, // overloaded
		connect.CodeInternal,          // it broke
		connect.CodeDataLoss,
		connect.CodeUnknown:
		return true

	// Client faults. The dependency is fine; the caller is wrong. Counting
	// these is how one buggy client takes down a healthy service.
	case connect.CodeInvalidArgument,
		connect.CodeNotFound,
		connect.CodeAlreadyExists,
		connect.CodePermissionDenied,
		connect.CodeUnauthenticated,
		connect.CodeFailedPrecondition,
		connect.CodeOutOfRange,
		connect.CodeAborted,
		connect.CodeUnimplemented:
		return false

	// The caller gave up. This says nothing about the server's health — a
	// user closing a browser tab is not evidence that a dependency is
	// sick — and counting it means a spike in abandoned requests trips
	// every breaker in the call path. This one catches people out.
	case connect.CodeCanceled:
		return false
	}

	// An unrecognised code is treated as a server fault: an error we cannot
	// classify is more likely to be a broken dependency than a broken
	// caller, and failing safe here means the breaker protects rather than
	// ignores.
	return true
}

// IsRetryable reports whether an error is worth another attempt.
//
// Narrower than CountsAsFailure on purpose. Internal is a server fault and
// should open a breaker, but it usually means the callee hit a bug, and
// repeating the same request will hit the same bug while adding load.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Retrying into an open breaker is pure waste: the attempt fails
	// instantly, and the retry budget is spent on a call that never left
	// the process. The breaker only closes on the passage of time, which a
	// retry loop cannot hurry.
	//
	// This is exactly why the breaker sits *inside* the retry loop and why
	// its error must be non-retryable — together they make the breaker the
	// cap on retry amplification rather than a bystander to it.
	if errors.Is(err, ErrBreakerOpen) {
		return false
	}

	// A cancelled or expired context means the caller is no longer waiting.
	// Another attempt would be work nobody reads.
	if errors.Is(err, context.Canceled) {
		return false
	}

	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeResourceExhausted:
		return true
	case connect.CodeDeadlineExceeded:
		// Retryable only in the sense that the deadline budget gets the
		// final say: it refuses to start an attempt that cannot finish, so
		// a genuinely exhausted deadline stops the loop there rather than
		// here.
		return true
	default:
		return false
	}
}

// Clock is the time source every time-dependent component takes.
//
// Injecting it matters more than it looks. Without it, a breaker test needs
// real sleeps: the suite takes minutes, and it is flaky on a loaded CI
// machine, which is the machine it runs on. Go 1.25's testing/synctest
// removes the need for this, but the framework targets Go 1.24, where it is
// still experimental.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// SlidingCounter counts events over a rolling window, in buckets.
//
// Buckets rather than timestamps: a service at 50k rps would hold 500k
// timestamps for a 10-second window, and the allocation alone would show up
// in a profile. Bucketing costs a little precision at the window edge and
// makes the memory constant.
type SlidingCounter struct {
	mu      sync.Mutex
	clock   Clock
	window  time.Duration
	buckets []counterBucket
	width   time.Duration
}

type counterBucket struct {
	start time.Time
	value float64
}

// NewSlidingCounter returns a counter over window, divided into n buckets.
func NewSlidingCounter(clock Clock, window time.Duration, n int) *SlidingCounter {
	if n < 1 {
		n = 1
	}
	return &SlidingCounter{
		clock:   clock,
		window:  window,
		buckets: make([]counterBucket, n),
		width:   window / time.Duration(n),
	}
}

// Add records v at the current time.
func (c *SlidingCounter) Add(v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bucketLocked(c.clock.Now())
	b.value += v
}

// Sum returns the total over the window, discarding expired buckets.
func (c *SlidingCounter) Sum() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock.Now()
	cutoff := now.Add(-c.window)
	var total float64
	for i := range c.buckets {
		if c.buckets[i].start.After(cutoff) {
			total += c.buckets[i].value
		}
	}
	return total
}

// Reset clears every bucket.
func (c *SlidingCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.buckets {
		c.buckets[i] = counterBucket{}
	}
}

// bucketLocked returns the bucket for now, recycling it if it has expired.
func (c *SlidingCounter) bucketLocked(now time.Time) *counterBucket {
	// Index by absolute time, so a gap in traffic recycles the right
	// buckets rather than leaving stale counts wherever the ring happened
	// to stop.
	idx := int(now.UnixNano()/int64(c.width)) % len(c.buckets)
	if idx < 0 {
		idx += len(c.buckets)
	}
	b := &c.buckets[idx]
	start := now.Truncate(c.width)
	if !b.start.Equal(start) {
		b.start = start
		b.value = 0
	}
	return b
}
