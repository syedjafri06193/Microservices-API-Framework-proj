package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/syedjafri06193/microservices-api-framework/resilience/shed"
)

// recorder builds interceptors that log when they are entered and exited,
// which is the only way to assert on ordering.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) named(name string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			r.record(name + ":enter")
			resp, err := next(ctx, req)
			r.record(name + ":exit")
			return resp, err
		}
	}
}

// invoke runs a chain against a handler without a network, which keeps
// these tests about ordering rather than transport.
func invoke(t *testing.T, chain []connect.Interceptor, handler connect.UnaryFunc) error {
	t.Helper()
	next := handler
	// Connect applies interceptors outermost-first, which means wrapping
	// from the end of the slice backwards.
	for i := len(chain) - 1; i >= 0; i-- {
		next = chain[i].WrapUnary(next)
	}
	_, err := next(context.Background(), &fakeRequest{})
	return err
}

// fakeRequest implements just enough of connect.AnyRequest to drive the
// chain without a transport. The embedded interface is nil, so every method
// not defined here panics — which is deliberate: a test that needs another
// method should add it rather than silently receiving a zero value.
type fakeRequest struct {
	connect.AnyRequest
	header http.Header
}

func (r *fakeRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: "/test.v1.TestService/Do"}
}
func (r *fakeRequest) Any() any { return nil }
func (r *fakeRequest) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

// -------------------------------------------------------------- ordering

func TestChainRunsOutermostFirst(t *testing.T) {
	// The bugs live in composition, so this asserts the shape of the whole
	// chain rather than any one interceptor.
	r := &recorder{}
	chain := ServerChain{
		PanicGuard: r.named("guard"),
		Tracing:    r.named("trace"),
		Logging:    r.named("log"),
		Metrics:    r.named("metrics"),
		LoadShed:   r.named("shed"),
		Timeout:    r.named("timeout"),
		Auth:       r.named("auth"),
		Validation: r.named("validate"),
		Recovery:   r.named("recovery"),
	}.Interceptors()

	err := invoke(t, chain, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		r.record("handler")
		return nil, nil
	})
	require.NoError(t, err)

	require.Equal(t, []string{
		"guard:enter", "trace:enter", "log:enter", "metrics:enter",
		"shed:enter", "timeout:enter", "auth:enter", "validate:enter",
		"recovery:enter",
		"handler",
		"recovery:exit", "validate:exit", "auth:exit", "timeout:exit",
		"shed:exit", "metrics:exit", "log:exit", "trace:exit", "guard:exit",
	}, r.snapshot())
}

func TestTracingIsAboveLogging(t *testing.T) {
	// A log line without a trace ID is nearly useless in a distributed
	// system, so the span has to exist before any logger is constructed.
	r := &recorder{}
	chain := ServerChain{Tracing: r.named("trace"), Logging: r.named("log")}.Interceptors()
	require.NoError(t, invoke(t, chain, nilHandler))

	calls := r.snapshot()
	require.Less(t, indexOf(calls, "trace:enter"), indexOf(calls, "log:enter"))
}

func TestMetricsIsAboveLoadShedding(t *testing.T) {
	// If shedding sat outside metrics, shed requests would be invisible and
	// the dashboard would show a healthy service throughout an overload.
	r := &recorder{}
	chain := ServerChain{Metrics: r.named("metrics"), LoadShed: r.named("shed")}.Interceptors()
	require.NoError(t, invoke(t, chain, nilHandler))

	calls := r.snapshot()
	require.Less(t, indexOf(calls, "metrics:enter"), indexOf(calls, "shed:enter"))
}

func TestLoadSheddingIsAboveAuth(t *testing.T) {
	// Auth can mean a JWKS fetch or a call to an authz service. Under a
	// flood you want to reject before paying for that.
	r := &recorder{}
	chain := ServerChain{LoadShed: r.named("shed"), Auth: r.named("auth")}.Interceptors()
	require.NoError(t, invoke(t, chain, nilHandler))

	calls := r.snapshot()
	require.Less(t, indexOf(calls, "shed:enter"), indexOf(calls, "auth:enter"))
}

func TestRecoveryIsInnermost(t *testing.T) {
	r := &recorder{}
	chain := ServerChain{
		Tracing:  r.named("trace"),
		Metrics:  r.named("metrics"),
		Recovery: r.named("recovery"),
		Extra:    []connect.Interceptor{r.named("user")},
	}.Interceptors()
	require.NoError(t, invoke(t, chain, nilHandler))

	// The last ":enter" recorded is the interceptor closest to the handler.
	calls := r.snapshot()
	var lastEnter string
	for _, c := range calls {
		if strings.HasSuffix(c, ":enter") {
			lastEnter = c
		}
	}
	require.Equal(t, "recovery:enter", lastEnter,
		"recovery must be the last interceptor entered before the handler")
}

func TestUserInterceptorsRunInsideObservability(t *testing.T) {
	// A user interceptor that is slow should show up in the framework's
	// latency metric, and one that panics should be caught by recovery.
	r := &recorder{}
	chain := ServerChain{
		Metrics:  r.named("metrics"),
		Recovery: r.named("recovery"),
		Extra:    []connect.Interceptor{r.named("user")},
	}.Interceptors()
	require.NoError(t, invoke(t, chain, nilHandler))

	calls := r.snapshot()
	require.Less(t, indexOf(calls, "metrics:enter"), indexOf(calls, "user:enter"))
	require.Less(t, indexOf(calls, "user:enter"), indexOf(calls, "recovery:enter"))
}

func TestNilInterceptorsAreSkipped(t *testing.T) {
	// A service with auth and validation disabled should not carry two
	// no-op frames on every call.
	r := &recorder{}
	chain := ServerChain{Tracing: r.named("trace")}.Interceptors()
	require.Len(t, chain, 1)
	require.NoError(t, invoke(t, chain, nilHandler))
	require.Equal(t, []string{"trace:enter", "trace:exit"}, r.snapshot())
}

func TestClientChainPutsResilienceInnermost(t *testing.T) {
	// Tracing and metrics are per *logical* call, so they must wrap the
	// retry loop rather than sit inside it — otherwise a retried call
	// produces three spans and three observations for one thing the caller
	// asked for once.
	r := &recorder{}
	chain := ClientChain{
		Tracing:    r.named("trace"),
		Metrics:    r.named("metrics"),
		Resilience: r.named("resilience"),
	}.Interceptors()
	require.NoError(t, invoke(t, chain, nilHandler))

	calls := r.snapshot()
	require.Equal(t, []string{
		"trace:enter", "metrics:enter", "resilience:enter",
		"resilience:exit", "metrics:exit", "trace:exit",
	}, calls)
}

// --------------------------------------------------------------- panics

func TestPanicIsObservedByOuterInterceptors(t *testing.T) {
	// The real requirement behind recovery-innermost, and the reason it is
	// worth being counterintuitive about.
	//
	// A panicking handler must produce an error that the tracing, logging
	// and metrics interceptors *see*. If recovery were outermost, the panic
	// would become a tidy error before any of them ran, and a crashing
	// handler would look like an ordinary failure on every dashboard.
	r := &recorder{}
	var observed error

	observer := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			r.record("observer:enter")
			resp, err := next(ctx, req)
			observed = err
			r.record("observer:exit")
			return resp, err
		}
	})

	logger := slog.New(slog.NewTextHandler(discard{}, nil))
	chain := ServerChain{
		Metrics:  observer,
		Recovery: Recovery(logger),
	}.Interceptors()

	err := invoke(t, chain, func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		panic("handler exploded")
	})

	require.Error(t, err)
	require.Equal(t, connect.CodeInternal, connect.CodeOf(err))
	require.Error(t, observed, "the outer interceptor never saw the panic as an error")
	require.Equal(t, connect.CodeInternal, connect.CodeOf(observed))

	// And the outer interceptor's deferred work ran, which it would not
	// have if the panic had propagated past it.
	require.Contains(t, r.snapshot(), "observer:exit")
}

func TestPanicDetailsDoNotReachTheCaller(t *testing.T) {
	// A panic message routinely contains a pointer, a query, or a fragment
	// of somebody's data.
	logger := slog.New(slog.NewTextHandler(discard{}, nil))
	chain := ServerChain{Recovery: Recovery(logger)}.Interceptors()

	err := invoke(t, chain, func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		panic("user 4815162342 token sk-secret-value")
	})

	require.Error(t, err)
	require.NotContains(t, err.Error(), "sk-secret-value")
	require.NotContains(t, err.Error(), "4815162342")
	require.Contains(t, err.Error(), "internal error")
}

func TestPanicIsLoggedWithAStack(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	chain := ServerChain{Recovery: Recovery(logger)}.Interceptors()

	_ = invoke(t, chain, func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		panic("boom")
	})

	out := buf.String()
	require.Contains(t, out, "panic recovered")
	require.Contains(t, out, "boom", "the panic value must reach the logs even though it must not reach the caller")
	require.Contains(t, out, "stack")
}

func TestPanicGuardCatchesPanicsInInterceptors(t *testing.T) {
	// Recovery, being innermost, cannot protect against a panic in an
	// interceptor above it — including in the tracing or metrics
	// interceptors themselves.
	logger := slog.New(slog.NewTextHandler(discard{}, nil))

	exploding := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			panic("interceptor exploded")
		}
	})

	chain := ServerChain{
		PanicGuard: PanicGuard(logger),
		Metrics:    exploding,
		Recovery:   Recovery(logger),
	}.Interceptors()

	err := invoke(t, chain, nilHandler)
	require.Error(t, err)
	require.Equal(t, connect.CodeInternal, connect.CodeOf(err))
}

func TestAbortHandlerIsRepanicked(t *testing.T) {
	// http.ErrAbortHandler is a sentinel net/http uses to abort a
	// connection deliberately. Swallowing it breaks that mechanism and logs
	// a fake error for something working as designed.
	logger := slog.New(slog.NewTextHandler(discard{}, nil))
	chain := ServerChain{Recovery: Recovery(logger)}.Interceptors()

	require.Panics(t, func() {
		_ = invoke(t, chain, func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			panic(http.ErrAbortHandler)
		})
	})
}

// -------------------------------------------------------------- timeout

func TestTimeoutOnlyShortensADeadline(t *testing.T) {
	// The caller said how long it is prepared to wait. Extending that would
	// mean finishing work after the caller has given up.
	chain := ServerChain{Timeout: Timeout(time.Hour)}.Interceptors()

	var handlerDeadline time.Time
	next := chain[0].WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		handlerDeadline, _ = ctx.Deadline()
		return nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()

	_, err := next(ctx, &fakeRequest{})
	require.NoError(t, err)
	require.Equal(t, callerDeadline, handlerDeadline,
		"the server's longer timeout overrode the caller's shorter one")
}

func TestTimeoutAppliesWhenTheCallerHasNoDeadline(t *testing.T) {
	chain := ServerChain{Timeout: Timeout(time.Minute)}.Interceptors()

	var hasDeadline bool
	next := chain[0].WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		_, hasDeadline = ctx.Deadline()
		return nil, nil
	})

	_, err := next(context.Background(), &fakeRequest{})
	require.NoError(t, err)
	require.True(t, hasDeadline, "an unbounded request was allowed to run forever")
}

// ------------------------------------------------------------ load shed

func TestLoadShedReturnsResourceExhausted(t *testing.T) {
	// Not CodeUnavailable. ResourceExhausted is retryable with backoff and
	// is counted by the breaker, which is the behaviour you want:
	// Unavailable would suggest the service is gone rather than busy.
	s, err := shed.New(shed.Config{MaxQueueLatency: time.Millisecond})
	require.NoError(t, err)

	chain := ServerChain{
		LoadShed: LoadShed(s, func(context.Context) time.Duration { return time.Hour }),
	}.Interceptors()

	invokeErr := invoke(t, chain, nilHandler)
	require.Error(t, invokeErr)
	require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(invokeErr))
}

func TestLoadShedAdmitsWhenNotOverloaded(t *testing.T) {
	s, err := shed.New(shed.Config{MaxQueueLatency: time.Hour})
	require.NoError(t, err)

	var reached bool
	chain := ServerChain{
		LoadShed: LoadShed(s, func(context.Context) time.Duration { return 0 }),
	}.Interceptors()

	require.NoError(t, invoke(t, chain, func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		reached = true
		return nil, nil
	}))
	require.True(t, reached)
}

// ------------------------------------------------------------------ auth

func TestAuthRejectsWithoutAPrincipal(t *testing.T) {
	chain := ServerChain{
		Auth: Auth(AuthenticatorFunc(func(context.Context, Header) (any, error) {
			return nil, errors.New("no token")
		}), nil),
	}.Interceptors()

	err := invoke(t, chain, nilHandler)
	require.Error(t, err)
	// Not Unknown. Unknown is classified as a server fault, so a wave of
	// bad tokens would trip the caller's breaker against a service that is
	// working perfectly.
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestAuthPassesThePrincipalToTheHandler(t *testing.T) {
	chain := ServerChain{
		Auth: Auth(AuthenticatorFunc(func(context.Context, Header) (any, error) {
			return "alice", nil
		}), nil),
	}.Interceptors()

	var seen any
	require.NoError(t, invoke(t, chain, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		seen, _ = PrincipalFrom(ctx)
		return nil, nil
	}))
	require.Equal(t, "alice", seen)
}

func TestPublicProceduresAreMatchedExactly(t *testing.T) {
	// A prefix match on "/test.v1.TestService/Do" would also exempt
	// "/test.v1.TestService/DoAdminThing". An authentication bypass that
	// comes from a string prefix is how a CVE starts.
	var authCalled bool
	auth := AuthenticatorFunc(func(context.Context, Header) (any, error) {
		authCalled = true
		return "principal", nil
	})

	exempt := ServerChain{
		Auth: Auth(auth, map[string]bool{"/test.v1.TestService/Do": true}),
	}.Interceptors()
	require.NoError(t, invoke(t, exempt, nilHandler))
	require.False(t, authCalled, "an exempt procedure still ran authentication")

	// A procedure that merely shares a prefix is not exempt.
	authCalled = false
	notExempt := ServerChain{
		Auth: Auth(auth, map[string]bool{"/test.v1.TestService/D": true}),
	}.Interceptors()
	require.NoError(t, invoke(t, notExempt, nilHandler))
	require.True(t, authCalled, "a prefix of the procedure name exempted it")
}

// ----------------------------------------------------------------- utils

func nilHandler(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
	return nil, nil
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
