// The downstream service in the two-hop example.
//
// It does nothing interesting on purpose: the point of the pair is to show
// a trace crossing a service boundary, a deadline shrinking as it goes, and
// a retry being marked so this service can see it is a retry.
//
//	SERVICE_NAME=greeter ENVIRONMENT=dev OTEL_TRACES_EXPORTER=stdout \
//	  MAIN_ADDR=:8081 ADMIN_ADDR=:9091 go run ./examples/twohops/greeter
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/interceptor"
	"github.com/syedjafri06193/microservices-api-framework/resilience/retry"
	"github.com/syedjafri06193/microservices-api-framework/service"
)

type greeter struct {
	echov1connect.UnimplementedEchoServiceHandler
}

func (g *greeter) Echo(
	ctx context.Context,
	req *connect.Request[echov1.EchoRequest],
) (*connect.Response[echov1.EchoResponse], error) {
	// The request logger already carries trace_id and span_id, because the
	// tracing interceptor ran above the logging interceptor and established
	// the span first.
	logger := interceptor.LoggerFrom(ctx)

	attempt := req.Header().Get(retry.HeaderPreviousAttempts)
	if attempt != "" {
		// A downstream service that can see a request is already a retry
		// can decline to retry its own dependencies — which is what stops
		// three layers of threefold retries becoming 27x load.
		logger.WarnContext(ctx, "handling a retried request", slog.String("attempt", attempt))
	}

	// What is left of the caller's deadline, after the frontend reserved
	// its own buffer. It is strictly less than what the original caller
	// set, which is the entire point of a deadline budget.
	if dl, ok := ctx.Deadline(); ok {
		logger.InfoContext(ctx, "greeting",
			slog.Duration("remaining", time.Until(dl)))
	}

	return connect.NewResponse(&echov1.EchoResponse{
		Message: "hello, " + req.Msg.GetMessage(),
		Attempt: attempt,
	}), nil
}

func main() {
	svc, err := service.New(
		service.WithConnectService(func(opts ...connect.HandlerOption) (string, http.Handler) {
			return echov1connect.NewEchoServiceHandler(&greeter{}, opts...)
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := svc.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
