package interceptor

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/syedjafri06193/microservices-api-framework/resilience"
	"github.com/syedjafri06193/microservices-api-framework/resilience/breaker"
	"github.com/syedjafri06193/microservices-api-framework/resilience/retry"
)

// Resilience is the client-side interceptor that combines the deadline
// budget, the retry loop and the circuit breaker.
//
// They are one interceptor rather than three because their nesting is not
// negotiable and splitting them would let a user reassemble them wrongly.
// The order inside is:
//
//	deadline budget  →  retry loop  →  breaker  →  transport
//
// The breaker sits *inside* the retry loop. Both orderings look defensible,
// so it is worth being explicit about why:
//
// With the breaker outside, one logical call is one breaker observation.
// Three failing attempts look like a single failure, so the breaker trips
// slowly — and while it is deciding, you are sending three times the
// traffic to a dependency that is already struggling.
//
// With the breaker inside, every attempt is an observation. The breaker
// opens quickly, and once open the remaining attempts fail instantly and
// locally. The breaker becomes the cap on retry amplification instead of a
// bystander to it.
//
// What makes that work is that resilience.IsRetryable returns false for
// ErrBreakerOpen. Without it the retry loop would treat an open breaker as
// a transient failure, back off, and retry into a breaker that is still
// open — spending the retry budget on calls that never leave the process.
type Resilience struct {
	// Target names the peer, for breaker keying. It comes from
	// configuration, never from a request.
	Target   string
	Retrier  *retry.Retrier
	Breakers *breaker.Group
}

// Interceptor returns the client interceptor.
func (r Resilience) Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			procedure := req.Spec().Procedure

			call := func(attemptCtx context.Context, attempt int) (connect.AnyResponse, error) {
				// Mark retries so the callee can decline to retry again
				// itself. This one header is the cheapest defence there is
				// against multi-hop amplification: it is what stops three
				// layers of threefold retries becoming 27x load.
				if v := retry.AttemptHeader(attempt); v != "" {
					req.Header().Set(retry.HeaderPreviousAttempts, v)
				}

				if r.Breakers == nil {
					return next(attemptCtx, req)
				}

				b := r.Breakers.Get(breaker.Key(r.Target, procedure))
				done, err := b.Allow()
				if err != nil {
					// Rejected locally. Nothing was sent, so there is
					// nothing to report to the breaker.
					return nil, err
				}
				resp, callErr := next(attemptCtx, req)
				done(callErr)
				return resp, callErr
			}

			if r.Retrier == nil {
				return call(ctx, 0)
			}

			var resp connect.AnyResponse
			err := r.Retrier.Do(ctx, procedure, func(attemptCtx context.Context, attempt int) error {
				var callErr error
				resp, callErr = call(attemptCtx, attempt)
				return callErr
			})
			if err != nil {
				// Do not return a partial response alongside an error: a
				// caller that checks err first is fine either way, but one
				// that checks resp first would read a response from a
				// failed attempt.
				return nil, err
			}
			return resp, nil
		}
	}
}

// BreakerOpen reports whether an error came from an open breaker, so
// callers can distinguish "the dependency is being protected from us" from
// "the dependency failed".
func BreakerOpen(err error) bool {
	return errors.Is(err, resilience.ErrBreakerOpen)
}
