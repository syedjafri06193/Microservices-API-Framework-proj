package service_test

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/syedjafri06193/microservices-api-framework/fwtest"
	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/service"
)

// BenchmarkChainOverhead measures what the framework costs per RPC against
// bare Connect.
//
// This number is the first thing a skeptical Go developer asks for, and
// having it ready is worth more than a paragraph of prose about how
// lightweight the framework is. Track it in CI; a regression here is a
// regression users will feel.
//
//	go test ./service/ -bench=ChainOverhead -benchmem
func BenchmarkChainOverhead(b *testing.B) {
	b.Run("bare-connect", func(b *testing.B) {
		impl := fwtest.NewEchoServer()
		srv := newBenchServer(b, impl)
		client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL)
		runEcho(b, client)
	})

	b.Run("with-framework", func(b *testing.B) {
		cfg := testConfig()
		svc, err := service.NewWithConfig(cfg)
		if err != nil {
			b.Fatal(err)
		}
		impl := fwtest.NewEchoServer()
		srv := newBenchServer(b, impl, svc.HandlerOptions()...)
		client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL)
		runEcho(b, client)
	})

	b.Run("with-framework-and-client-chain", func(b *testing.B) {
		cfg := testConfig()
		svc, err := service.NewWithConfig(cfg)
		if err != nil {
			b.Fatal(err)
		}
		impl := fwtest.NewEchoServer()
		srv := newBenchServer(b, impl, svc.HandlerOptions()...)

		clientInterceptors, err := svc.ClientInterceptors("echo")
		if err != nil {
			b.Fatal(err)
		}
		client := echov1connect.NewEchoServiceClient(srv.Client(), srv.URL,
			connect.WithInterceptors(clientInterceptors...))
		runEcho(b, client)
	})
}

func newBenchServer(b *testing.B, impl *fwtest.EchoServer, opts ...connect.HandlerOption) *fwtest.Server {
	b.Helper()
	return fwtest.NewServer(b, func(o ...connect.HandlerOption) (string, http.Handler) {
		return echov1connect.NewEchoServiceHandler(impl, o...)
	}, fwtest.WithHandlerOptions(opts...))
}

func runEcho(b *testing.B, client echov1connect.EchoServiceClient) {
	ctx := context.Background()
	req := connect.NewRequest(&echov1.EchoRequest{Message: "bench"})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Echo(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
