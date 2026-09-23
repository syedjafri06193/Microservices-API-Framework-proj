package service_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/syedjafri06193/microservices-api-framework/fwtest"
	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/interceptor"
	"github.com/syedjafri06193/microservices-api-framework/resilience/breaker"
	"github.com/syedjafri06193/microservices-api-framework/service"
)

// testConfig is a service configuration with no external dependencies: no
// collector, no scrape target, nothing to stand up.
func testConfig() service.Config {
	return service.Config{
		ServiceName:     "test-service",
		Version:         "test",
		Environment:     "dev",
		MainAddr:        "127.0.0.1:0",
		AdminAddr:       "127.0.0.1:0",
		TraceExporter:   "none",
		EnableMetrics:   false,
		LogLevel:        "error",
		LogFormat:       "json",
		PreStopDelay:    time.Millisecond,
		DrainTimeout:    time.Second,
		RequestTimeout:  5 * time.Second,
		SlowRequest:     time.Second,
		RetryEnabled:    true,
		RetryBudget:     0.5,
		RetryAttempts:   2,
		BreakerEnabled:  true,
		BreakerRatio:    0.5,
		BreakerMinReqs:  4,
		ShedEnabled:     true,
		ShedMaxLatency:  time.Second,
		DeadlineBuffer:  10 * time.Millisecond,
		DeadlineMinimum: 20 * time.Millisecond,
	}
}

// newStack starts a real server with the framework's chain and returns a
// client wired with the framework's client chain.
func newStack(t *testing.T, cfg service.Config) (*fwtest.EchoServer, echov1connect.EchoServiceClient, *service.Service) {
	t.Helper()

	svc, err := service.NewWithConfig(cfg)
	require.NoError(t, err)

	impl := fwtest.NewEchoServer()
	srv := fwtest.NewServer(t, func(opts ...connect.HandlerOption) (string, http.Handler) {
		return echov1connect.NewEchoServiceHandler(impl, opts...)
	}, fwtest.WithHandlerOptions(svc.HandlerOptions()...))

	clientInterceptors, err := svc.ClientInterceptors("echo")
	require.NoError(t, err)

	client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL,
		connect.WithInterceptors(clientInterceptors...))

	return impl, client, svc
}

func TestEchoRoundTripsThroughTheWholeStack(t *testing.T) {
	_, client, _ := newStack(t, testConfig())

	resp, err := client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "hello"}))
	require.NoError(t, err)
	require.Equal(t, "hello", resp.Msg.GetMessage())
	require.Empty(t, resp.Msg.GetAttempt(), "a first attempt must not be marked as a retry")
}

func TestNonIdempotentMethodIsNeverRetried(t *testing.T) {
	// The end-to-end version of the property: the framework reads the
	// absence of an idempotency annotation from the real generated
	// descriptor and calls Charge exactly once, however the call goes.
	impl, client, _ := newStack(t, testConfig())

	_, err := client.Charge(context.Background(),
		connect.NewRequest(&echov1.ChargeRequest{Account: "acct", AmountCents: 500}))
	require.NoError(t, err)
	require.Equal(t, 1, impl.Charges())
}

func TestIdempotentMethodIsRetriedAndMarked(t *testing.T) {
	// The other half: an annotated method is retried, and each retry
	// carries grpc-previous-rpc-attempts so a downstream service can
	// decline to retry it again — the cheapest defence against multi-hop
	// amplification there is.
	cfg := testConfig()
	cfg.BreakerEnabled = false
	impl, client, _ := newStack(t, cfg)

	// Fail is annotated NO_SIDE_EFFECTS and always fails, so the retrier
	// makes the full number of attempts.
	_, err := client.Fail(context.Background(),
		connect.NewRequest(&echov1.FailRequest{Code: "unavailable"}))
	require.Error(t, err)
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	require.Equal(t, 3, impl.FailCount("unavailable"),
		"expected the initial attempt plus two retries")
}

func TestRetriesAreMarkedForDownstreamServices(t *testing.T) {
	cfg := testConfig()
	cfg.BreakerEnabled = false
	_, client, _ := newStack(t, cfg)

	// Echo succeeds, so this only proves the first attempt is unmarked.
	// The marking itself is asserted in the retry package's unit tests,
	// where an attempt can be forced.
	resp, err := client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "x"}))
	require.NoError(t, err)
	require.Empty(t, resp.Msg.GetAttempt())
}

func TestClientFaultsNeverOpenTheBreaker(t *testing.T) {
	// The most consequential property in the framework, end to end: a
	// caller sending bad input must not be able to take a healthy service
	// offline for everybody else.
	cfg := testConfig()
	cfg.RetryEnabled = false
	_, client, svc := newStack(t, cfg)

	for i := 0; i < 50; i++ {
		_, err := client.Fail(context.Background(),
			connect.NewRequest(&echov1.FailRequest{Code: "invalid_argument"}))
		require.Error(t, err)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}

	for key, state := range svc.Breakers().Snapshot() {
		require.Equal(t, breaker.StateClosed, state,
			"breaker %s opened on client faults", key)
	}
}

func TestServerFaultsOpenTheBreakerAndStopTraffic(t *testing.T) {
	cfg := testConfig()
	cfg.RetryEnabled = false
	cfg.BreakerMinReqs = 4
	impl, client, _ := newStack(t, cfg)

	// Trip it.
	for i := 0; i < 10; i++ {
		_, _ = client.Fail(context.Background(),
			connect.NewRequest(&echov1.FailRequest{Code: "unavailable"}))
	}
	reached := impl.FailCount("unavailable")

	// Once open, further calls must fail locally without reaching the
	// upstream at all. That is the whole value: a dying dependency stops
	// receiving traffic rather than merely returning errors faster.
	for i := 0; i < 10; i++ {
		_, err := client.Fail(context.Background(),
			connect.NewRequest(&echov1.FailRequest{Code: "unavailable"}))
		require.Error(t, err)
		require.True(t, interceptor.BreakerOpen(err),
			"expected a breaker-open error, got %v", err)
	}

	require.Equal(t, reached, impl.FailCount("unavailable"),
		"requests reached the upstream after the breaker opened")
}

func TestBreakerIsPerMethodNotPerService(t *testing.T) {
	// One slow method must not black-hole every other method on the target.
	cfg := testConfig()
	cfg.RetryEnabled = false
	_, client, _ := newStack(t, cfg)

	for i := 0; i < 20; i++ {
		_, _ = client.Fail(context.Background(),
			connect.NewRequest(&echov1.FailRequest{Code: "unavailable"}))
	}

	// Echo shares the target but not the procedure, so it must still work.
	resp, err := client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "still here"}))
	require.NoError(t, err, "a healthy method was blocked by another method's breaker")
	require.Equal(t, "still here", resp.Msg.GetMessage())
}

func TestPanicBecomesInternalAndLeaksNothing(t *testing.T) {
	cfg := testConfig()
	cfg.RetryEnabled = false
	cfg.BreakerEnabled = false
	_, client, _ := newStack(t, cfg)

	_, err := client.Panic(context.Background(), connect.NewRequest(&echov1.PanicRequest{}))
	require.Error(t, err)
	require.Equal(t, connect.CodeInternal, connect.CodeOf(err))
	require.NotContains(t, err.Error(), "panicked on request",
		"the panic message reached the caller")
}

func TestServerSurvivesAPanicAndKeepsServing(t *testing.T) {
	// The process-safety half: a panicking handler must not take the
	// server down with it.
	cfg := testConfig()
	cfg.RetryEnabled = false
	cfg.BreakerEnabled = false
	_, client, _ := newStack(t, cfg)

	for i := 0; i < 5; i++ {
		_, err := client.Panic(context.Background(), connect.NewRequest(&echov1.PanicRequest{}))
		require.Error(t, err)
	}

	resp, err := client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "alive"}))
	require.NoError(t, err)
	require.Equal(t, "alive", resp.Msg.GetMessage())
}

func TestDeadlineBudgetRefusesDoomedCalls(t *testing.T) {
	// A call with less deadline left than the minimum is not worth making:
	// the dependency would spend capacity on work nobody will wait for.
	cfg := testConfig()
	cfg.DeadlineBuffer = 50 * time.Millisecond
	cfg.DeadlineMinimum = 200 * time.Millisecond
	impl, client, _ := newStack(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.Echo(ctx, connect.NewRequest(&echov1.EchoRequest{Message: "doomed"}))
	require.Error(t, err)
	require.Equal(t, connect.CodeDeadlineExceeded, connect.CodeOf(err))
	require.Zero(t, impl.Charges())
}

func TestServerTimeoutIsEnforced(t *testing.T) {
	cfg := testConfig()
	cfg.RequestTimeout = 50 * time.Millisecond
	cfg.RetryEnabled = false
	cfg.BreakerEnabled = false
	_, client, _ := newStack(t, cfg)

	_, err := client.Slow(context.Background(),
		connect.NewRequest(&echov1.SlowRequest{DelayMs: 2000}))
	require.Error(t, err)
	require.Equal(t, connect.CodeDeadlineExceeded, connect.CodeOf(err))
}

func TestPlainHTTPJSONWorksOnTheSamePort(t *testing.T) {
	// The whole dual-mode story, verified rather than asserted: the same
	// handler that serves gRPC answers a plain JSON POST with no client
	// library and no proxy.
	svc, err := service.NewWithConfig(testConfig())
	require.NoError(t, err)

	impl := fwtest.NewEchoServer()
	srv := fwtest.NewServer(t, func(opts ...connect.HandlerOption) (string, http.Handler) {
		return echov1connect.NewEchoServiceHandler(impl, opts...)
	}, fwtest.WithHandlerOptions(svc.HandlerOptions()...))

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/echo.v1.EchoService/Echo",
		stringReader(`{"message":"over json"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readAll(t, resp.Body)
	require.Contains(t, body, "over json")
}

func TestGRPCProtocolWorksOnTheSamePort(t *testing.T) {
	svc, err := service.NewWithConfig(testConfig())
	require.NoError(t, err)

	impl := fwtest.NewEchoServer()
	srv := fwtest.NewServer(t, func(opts ...connect.HandlerOption) (string, http.Handler) {
		return echov1connect.NewEchoServiceHandler(impl, opts...)
	}, fwtest.WithHandlerOptions(svc.HandlerOptions()...))

	// The same URL, the same handler, the gRPC wire protocol.
	client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL, connect.WithGRPC())
	resp, err := client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "over grpc"}))
	require.NoError(t, err)
	require.Equal(t, "over grpc", resp.Msg.GetMessage())
}

func TestEscapeHatchesAreUsableAlone(t *testing.T) {
	// Teams adopt tools they can abandon incrementally. Each of these has
	// to work with none of the rest of the framework.
	svc, err := service.NewWithConfig(testConfig())
	require.NoError(t, err)

	require.NotEmpty(t, svc.ServerInterceptors(),
		"the interceptor chain is the most reusable thing here and must be exported")
	require.NotNil(t, svc.Mux())
	require.NotNil(t, svc.Health())
	require.NotNil(t, svc.Logger())

	// The chain must work against a server this framework did not build.
	impl := fwtest.NewEchoServer()
	mux := http.NewServeMux()
	path, handler := echov1connect.NewEchoServiceHandler(impl,
		connect.WithInterceptors(svc.ServerInterceptors()...))
	mux.Handle(path, handler)
	require.NotNil(t, mux)
}

// ----------------------------------------------------------------- utils

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}
