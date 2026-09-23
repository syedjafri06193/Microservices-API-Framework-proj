// The upstream service in the two-hop example.
//
// It calls the greeter through the framework's client chain, which is where
// the resilience actually lives: a deadline budget that reserves time for
// this hop, a retry loop that only retries what the proto declares safe,
// and a circuit breaker inside that loop rather than outside it.
//
//	SERVICE_NAME=frontend ENVIRONMENT=dev OTEL_TRACES_EXPORTER=stdout \
//	  GREETER_ADDR=http://localhost:8081 go run ./examples/twohops/frontend
//
//	curl -X POST localhost:8080/echo.v1.EchoService/Echo \
//	  -H 'Content-Type: application/json' -d '{"message":"world"}'
//
// Stop the greeter and call it repeatedly: the breaker opens, and the
// frontend starts failing instantly and locally instead of waiting on a
// dependency that is not there. /debug/breakers on the admin port shows it.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"

	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/service"
)

type frontend struct {
	echov1connect.UnimplementedEchoServiceHandler
	greeter echov1connect.EchoServiceClient
}

func (f *frontend) Echo(
	ctx context.Context,
	req *connect.Request[echov1.EchoRequest],
) (*connect.Response[echov1.EchoResponse], error) {
	// The trace context propagates automatically — the client interceptor
	// injects the W3C traceparent header, and the greeter's server
	// interceptor extracts it. Neither service writes a line of code for
	// it, which is the difference between correlation that works and
	// correlation that works until someone forgets.
	resp, err := f.greeter.Echo(ctx, connect.NewRequest(&echov1.EchoRequest{
		Message: req.Msg.GetMessage(),
	}))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp.Msg), nil
}

func main() {
	greeterAddr := os.Getenv("GREETER_ADDR")
	if greeterAddr == "" {
		greeterAddr = "http://localhost:8081"
	}

	svc, err := service.New()
	if err != nil {
		log.Fatal(err)
	}

	// "greeter" is the breaker key's target: a configured name, never
	// anything a request can influence, so the breaker map cannot be made
	// to grow without bound by traffic.
	clientInterceptors, err := svc.ClientInterceptors("greeter")
	if err != nil {
		log.Fatal(err)
	}

	impl := &frontend{
		greeter: echov1connect.NewEchoServiceClient(
			&http.Client{Timeout: 10 * time.Second},
			greeterAddr,
			connect.WithInterceptors(clientInterceptors...),
		),
	}

	path, handler := echov1connect.NewEchoServiceHandler(impl, svc.HandlerOptions()...)
	svc.Handle(path, handler)

	if err := svc.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
