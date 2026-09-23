package retry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/syedjafri06193/microservices-api-framework/resilience"
	"github.com/syedjafri06193/microservices-api-framework/resilience/budget"
	"github.com/syedjafri06193/microservices-api-framework/resilience/deadline"
)

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

// buildMethod constructs a real MethodDescriptor with the given idempotency
// level. Using genuine protobuf descriptors rather than a stub interface is
// the point: the property under test is that the framework reads the
// annotation the proto author actually wrote.
func buildMethod(t *testing.T, level *descriptorpb.MethodOptions_IdempotencyLevel) protoreflect.MethodDescriptor {
	t.Helper()

	opts := &descriptorpb.MethodOptions{}
	if level != nil {
		opts.IdempotencyLevel = level
	}

	fd := &descriptorpb.FileDescriptorProto{
		Name:    strPtr("test/v1/test.proto"),
		Package: strPtr("test.v1"),
		Syntax:  strPtr("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: strPtr("Req")},
			{Name: strPtr("Resp")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: strPtr("TestService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       strPtr("Do"),
				InputType:  strPtr(".test.v1.Req"),
				OutputType: strPtr(".test.v1.Resp"),
				Options:    opts,
			}},
		}},
	}

	file, err := protodescFile(fd)
	require.NoError(t, err)
	return file.Services().Get(0).Methods().Get(0)
}

func strPtr(s string) *string { return &s }

func idem(l descriptorpb.MethodOptions_IdempotencyLevel) *descriptorpb.MethodOptions_IdempotencyLevel {
	return &l
}

func policyFor(md protoreflect.MethodDescriptor) *Policy {
	return NewPolicyWithResolver(func(string) (protoreflect.MethodDescriptor, error) {
		if md == nil {
			return nil, errors.New("not found")
		}
		return md, nil
	})
}

func newRetrier(t *testing.T, cfg Config, p *Policy, b *budget.Budget) *Retrier {
	t.Helper()
	d := deadline.Budget{Buffer: time.Millisecond, Minimum: time.Millisecond}
	r, err := New(cfg, p, b, d, nil)
	require.NoError(t, err)
	return r
}

func fastConfig() Config {
	// Tiny backoff: these tests exercise the decision logic, not the
	// waiting. The backoff arithmetic gets its own test.
	return Config{MaxAttempts: 2, Base: time.Microsecond, MaxBackoff: time.Microsecond}
}

func unavailable() error { return connect.NewError(connect.CodeUnavailable, errors.New("down")) }

// --------------------------------------------------------------- policy

func TestUnannotatedMethodIsNeverRetried(t *testing.T) {
	// Fail closed. This is the property that stops the framework from
	// double-charging a credit card because someone forgot an annotation.
	md := buildMethod(t, nil)
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	err := r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return unavailable()
	})

	require.Error(t, err)
	require.Equal(t, 1, attempts, "an unannotated method was retried")
}

func TestUnresolvableMethodIsNeverRetried(t *testing.T) {
	// A descriptor that cannot be found is not known to be safe, and "not
	// known to be safe" has to mean "not retried".
	r := newRetrier(t, fastConfig(), policyFor(nil), nil)

	var attempts int
	_ = r.Do(context.Background(), "/unknown.Service/Method", func(context.Context, int) error {
		attempts++
		return unavailable()
	})
	require.Equal(t, 1, attempts)
}

func TestNoSideEffectsMethodIsRetried(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	err := r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return unavailable()
	})

	require.Error(t, err)
	require.Equal(t, 3, attempts, "expected the initial attempt plus MaxAttempts retries")
}

func TestIdempotentMethodIsRetried(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_IDEMPOTENT))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return unavailable()
	})
	require.Equal(t, 3, attempts)
}

func TestIdempotencyUnknownIsNotRetried(t *testing.T) {
	// IDEMPOTENCY_UNKNOWN is protobuf's default, which means the author
	// said nothing. Treating silence as permission is the whole bug.
	md := buildMethod(t, idem(descriptorpb.MethodOptions_IDEMPOTENCY_UNKNOWN))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return unavailable()
	})
	require.Equal(t, 1, attempts)
}

func TestPolicyCachesTheDescriptorLookup(t *testing.T) {
	// Reflection on every RPC would put a registry lookup in the hot path.
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	var lookups int
	p := NewPolicyWithResolver(func(string) (protoreflect.MethodDescriptor, error) {
		lookups++
		return md, nil
	})

	for i := 0; i < 100; i++ {
		require.True(t, p.Retryable("/test.v1.TestService/Do"))
	}
	require.Equal(t, 1, lookups)
}

// ------------------------------------------------------------- retrying

func TestSuccessOnFirstAttemptMakesNoFurtherCalls(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	err := r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, attempts)
}

func TestRetryStopsAtTheFirstSuccess(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	err := r.Do(context.Background(), "/test.v1.TestService/Do", func(_ context.Context, a int) error {
		attempts++
		if a < 1 {
			return unavailable()
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

func TestNonRetryableErrorsStopImmediately(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	for _, code := range []connect.Code{
		connect.CodeInvalidArgument,
		connect.CodeNotFound,
		connect.CodePermissionDenied,
		connect.CodeInternal, // a server fault, but repeating hits the same bug
	} {
		var attempts int
		_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
			attempts++
			return connect.NewError(code, errors.New("no"))
		})
		require.Equal(t, 1, attempts, "code %v was retried", code)
	}
}

func TestOpenBreakerIsNotRetried(t *testing.T) {
	// The adjacency that makes the breaker the cap on retry amplification
	// rather than a bystander to it: an open breaker fails instantly, so
	// retrying it would spend the budget without a request leaving the
	// process.
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var attempts int
	err := r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return resilience.ErrBreakerOpen
	})
	require.ErrorIs(t, err, resilience.ErrBreakerOpen)
	require.Equal(t, 1, attempts)
}

func TestAttemptNumberIsPassedThrough(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, fastConfig(), policyFor(md), nil)

	var seen []int
	_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(_ context.Context, a int) error {
		seen = append(seen, a)
		return unavailable()
	})
	require.Equal(t, []int{0, 1, 2}, seen)
}

func TestAttemptHeaderOmittedOnFirstTry(t *testing.T) {
	// gRPC omits grpc-previous-rpc-attempts entirely on the first attempt;
	// sending "0" would make every request look like a retry to a
	// downstream service deciding whether to retry again.
	require.Equal(t, "", AttemptHeader(0))
	require.Equal(t, "1", AttemptHeader(1))
	require.Equal(t, "2", AttemptHeader(2))
}

// --------------------------------------------------------------- budget

func TestBudgetStopsRetryStorms(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	clock := newFakeClock()

	// 10% budget, no floor: with 100 failing calls, retries are capped at
	// roughly a tenth of traffic rather than doubling it.
	b, err := budget.New(budget.Config{
		Ratio: 0.1, MinPerSecond: 0, Window: time.Minute, Buckets: 6,
	}, clock)
	require.NoError(t, err)

	r := newRetrier(t, fastConfig(), policyFor(md), b)

	var attempts int
	for i := 0; i < 100; i++ {
		_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
			attempts++
			return unavailable()
		})
	}

	requests, retries, exhausted := b.Stats()
	require.Equal(t, float64(100), requests)
	// Without a budget this would be 300 attempts. With one it is about
	// 110 — the difference between 3x load on a dying dependency and 1.1x.
	require.Less(t, attempts, 130, "budget did not contain the retry storm")
	require.Greater(t, attempts, 100, "budget blocked every retry")
	require.Positive(t, retries)
	require.Positive(t, exhausted)
}

func TestBudgetFloorLetsQuietServicesRetry(t *testing.T) {
	// Without a floor, a service handling two requests gets a budget of
	// 0.2 retries and can never retry at all — which is the case where a
	// retry is cheapest and most likely to help.
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	clock := newFakeClock()
	b, err := budget.New(budget.Config{
		Ratio: 0.1, MinPerSecond: 1, Window: 10 * time.Second, Buckets: 10,
	}, clock)
	require.NoError(t, err)

	r := newRetrier(t, fastConfig(), policyFor(md), b)

	var attempts int
	_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		attempts++
		return unavailable()
	})
	require.Equal(t, 3, attempts, "the floor did not permit retries on a quiet service")
}

func TestBudgetCountsLogicalCallsNotAttempts(t *testing.T) {
	// If attempts inflated the denominator, a retry storm would keep
	// granting itself more budget — the exact failure the budget prevents.
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	clock := newFakeClock()
	b, err := budget.New(budget.Config{
		Ratio: 0.5, MinPerSecond: 0, Window: time.Minute, Buckets: 6,
	}, clock)
	require.NoError(t, err)

	r := newRetrier(t, fastConfig(), policyFor(md), b)
	for i := 0; i < 10; i++ {
		_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
			return unavailable()
		})
	}

	requests, _, _ := b.Stats()
	require.Equal(t, float64(10), requests, "attempts were counted as requests")
}

func TestUnretryableCallsDoNotConsumeBudget(t *testing.T) {
	// A method that can never be retried should not be charged against the
	// budget that protects the methods that can.
	md := buildMethod(t, nil)
	clock := newFakeClock()
	b, err := budget.New(budget.DefaultConfig(), clock)
	require.NoError(t, err)

	r := newRetrier(t, fastConfig(), policyFor(md), b)
	for i := 0; i < 10; i++ {
		_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
			return unavailable()
		})
	}

	requests, retries, _ := b.Stats()
	require.Zero(t, requests)
	require.Zero(t, retries)
}

// ------------------------------------------------------------- deadline

func TestRetryStopsWhenTheDeadlineBudgetRunsOut(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	d := deadline.Budget{Buffer: 10 * time.Millisecond, Minimum: 50 * time.Millisecond}
	r, err := New(Config{MaxAttempts: 5, Base: time.Millisecond, MaxBackoff: time.Millisecond},
		policyFor(md), nil, d, nil)
	require.NoError(t, err)

	// 80ms of deadline: enough for one attempt, not for several.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	var attempts int
	_ = r.Do(ctx, "/test.v1.TestService/Do", func(ctx context.Context, _ int) error {
		attempts++
		// Burn most of the remaining budget.
		time.Sleep(30 * time.Millisecond)
		return unavailable()
	})

	require.Positive(t, attempts)
	require.Less(t, attempts, 5, "retried past the caller's deadline")
}

func TestAttemptDeadlineIsNarrowerThanTheCallers(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	d := deadline.Budget{Buffer: 50 * time.Millisecond, Minimum: 10 * time.Millisecond}
	r, err := New(fastConfig(), policyFor(md), nil, d, nil)
	require.NoError(t, err)

	outer, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outerDeadline, _ := outer.Deadline()

	err = r.Do(outer, "/test.v1.TestService/Do", func(ctx context.Context, _ int) error {
		dl, ok := ctx.Deadline()
		require.True(t, ok)
		require.True(t, dl.Before(outerDeadline),
			"the attempt deadline left no buffer for this hop's own work")
		return nil
	})
	require.NoError(t, err)
}

func TestContextWithoutDeadlineIsPassedThroughUnchanged(t *testing.T) {
	// Inventing a deadline here would silently cap call paths the operator
	// never configured, and the timeouts would be blamed on the dependency.
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	var missing []string
	d := deadline.Budget{
		Buffer:    time.Millisecond,
		Minimum:   time.Millisecond,
		OnMissing: func(p string) { missing = append(missing, p) },
	}
	r, err := New(fastConfig(), policyFor(md), nil, d, nil)
	require.NoError(t, err)

	err = r.Do(context.Background(), "/test.v1.TestService/Do", func(ctx context.Context, _ int) error {
		_, ok := ctx.Deadline()
		require.False(t, ok, "a deadline was invented")
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, missing, "an unbounded call path was not reported")
}

// ------------------------------------------------------------ mechanics

func TestBackoffRespectsCancellation(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	r := newRetrier(t, Config{MaxAttempts: 5, Base: time.Hour, MaxBackoff: time.Hour},
		policyFor(md), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.Do(ctx, "/test.v1.TestService/Do", func(context.Context, int) error {
			return unavailable()
		})
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the caller did not interrupt the backoff sleep")
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	_, err := New(Config{MaxAttempts: -1}, nil, nil, deadline.DefaultBudget(), nil)
	require.Error(t, err)
	_, err = New(Config{MaxAttempts: 2, Base: 0}, nil, nil, deadline.DefaultBudget(), nil)
	require.Error(t, err)
	_, err = New(Config{MaxAttempts: 2, Base: time.Second, MaxBackoff: time.Millisecond},
		nil, nil, deadline.DefaultBudget(), nil)
	require.Error(t, err)
}

func TestObserverSeesRetriesAndRefusals(t *testing.T) {
	md := buildMethod(t, idem(descriptorpb.MethodOptions_NO_SIDE_EFFECTS))
	clock := newFakeClock()
	b, err := budget.New(budget.Config{Ratio: 0, MinPerSecond: 0, Window: time.Minute, Buckets: 6}, clock)
	require.NoError(t, err)

	obs := &recordingObserver{}
	r, err := New(fastConfig(), policyFor(md), b, deadline.Budget{Buffer: time.Millisecond, Minimum: time.Millisecond}, obs)
	require.NoError(t, err)

	_ = r.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		return unavailable()
	})
	require.Positive(t, obs.exhausted, "budget refusals were not reported")

	plain := buildMethod(t, nil)
	r2 := newRetrier(t, fastConfig(), policyFor(plain), nil)
	r2.observer = obs
	_ = r2.Do(context.Background(), "/test.v1.TestService/Do", func(context.Context, int) error {
		return nil
	})
	require.Positive(t, obs.skipped, "a skipped non-idempotent method was not reported")
}

type recordingObserver struct {
	mu        sync.Mutex
	attempts  int
	exhausted int
	skipped   int
}

func (o *recordingObserver) RetryAttempt(context.Context, string, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts++
}
func (o *recordingObserver) RetryBudgetExhausted(context.Context, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.exhausted++
}
func (o *recordingObserver) RetrySkippedNotIdempotent(context.Context, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.skipped++
}

// protodescFile builds a FileDescriptor from a FileDescriptorProto without
// registering it globally, so each test can define its own annotations
// without colliding with another test's file of the same name.
func protodescFile(fd *descriptorpb.FileDescriptorProto) (protoreflect.FileDescriptor, error) {
	return protodesc.NewFile(fd, nil)
}
