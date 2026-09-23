// Package interceptor holds the framework's Connect interceptors and, more
// importantly, the order it puts them in.
//
// Ordering is the single most load-bearing decision in the framework, and
// it is where most hand-rolled setups are subtly wrong. Order is what
// decides whether your telemetry is truthful: get it wrong and the
// dashboards look healthy during the outage they exist to show you.
//
// These are ordinary connect.Interceptor values. The framework does not
// invent a middleware type, because connect.Interceptor already exists,
// already composes, and already works with every Connect handler in the
// ecosystem — inventing framework.Middleware would mean users could not
// reuse anything they already have, in either direction.
//
// ServerInterceptors is the framework's most reusable artifact. Someone who
// wants nothing else from this project can take that one slice and get a
// correctly ordered chain.
package interceptor

import (
	"connectrpc.com/connect"
)

// Chain returns the interceptors in the order given, as a Connect option.
//
// Connect applies interceptors outermost-first: the first in the slice is
// the first to see a request and the last to see the response.
func Chain(interceptors ...connect.Interceptor) connect.Option {
	return connect.WithInterceptors(interceptors...)
}

// ServerChain assembles the server-side chain in the framework's order.
//
// Outermost (first to see the request) to innermost (closest to the
// handler):
//
//  1. panic guard    catch panics in the interceptors themselves
//  2. tracing        extract propagation headers, start the server span
//  3. logging        attach a request logger carrying trace_id/span_id
//  4. metrics        measure everything inside, including rejections
//  5. load shed      reject early under overload, before expensive auth
//  6. timeout        enforce the server-side deadline
//  7. auth           authenticate and authorise
//  8. validation     check the request message
//  9. recovery       panic → CodeInternal, so 2-4 observe it correctly
//  10. ── handler ──
//
// The adjacencies that matter, and why:
//
//   - Tracing above logging. Log lines are useless in a distributed system
//     without a trace ID, so tracing must establish context before any
//     logger is built.
//   - Metrics above load shedding. If shedding sat outside metrics, shed
//     requests would be invisible and the dashboards would show a healthy
//     service throughout an overload incident.
//   - Load shedding above auth. Auth can be expensive — a JWT verification
//     with a JWKS fetch, or a call to an authz service. Under a flood you
//     want to reject before paying that.
//   - Recovery innermost. This is the counterintuitive one. Recovery
//     outermost converts a panic to an error *before* metrics, logging and
//     tracing see it, so they record a tidy error rather than a panic — or
//     never run their deferred code at all. Innermost means the panic
//     becomes a proper Internal error that propagates outward and is
//     recorded correctly at every layer.
//   - The outer panic guard exists because recovery-innermost cannot
//     protect against a panic in an interceptor. It does nothing but
//     recover, log, and return Internal.
type ServerChain struct {
	PanicGuard connect.Interceptor
	Tracing    connect.Interceptor
	Logging    connect.Interceptor
	Metrics    connect.Interceptor
	LoadShed   connect.Interceptor
	Timeout    connect.Interceptor
	Auth       connect.Interceptor
	Validation connect.Interceptor
	Recovery   connect.Interceptor

	// Extra is appended just before Recovery, so user interceptors run
	// inside the framework's observability and outside the handler. A user
	// interceptor that panics is caught by Recovery; one that is slow shows
	// up in the framework's own latency metric, which is what you want.
	Extra []connect.Interceptor
}

// Interceptors flattens the chain, skipping anything not configured.
func (c ServerChain) Interceptors() []connect.Interceptor {
	ordered := []connect.Interceptor{
		c.PanicGuard,
		c.Tracing,
		c.Logging,
		c.Metrics,
		c.LoadShed,
		c.Timeout,
		c.Auth,
		c.Validation,
	}
	out := make([]connect.Interceptor, 0, len(ordered)+len(c.Extra)+1)
	for _, i := range ordered {
		if i != nil {
			out = append(out, i)
		}
	}
	for _, i := range c.Extra {
		if i != nil {
			out = append(out, i)
		}
	}
	if c.Recovery != nil {
		out = append(out, c.Recovery)
	}
	return out
}

// ClientChain assembles the client-side chain.
//
//  1. tracing (logical)   one span for the whole logical call
//  2. metrics (logical)   one observation per logical call
//  3. deadline budget     per-attempt deadline from the remaining budget
//  4. retry               the loop
//     ├─ 5. breaker       per attempt; open → terminal, non-retryable
//     └─ 6. ── transport ──
//
// The breaker goes *inside* the retry loop, and that is worth stating
// explicitly because both orderings look defensible.
//
// Breaker outside retry: one logical call is one breaker observation, so
// three failing attempts look like a single failure. The breaker trips
// slowly while you send three times the traffic to a dying dependency.
//
// Breaker inside retry: each attempt is an observation, the breaker opens
// quickly, and once open the remaining attempts fail instantly at nearly no
// cost. The breaker becomes the natural cap on retry amplification rather
// than a bystander to it.
//
// The requirement that makes it work is that a breaker-open error must be
// non-retryable — see resilience.IsRetryable. Otherwise the retry loop
// treats it as transient, backs off, and retries into a breaker that is
// still open, burning the budget for nothing.
type ClientChain struct {
	Tracing    connect.Interceptor
	Metrics    connect.Interceptor
	Resilience connect.Interceptor // deadline budget + retry + breaker, in one
	Extra      []connect.Interceptor
}

// Interceptors flattens the client chain.
func (c ClientChain) Interceptors() []connect.Interceptor {
	ordered := []connect.Interceptor{c.Tracing, c.Metrics}
	out := make([]connect.Interceptor, 0, len(ordered)+len(c.Extra)+1)
	for _, i := range ordered {
		if i != nil {
			out = append(out, i)
		}
	}
	for _, i := range c.Extra {
		if i != nil {
			out = append(out, i)
		}
	}
	if c.Resilience != nil {
		out = append(out, c.Resilience)
	}
	return out
}
