// Package budget caps retries as a fraction of total request volume.
//
// Exponential backoff with jitter bounds *timing*, not *volume*. During a
// partial outage every client retrying three times means the struggling
// dependency receives three times the traffic precisely when it can least
// handle it; backoff only spreads that slightly. With a 20% budget a total
// outage produces at most 1.2x load instead of 3x, and that difference is
// often the difference between a degraded dependency and a dead one.
//
// The design follows gRPC's retry throttling and Linkerd's retry budgets.
package budget

import (
	"fmt"
	"sync"
	"time"

	"github.com/syedjafri06193/microservices-api-framework/resilience"
)

// Config describes a retry budget.
type Config struct {
	// Ratio is the fraction of total requests that may be retries. 0.2
	// means retries may add at most 20% on top of normal traffic.
	Ratio float64
	// MinPerSecond is a floor, so a low-traffic service can still retry at
	// all. Without it a service handling 2 rps has a budget of 0.4 retries
	// per second and effectively never retries — which is the case where a
	// retry is cheapest and most likely to help.
	MinPerSecond float64
	// Window is how far back the counters look.
	Window time.Duration
	// Buckets divides the window.
	Buckets int
}

// DefaultConfig returns the framework's opinion.
func DefaultConfig() Config {
	return Config{
		Ratio:        0.2,
		MinPerSecond: 1,
		Window:       10 * time.Second,
		Buckets:      10,
	}
}

// Validate rejects a budget that cannot behave.
func (c Config) Validate() error {
	if c.Ratio < 0 || c.Ratio > 1 {
		return fmt.Errorf("Ratio must be in [0, 1], got %v", c.Ratio)
	}
	if c.MinPerSecond < 0 {
		return fmt.Errorf("MinPerSecond must not be negative, got %v", c.MinPerSecond)
	}
	if c.Window <= 0 {
		return fmt.Errorf("Window must be positive, got %v", c.Window)
	}
	return nil
}

// Budget tracks requests and retries over a sliding window.
type Budget struct {
	cfg Config

	mu       sync.Mutex
	requests *resilience.SlidingCounter
	retries  *resilience.SlidingCounter

	// Exhausted counts refusals, so "the budget is holding retries back" is
	// visible on a dashboard rather than inferred from the absence of
	// retries.
	exhausted uint64
}

// New returns a budget.
func New(cfg Config, clock resilience.Clock) (*Budget, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = resilience.SystemClock{}
	}
	if cfg.Buckets < 1 {
		cfg.Buckets = 1
	}
	return &Budget{
		cfg:      cfg,
		requests: resilience.NewSlidingCounter(clock, cfg.Window, cfg.Buckets),
		retries:  resilience.NewSlidingCounter(clock, cfg.Window, cfg.Buckets),
	}, nil
}

// RecordRequest counts one logical call.
//
// Called once per logical call, not once per attempt. Counting attempts
// would let retries inflate the denominator they are measured against,
// so a retry storm would keep granting itself more budget — the exact
// failure the budget exists to prevent.
func (b *Budget) RecordRequest() { b.requests.Add(1) }

// Allow reports whether one more retry fits in the budget, and charges it
// if so.
func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	total := b.requests.Sum()
	used := b.retries.Sum()

	allowed := total*b.cfg.Ratio + b.cfg.MinPerSecond*b.cfg.Window.Seconds()
	if used >= allowed {
		b.exhausted++
		return false
	}
	b.retries.Add(1)
	return true
}

// Stats returns the window's contents, for metrics and the admin endpoint.
func (b *Budget) Stats() (requests, retries float64, exhausted uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests.Sum(), b.retries.Sum(), b.exhausted
}
