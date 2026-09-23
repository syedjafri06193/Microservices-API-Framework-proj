// Package shed rejects requests when the service is overloaded.
//
// Rejecting keeps latency bounded for the requests that are accepted.
// Queueing them instead produces the classic death spiral: the queue grows,
// latency grows, clients time out and retry, and the queue grows faster.
//
// The signal is queue latency, not CPU. Time spent waiting to be handled is
// a direct measure of whether the service is keeping up, it needs no
// platform-specific instrumentation, and it stays correct when the
// bottleneck is a lock or a connection pool rather than the processor.
package shed

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Config describes a shedder.
type Config struct {
	// MaxQueueLatency is the wait above which requests are rejected.
	MaxQueueLatency time.Duration
	// MaxInFlight caps concurrency. Zero means unlimited, and relies on
	// queue latency alone.
	MaxInFlight int
}

// DefaultConfig returns the framework's opinion.
func DefaultConfig() Config {
	return Config{MaxQueueLatency: 100 * time.Millisecond, MaxInFlight: 0}
}

// Validate rejects nonsense.
func (c Config) Validate() error {
	if c.MaxQueueLatency < 0 {
		return fmt.Errorf("MaxQueueLatency must not be negative, got %v", c.MaxQueueLatency)
	}
	if c.MaxInFlight < 0 {
		return fmt.Errorf("MaxInFlight must not be negative, got %d", c.MaxInFlight)
	}
	return nil
}

// Shedder decides whether to admit a request.
type Shedder struct {
	cfg      Config
	inFlight atomic.Int64
	shed     atomic.Uint64
	admitted atomic.Uint64
}

// New returns a Shedder.
func New(cfg Config) (*Shedder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Shedder{cfg: cfg}, nil
}

// Admit reports whether a request that waited queuedFor should be handled.
// When it returns true, the returned function must be called when the
// request finishes.
func (s *Shedder) Admit(queuedFor time.Duration) (done func(), ok bool) {
	if s.cfg.MaxQueueLatency > 0 && queuedFor >= s.cfg.MaxQueueLatency {
		s.shed.Add(1)
		return nil, false
	}
	if s.cfg.MaxInFlight > 0 {
		// Increment first and roll back on rejection: checking then
		// incrementing lets two goroutines both observe capacity and both
		// take it, which is precisely the case a concurrency cap exists
		// to prevent.
		if s.inFlight.Add(1) > int64(s.cfg.MaxInFlight) {
			s.inFlight.Add(-1)
			s.shed.Add(1)
			return nil, false
		}
	}
	s.admitted.Add(1)
	return func() {
		if s.cfg.MaxInFlight > 0 {
			s.inFlight.Add(-1)
		}
	}, true
}

// Stats returns counters for metrics and the admin endpoint.
func (s *Shedder) Stats() (inFlight int64, admitted, shedded uint64) {
	return s.inFlight.Load(), s.admitted.Load(), s.shed.Load()
}
