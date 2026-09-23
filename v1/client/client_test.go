package client

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/syedjafri06193/microservices-api-framework/fwtest"
	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/interceptor"
	"github.com/syedjafri06193/microservices-api-framework/resilience/breaker"
)

// The escape-hatch property: this works against a plain Connect handler,
// with none of the rest of the framework involved.
func newBareServer(t *testing.T) (*fwtest.EchoServer, *fwtest.Server) {
	t.Helper()
	impl := fwtest.NewEchoServer()
	srv := fwtest.NewServer(t, func(opts ...connect.HandlerOption) (string, http.Handler) {
		return echov1connect.NewEchoServiceHandler(impl, opts...)
	})
	return impl, srv
}

func TestClientWorksAgainstAPlainConnectHandler(t *testing.T) {
	impl, srv := newBareServer(t)

	c, err := New(DefaultConfig("echo"))
	require.NoError(t, err)

	client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL, c.Options...)
	resp, err := client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "hi"}))
	require.NoError(t, err)
	require.Equal(t, "hi", resp.Msg.GetMessage())
	require.Zero(t, impl.Charges())
}

func TestTargetIsRequired(t *testing.T) {
	// It keys the breaker and labels metrics. An empty target would put
	// every peer in one breaker, which is the failure mode per-method
	// keying exists to avoid, one level up.
	_, err := New(Config{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Target is required")
}

func TestBreakerOpensAndStopsTraffic(t *testing.T) {
	impl, srv := newBareServer(t)

	cfg := DefaultConfig("echo")
	cfg.DisableRetry = true
	cfg.Breaker.MinimumRequests = 4
	c, err := New(cfg)
	require.NoError(t, err)

	client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL, c.Options...)

	for i := 0; i < 10; i++ {
		_, _ = client.Fail(context.Background(),
			connect.NewRequest(&echov1.FailRequest{Code: "unavailable"}))
	}
	reached := impl.FailCount("unavailable")

	_, err = client.Fail(context.Background(),
		connect.NewRequest(&echov1.FailRequest{Code: "unavailable"}))
	require.True(t, interceptor.BreakerOpen(err))
	require.Equal(t, reached, impl.FailCount("unavailable"),
		"a request reached the upstream after the breaker opened")

	require.Equal(t, breaker.StateOpen,
		c.Breakers().Get(breaker.Key("echo", "/echo.v1.EchoService/Fail")).State())
}

func TestDisablingBothMechanismsLeavesAWorkingClient(t *testing.T) {
	// What a mesh deployment configures. The client must still function,
	// just without in-process retries or breaking.
	_, srv := newBareServer(t)

	cfg := DefaultConfig("echo")
	cfg.DisableRetry = true
	cfg.DisableBreaker = true
	c, err := New(cfg)
	require.NoError(t, err)
	require.Nil(t, c.Breakers())
	require.Nil(t, c.Budget())

	client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL, c.Options...)
	_, err = client.Echo(context.Background(),
		connect.NewRequest(&echov1.EchoRequest{Message: "x"}))
	require.NoError(t, err)
}

func TestTransportDefaultsAreRaisedForServiceToServiceTraffic(t *testing.T) {
	// Go's default MaxIdleConnsPerHost is 2. A service making hundreds of
	// concurrent calls to one peer would spend its time in handshakes, and
	// the symptom looks exactly like the dependency being slow.
	c := NewHTTPClient(0)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.Greater(t, tr.MaxIdleConnsPerHost, 2)
	require.True(t, tr.ForceAttemptHTTP2, "gRPC over TLS does not work without this")
}
