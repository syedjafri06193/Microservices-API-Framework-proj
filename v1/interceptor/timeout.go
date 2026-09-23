package interceptor

import (
	"context"
	"time"

	"connectrpc.com/connect"
)

// Timeout caps how long a handler may run.
//
// It only ever *shortens* the deadline. A request that arrives with a
// tighter deadline than the server's default keeps its own: the caller said
// how long it is prepared to wait, and extending that would mean the server
// finishing work after the caller has given up — which is the waste the
// deadline budget exists to eliminate on the client side.
func Timeout(d time.Duration) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if d <= 0 {
				return next(ctx, req)
			}
			if existing, ok := ctx.Deadline(); ok && time.Until(existing) <= d {
				return next(ctx, req)
			}
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, req)
		}
	}
}
