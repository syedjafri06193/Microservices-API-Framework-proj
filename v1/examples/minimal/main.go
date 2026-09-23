// A complete service. This is the whole point of the framework: everything
// below `main` is business logic, and the ~800 lines of wiring that every
// Go team copy-pastes between services is one call to service.New.
//
//	SERVICE_NAME=echo ENVIRONMENT=dev OTEL_TRACES_EXPORTER=stdout \
//	  go run ./examples/minimal
//
// Then, with no client library and no proxy:
//
//	curl -X POST localhost:8080/echo.v1.EchoService/Echo \
//	  -H 'Content-Type: application/json' -d '{"message":"hello"}'
//
// The same port also answers gRPC and gRPC-Web. That is Connect, not
// anything this framework invented — and being honest about which parts are
// borrowed is most of what separates a useful project from an overclaim.
package main

import (
	"context"
	"log"
	"net/http"

	"connectrpc.com/connect"

	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/service"
)

type server struct {
	echov1connect.UnimplementedEchoServiceHandler
}

func (s *server) Echo(
	ctx context.Context,
	req *connect.Request[echov1.EchoRequest],
) (*connect.Response[echov1.EchoResponse], error) {
	return connect.NewResponse(&echov1.EchoResponse{Message: req.Msg.GetMessage()}), nil
}

func main() {
	svc, err := service.New(
		service.WithConnectService(func(opts ...connect.HandlerOption) (string, http.Handler) {
			return echov1connect.NewEchoServiceHandler(&server{}, opts...)
		}),
	)
	if err != nil {
		// Fail fast and loudly, with a message naming the exact field. A
		// service that starts with a misconfiguration and fails on the
		// first request is worse than one that refuses to start.
		log.Fatal(err)
	}

	if err := svc.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
