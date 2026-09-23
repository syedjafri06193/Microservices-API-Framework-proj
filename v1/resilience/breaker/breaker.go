// Package breaker implements a sliding-window circuit breaker, keyed per
// (target, procedure).
//
// Per-method, not per-service: one slow method should not black-hole every
// other method on the same target. A service where GetUser is fine and
// SearchUsers is timing out should keep serving GetUser.
//
// Trips on a failure *ratio* over a minimum request volume, not an absolute
// count. An absolute threshold makes a method serving 5 rps behave
// completely differently from one serving 5000 rps, and the minimum-volume
// gate is what stops a single failure on a quiet method from tripping it.
package breaker

import (
	"fmt"
	"sync"
	"time"

	"github.com/syedjafri06193/microservices-api-framework/resilience"
)

// State is a breaker's position in the CLOSED → OPEN → HALF_OPEN cycle.
type State int

const (
	// StateClosed passes calls through and watches the failure ratio.
	StateClosed State = iota
	// StateOpen rejects calls without attempting them.
	StateOpen
	// StateHalfOpen admits a limited number of probes.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// Config holds one breaker's thresholds.
type Config struct {
	// MinimumRequests is the volume gate: the ratio is not evaluated until
	// the window holds at least this many requests. Without it, one failure
	// out of two requests on a quiet method trips the breaker.
	MinimumRequests int
	// FailureRatio is the fraction of server-fault responses that trips it.
	FailureRatio float64
	// Window is the sliding window over which the ratio is measured.
	Window time.Duration
	// OpenDuration is how long to stay open before admitting probes.
	OpenDuration time.Duration
	// HalfOpenMaxCalls caps concurrent probes while half-open. Letting the
	// full load through on the first probe is how a recovering dependency
	// gets knocked back over.
	HalfOpenMaxCalls int
	// HalfOpenSuccesses is the number of consecutive successful probes
	// needed to close.
	HalfOpenSuccesses int
	// Buckets divides the window. More buckets means a smoother slide and
	// slightly more memory per breaker.
	Buckets int
}

// DefaultConfig returns the framework's opinion about breaker thresholds.
func DefaultConfig() Config {
	return Config{
		MinimumRequests:   20,
		FailureRatio:      0.5,
		Window:            10 * time.Second,
		OpenDuration:      5 * time.Second,
		HalfOpenMaxCalls:  3,
		HalfOpenSuccesses: 3,
		Buckets:           10,
	}
}

// Validate reports configuration that cannot work, rather than letting it
// produce a breaker that silently never trips.
func (c Config) Validate() error {
	if c.MinimumRequests < 1 {
		return fmt.Errorf("MinimumRequests must be at least 1, got %d", c.MinimumRequests)
	}
	if c.FailureRatio <= 0 || c.FailureRatio > 1 {
		return fmt.Errorf("FailureRatio must be in (0, 1], got %v", c.FailureRatio)
	}
	if c.Window <= 0 {
		return fmt.Errorf("Window must be positive, got %v", c.Window)
	}
	if c.OpenDuration <= 0 {
		return fmt.Errorf("OpenDuration must be positive, got %v", c.OpenDuration)
	}
	if c.HalfOpenMaxCalls < 1 {
		return fmt.Errorf("HalfOpenMaxCalls must be at least 1, got %d", c.HalfOpenMaxCalls)
	}
	if c.HalfOpenSuccesses < 1 {
		return fmt.Errorf("HalfOpenSuccesses must be at least 1, got %d", c.HalfOpenSuccesses)
	}
	return nil
}

// StateChange is reported to an observer when a breaker transitions.
type StateChange struct {
	Key  string
	From State
	To   State
	// Failures and Total are the window contents at the moment of the
	// change, so a log line can say *why* rather than just *that*.
	Failures int
	Total    int
}

// Breaker is a single circuit breaker. Use Group to get one per method.
type Breaker struct {
	cfg      Config
	clock    resilience.Clock
	key      string
	onChange func(StateChange)

	mu            sync.Mutex
	state         State
	buckets       []bucket
	width         time.Duration
	openedAt      time.Time
	halfOpen      int
	consecutiveOK int
}

type bucket struct {
	start    time.Time
	total    int
	failures int
}

// New returns a breaker. A zero Clock means the system clock.
func New(key string, cfg Config, clock resilience.Clock, onChange func(StateChange)) (*Breaker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = resilience.SystemClock{}
	}
	if cfg.Buckets < 1 {
		cfg.Buckets = 1
	}
	return &Breaker{
		cfg:      cfg,
		clock:    clock,
		key:      key,
		onChange: onChange,
		state:    StateClosed,
		buckets:  make([]bucket, cfg.Buckets),
		width:    cfg.Window / time.Duration(cfg.Buckets),
	}, nil
}

// Allow asks permission to make a call.
//
// On success it returns a function that must be called exactly once with
// the call's result. Returning a callback rather than exposing Success and
// Failure methods is deliberate: the half-open probe count has to be
// decremented whatever the outcome, and a caller who forgets to report a
// result would leak a probe slot and wedge the breaker half-open forever.
func (b *Breaker) Allow() (done func(error), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()

	switch b.state {
	case StateOpen:
		if now.Sub(b.openedAt) < b.cfg.OpenDuration {
			return nil, resilience.ErrBreakerOpen
		}
		b.transition(StateHalfOpen, now)
		fallthrough

	case StateHalfOpen:
		if b.halfOpen >= b.cfg.HalfOpenMaxCalls {
			return nil, resilience.ErrBreakerOpen
		}
		b.halfOpen++
	}

	var once sync.Once
	return func(callErr error) {
		once.Do(func() { b.record(callErr) })
	}, nil
}

// State returns the current state, evaluating the open timeout so a caller
// polling for observability sees half-open as soon as it is due.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && b.clock.Now().Sub(b.openedAt) >= b.cfg.OpenDuration {
		return StateHalfOpen
	}
	return b.state
}

// Counts returns the window's totals, for metrics and tests.
func (b *Breaker) Counts() (total, failures int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sums(b.clock.Now())
}

func (b *Breaker) record(callErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	failed := resilience.CountsAsFailure(callErr)

	cur := b.currentBucket(now)
	cur.total++
	if failed {
		cur.failures++
	}

	switch b.state {
	case StateHalfOpen:
		b.halfOpen--
		if failed {
			// One failed probe is enough. A dependency that is still
			// broken should not have to fail the full ratio again — that
			// would mean sending it MinimumRequests calls it cannot serve.
			b.trip(now)
			return
		}
		b.consecutiveOK++
		if b.consecutiveOK >= b.cfg.HalfOpenSuccesses {
			b.transition(StateClosed, now)
			b.reset()
		}

	case StateClosed:
		total, failures := b.sums(now)
		if total >= b.cfg.MinimumRequests &&
			float64(failures)/float64(total) >= b.cfg.FailureRatio {
			b.trip(now)
		}
	}
}

func (b *Breaker) trip(now time.Time) {
	b.openedAt = now
	b.transition(StateOpen, now)
	b.halfOpen = 0
	b.consecutiveOK = 0
}

// transition changes state and notifies the observer outside the lock's
// critical decisions but still under it — the callback is documented as
// needing to be cheap and non-blocking, because a slow observer would hold
// up every call through this breaker.
func (b *Breaker) transition(to State, now time.Time) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	if to == StateHalfOpen {
		b.halfOpen = 0
		b.consecutiveOK = 0
	}
	if b.onChange != nil {
		total, failures := b.sums(now)
		b.onChange(StateChange{Key: b.key, From: from, To: to, Failures: failures, Total: total})
	}
}

func (b *Breaker) reset() {
	for i := range b.buckets {
		b.buckets[i] = bucket{}
	}
	b.consecutiveOK = 0
	b.halfOpen = 0
}

func (b *Breaker) currentBucket(now time.Time) *bucket {
	idx := int(now.UnixNano()/int64(b.width)) % len(b.buckets)
	if idx < 0 {
		idx += len(b.buckets)
	}
	bk := &b.buckets[idx]
	start := now.Truncate(b.width)
	if !bk.start.Equal(start) {
		*bk = bucket{start: start}
	}
	return bk
}

func (b *Breaker) sums(now time.Time) (total, failures int) {
	cutoff := now.Add(-b.cfg.Window)
	for i := range b.buckets {
		if b.buckets[i].start.After(cutoff) {
			total += b.buckets[i].total
			failures += b.buckets[i].failures
		}
	}
	return total, failures
}

// Group holds one breaker per key, created on demand.
//
// Keys are "target|procedure", and procedures come from Connect specs,
// which are static strings generated from the proto. There is no path by
// which a user-supplied value becomes a key, so the map cannot be made to
// grow without bound by traffic.
type Group struct {
	cfg      Config
	clock    resilience.Clock
	onChange func(StateChange)

	mu       sync.RWMutex
	breakers map[string]*Breaker
}

// NewGroup returns a group of breakers sharing one config.
func NewGroup(cfg Config, clock resilience.Clock, onChange func(StateChange)) (*Group, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = resilience.SystemClock{}
	}
	return &Group{
		cfg:      cfg,
		clock:    clock,
		onChange: onChange,
		breakers: make(map[string]*Breaker),
	}, nil
}

// Get returns the breaker for a key, creating it if necessary.
func (g *Group) Get(key string) *Breaker {
	g.mu.RLock()
	b, ok := g.breakers[key]
	g.mu.RUnlock()
	if ok {
		return b
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	// Re-check: another goroutine may have created it between the RUnlock
	// and the Lock.
	if b, ok := g.breakers[key]; ok {
		return b
	}
	// The config was validated in NewGroup, so this cannot fail.
	b, _ = New(key, g.cfg, g.clock, g.onChange)
	g.breakers[key] = b
	return b
}

// Key builds the breaker key for a target and procedure.
func Key(target, procedure string) string { return target + "|" + procedure }

// Snapshot returns every breaker's state, for the admin endpoint.
func (g *Group) Snapshot() map[string]State {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]State, len(g.breakers))
	for k, b := range g.breakers {
		out[k] = b.State()
	}
	return out
}
