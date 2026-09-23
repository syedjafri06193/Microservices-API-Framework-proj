package interceptor

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"github.com/syedjafri06193/microservices-api-framework/resilience/shed"
)

// LoadShed rejects requests when the service is behind.
//
// It sits *below* metrics and *above* auth. Below metrics, so that shed
// requests are counted — a shedder outside the metrics interceptor makes an
// overload incident invisible, and the dashboard shows a healthy service
// while callers get errors. Above auth, so that a flood is rejected before
// the service pays for a JWT verification or a call to an authz service.
//
// The rejection is CodeResourceExhausted, never CodeUnavailable. The
// distinction matters: ResourceExhausted is retryable with backoff and is
// counted by the breaker, which is the behaviour you want — the caller
// should back off and the breaker should notice the dependency is
// saturated. CodeUnavailable would suggest the service is gone.
func LoadShed(s *shed.Shedder, queuedFor func(context.Context) time.Duration) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if s == nil {
				return next(ctx, req)
			}

			var queued time.Duration
			if queuedFor != nil {
				queued = queuedFor(ctx)
			}

			done, ok := s.Admit(queued)
			if !ok {
				return nil, connect.NewError(connect.CodeResourceExhausted,
					errors.New("server overloaded, retry with backoff"))
			}
			defer done()

			return next(ctx, req)
		}
	}
}
