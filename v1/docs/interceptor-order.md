# Why the chain is ordered the way it is

Ordering is the single most load-bearing decision in the framework. It is
also where most hand-rolled setups are subtly wrong, and the failure mode is
the worst kind: the service works, and the telemetry lies about it.

## The server chain

Outermost (first to see a request) to innermost (closest to the handler):

```
 1. panic guard    catch panics in the interceptors themselves
 2. tracing        extract propagation headers, start the server span
 3. logging        attach a request logger carrying trace_id/span_id
 4. metrics        measure everything inside, including rejections
 5. load shed      reject early under overload, before expensive auth
 6. timeout        enforce the server-side deadline
 7. auth           authenticate and authorise
 8. validation     check the request message
 9. recovery       panic → CodeInternal, so 2–4 observe it correctly
10. ── handler ──
```

`interceptor.ServerChain` builds this, and `TestChainRunsOutermostFirst`
asserts the exact sequence of entries and exits. A reordering that looks
harmless will fail that test.

### Tracing above logging

A log line without a trace ID is nearly useless in a distributed system: you
can see that something failed and not what else was part of the same
request. The span has to exist before any logger is constructed, so tracing
is above logging and not beside it.

### Metrics above load shedding

If shedding sat outside metrics, shed requests would never be counted. The
dashboard would show a service handling less traffic perfectly, during the
overload incident the dashboard exists to reveal. This is the single most
misleading arrangement available and it looks completely reasonable.

### Load shedding above auth

Auth can be expensive: a JWT verification with a JWKS fetch, or a call to an
authorisation service. Under a flood you want to reject before paying for
that, which means shedding has to be above it.

### Recovery innermost

The counterintuitive one, and the one people move.

Put recovery outermost and a panic becomes a tidy `Internal` error *before*
the tracing, logging and metrics interceptors run — so the span has no error
status, the log line has no stack, and the metric counts an ordinary
failure. Or worse, the panic unwinds past their deferred code entirely and
they record nothing at all.

Innermost, the panic becomes an error that propagates outward normally and
is recorded correctly at every layer.

`TestPanicIsObservedByOuterInterceptors` is the guard: it asserts that an
interceptor *above* recovery sees the panic as an error and that its
deferred code runs. If someone "fixes" the ordering, that test fails.

### Why there is also an outer panic guard

Recovery is innermost, so it cannot catch a panic in an interceptor above
it — including in the tracing or metrics interceptors themselves. The guard
does nothing but recover, log, and return `Internal`. It is deliberately
dumb: a guard that tried to record telemetry could panic again while
handling the first panic, and then the process really does go down.

### Where user interceptors go

Just above recovery. That way a user interceptor is measured by the
framework's latency metric and protected by its recovery — which is what
you want, and the opposite of what "add it to the end of the list" would
give you.

## The client chain

```
1. tracing (logical)   one span for the whole logical call
2. metrics (logical)   one observation per logical call
3. resilience          deadline budget → retry → breaker → transport
```

Tracing and metrics are per *logical* call, so they wrap the retry loop
rather than sitting inside it. Inside, one call the user made once would
produce three spans and three observations, and every latency percentile
would describe attempts rather than calls.

The deadline budget, the retry loop and the breaker are one interceptor
rather than three, because their nesting is not negotiable and three
separate interceptors could be reassembled wrongly.

### Why the breaker is inside the retry loop

Both orderings look defensible, which is why this is worth writing down.

**Breaker outside retry.** One logical call is one breaker observation.
Three failing attempts look like a single failure, so the breaker trips
slowly — and while it is deciding, you are sending three times the traffic
to a dependency that is already struggling.

**Breaker inside retry.** Every attempt is an observation. The breaker opens
quickly, and once open the remaining attempts fail instantly and locally.
The breaker becomes the cap on retry amplification instead of a bystander
to it.

The requirement that makes it work: **a breaker-open error must be
non-retryable.** `resilience.IsRetryable` returns false for
`ErrBreakerOpen`. Without that, the retry loop would treat an open breaker
as a transient failure, back off, and retry into a breaker that is still
open — spending the retry budget on calls that never leave the process.

## Taking just this

`ServerInterceptors()` and `ClientInterceptors(target)` are exported and
work against a server this framework did not build. Realistically they are
what people will take, and that is fine — a framework whose best-used part
is one function is still a framework that helped.
