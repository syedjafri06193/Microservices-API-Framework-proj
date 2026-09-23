// Package retry retries only what the protobuf declares safe.
//
// Retrying a non-idempotent method is how you get duplicate charges. Rather
// than trusting a config file or a comment, the decision is read from the
// proto descriptor, which already has the field:
//
//	rpc GetPayment(...) returns (...) { option idempotency_level = NO_SIDE_EFFECTS; }
//	rpc CancelPayment(...) returns (...) { option idempotency_level = IDEMPOTENT; }
//	rpc ChargeCard(...) returns (...);   // unannotated: never retried
//
// It fails closed. An unannotated method is never retried, which makes the
// safe thing the default and turns enabling retries into an explicit,
// reviewable change to the API's contract — exactly the property you want
// on a payments service.
package retry

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/syedjafri06193/microservices-api-framework/resilience"
	"github.com/syedjafri06193/microservices-api-framework/resilience/budget"
	"github.com/syedjafri06193/microservices-api-framework/resilience/deadline"
)

// HeaderPreviousAttempts marks a request as a retry, using gRPC's name so
// that gRPC servers and meshes already understand it.
//
// Setting it is the single cheapest defence against multi-hop
// amplification: a downstream service that can see a request is already a
// retry can decline to retry it again itself, which is what stops three
// layers of threefold retries from becoming 27x load.
const HeaderPreviousAttempts = "grpc-previous-rpc-attempts"

// Config describes retry behaviour.
type Config struct {
	// MaxAttempts is the number of *additional* attempts after the first.
	// 2 means up to three calls in total.
	MaxAttempts int
	// Base is the first backoff interval; it doubles each attempt.
	Base time.Duration
	// MaxBackoff caps the interval.
	MaxBackoff time.Duration
}

// DefaultConfig returns the framework's opinion.
func DefaultConfig() Config {
	return Config{MaxAttempts: 2, Base: 50 * time.Millisecond, MaxBackoff: 2 * time.Second}
}

// Validate rejects a configuration that cannot behave.
func (c Config) Validate() error {
	if c.MaxAttempts < 0 {
		return fmt.Errorf("MaxAttempts must not be negative, got %d", c.MaxAttempts)
	}
	if c.MaxAttempts > 0 && c.Base <= 0 {
		return fmt.Errorf("Base must be positive when retries are enabled, got %v", c.Base)
	}
	if c.MaxBackoff > 0 && c.MaxBackoff < c.Base {
		return fmt.Errorf("MaxBackoff (%v) is below Base (%v)", c.MaxBackoff, c.Base)
	}
	return nil
}

// Policy decides, per procedure, whether retries are permitted.
//
// The lookup happens once per procedure and is cached: resolving a
// descriptor from the global registry on every call would put reflection in
// the hot path of every RPC the service makes.
type Policy struct {
	mu    sync.RWMutex
	cache map[string]bool
	// resolve is swappable for tests; in production it reads the global
	// protobuf registry that generated code registers itself into.
	resolve func(procedure string) (protoreflect.MethodDescriptor, error)
}

// NewPolicy returns a Policy backed by the global protobuf registry.
func NewPolicy() *Policy {
	return &Policy{cache: make(map[string]bool), resolve: resolveFromRegistry}
}

// NewPolicyWithResolver returns a Policy with a custom descriptor lookup.
func NewPolicyWithResolver(resolve func(string) (protoreflect.MethodDescriptor, error)) *Policy {
	return &Policy{cache: make(map[string]bool), resolve: resolve}
}

// Retryable reports whether a procedure ("/pkg.Service/Method") may be
// retried automatically.
func (p *Policy) Retryable(procedure string) bool {
	p.mu.RLock()
	v, ok := p.cache[procedure]
	p.mu.RUnlock()
	if ok {
		return v
	}

	md, err := p.resolve(procedure)
	// Fail closed. A procedure whose descriptor cannot be found is not
	// known to be safe, and "not known to be safe" must mean "not retried"
	// — the alternative is that a registry mishap silently enables retries
	// on a method that charges a credit card.
	allowed := err == nil && md != nil && idempotent(md)

	p.mu.Lock()
	p.cache[procedure] = allowed
	p.mu.Unlock()
	return allowed
}

func idempotent(md protoreflect.MethodDescriptor) bool {
	opts, ok := md.Options().(*descriptorpb.MethodOptions)
	if !ok || opts == nil || opts.IdempotencyLevel == nil {
		return false
	}
	switch opts.GetIdempotencyLevel() {
	case descriptorpb.MethodOptions_NO_SIDE_EFFECTS,
		descriptorpb.MethodOptions_IDEMPOTENT:
		return true
	}
	return false
}

func resolveFromRegistry(procedure string) (protoreflect.MethodDescriptor, error) {
	// Connect procedures are "/pkg.Service/Method".
	rest := procedure
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	slash := -1
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 {
		return nil, fmt.Errorf("malformed procedure %q", procedure)
	}
	serviceName, methodName := rest[:slash], rest[slash+1:]

	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(serviceName))
	if err != nil {
		return nil, err
	}
	sd, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a service", serviceName)
	}
	md := sd.Methods().ByName(protoreflect.Name(methodName))
	if md == nil {
		return nil, fmt.Errorf("method %q not found on %q", methodName, serviceName)
	}
	return md, nil
}

// Observer receives retry events, for metrics and logs.
type Observer interface {
	RetryAttempt(ctx context.Context, procedure string, attempt int)
	RetryBudgetExhausted(ctx context.Context, procedure string)
	RetrySkippedNotIdempotent(ctx context.Context, procedure string)
}

// NopObserver ignores everything.
type NopObserver struct{}

func (NopObserver) RetryAttempt(context.Context, string, int)         {}
func (NopObserver) RetryBudgetExhausted(context.Context, string)      {}
func (NopObserver) RetrySkippedNotIdempotent(context.Context, string) {}

// Retrier runs an operation with retries, a budget and a deadline budget.
type Retrier struct {
	cfg      Config
	policy   *Policy
	budget   *budget.Budget
	deadline deadline.Budget
	observer Observer
	rand     *rand.Rand
	randMu   sync.Mutex
}

// New returns a Retrier. A nil budget means retries are unbudgeted, which
// is only appropriate in tests.
func New(cfg Config, policy *Policy, b *budget.Budget, d deadline.Budget, obs Observer) (*Retrier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if policy == nil {
		policy = NewPolicy()
	}
	if obs == nil {
		obs = NopObserver{}
	}
	return &Retrier{
		cfg:      cfg,
		policy:   policy,
		budget:   b,
		deadline: d,
		observer: obs,
		// Seeded independently of the global source so that two processes
		// starting together do not draw the same jitter sequence and
		// resynchronise the herd the jitter exists to break up.
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// Do runs fn, retrying when the procedure is declared safe and the error is
// transient.
//
// fn receives a context whose deadline has been narrowed to this attempt's
// share of the remaining budget, and a header map to attach to the request.
func (r *Retrier) Do(ctx context.Context, procedure string,
	fn func(ctx context.Context, attempt int) error) error {

	if !r.policy.Retryable(procedure) {
		r.observer.RetrySkippedNotIdempotent(ctx, procedure)
		// Exactly one attempt, ever. Still subject to the deadline budget,
		// because refusing to start doomed work is worth doing whether or
		// not the call can be retried.
		attemptCtx, cancel, err := r.deadline.Derive(ctx, procedure)
		if err != nil {
			return err
		}
		defer cancel()
		return fn(attemptCtx, 0)
	}

	if r.budget != nil {
		r.budget.RecordRequest()
	}

	var lastErr error
	for attempt := 0; attempt <= r.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			if r.budget != nil && !r.budget.Allow() {
				r.observer.RetryBudgetExhausted(ctx, procedure)
				return lastErr
			}
			if err := r.sleep(ctx, attempt); err != nil {
				return err
			}
			r.observer.RetryAttempt(ctx, procedure, attempt)
		}

		attemptCtx, cancel, err := r.deadline.Derive(ctx, procedure)
		if err != nil {
			// No budget left. Returning the earlier error, when there is
			// one, is more useful than the budget error: it says why the
			// call was failing, not merely that time ran out.
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		lastErr = fn(attemptCtx, attempt)
		cancel()

		if lastErr == nil || !resilience.IsRetryable(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// sleep waits out this attempt's backoff, or returns early if the caller
// gives up.
func (r *Retrier) sleep(ctx context.Context, attempt int) error {
	backoff := r.cfg.Base * time.Duration(1<<uint(attempt-1))
	if r.cfg.MaxBackoff > 0 && backoff > r.cfg.MaxBackoff {
		backoff = r.cfg.MaxBackoff
	}
	if backoff <= 0 {
		return nil
	}

	// Full jitter: sleep uniformly in [0, backoff), rather than the more
	// common "half the backoff plus a random half". Full jitter is strictly
	// better at desynchronising clients, which is the entire reason backoff
	// exists.
	r.randMu.Lock()
	wait := time.Duration(r.rand.Int63n(int64(backoff)))
	r.randMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}
}

// AttemptHeader returns the value for HeaderPreviousAttempts, or "" for the
// first attempt (where gRPC omits the header entirely).
func AttemptHeader(attempt int) string {
	if attempt <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", attempt)
}
