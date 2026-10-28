# Resilience

Four mechanisms, each of which makes things worse if you get it wrong.

## Circuit breaker

**Per-method, not per-service.** Keyed on `(target, procedure)`. One slow
method should not black-hole every other method on the same target.

**Trips on a failure ratio over a minimum volume.** An absolute threshold
makes a method serving 5 rps behave completely differently from one serving
5000 rps. The volume gate is what stops one failure out of two requests on a
quiet method from tripping it.

### What counts as a failure

The most consequential decision in the layer, and getting it wrong is
actively harmful rather than merely useless.

If the breaker counted every error, one caller sending malformed requests
would trip it and make a perfectly healthy service unavailable to everybody
else — and the service's own dashboards would show it failing.

| Counted (server faults) | Not counted (client faults) |
|---|---|
| `Unavailable` | `InvalidArgument` |
| `DeadlineExceeded` | `NotFound` |
| `ResourceExhausted` | `AlreadyExists` |
| `Internal` | `PermissionDenied` |
| `DataLoss` | `Unauthenticated` |
| `Unknown` | `FailedPrecondition` |
| anything unrecognised | `OutOfRange` |
| | `Aborted`, `Unimplemented` |
| | `Canceled` |

`Canceled` deserves its own note. A client cancelling because a user closed
a browser tab is not evidence that a downstream service is sick. Counting
cancellations means a spike in abandoned requests trips your breakers, and
this catches people out surprisingly often.

Unrecognised codes count. An error we cannot classify is more likely a
broken dependency than a broken caller, so the breaker should protect rather
than ignore.

### A note on how fast it trips

The ratio is evaluated on every call against the window so far, not against
the eventual average. A bursty failure pattern that averages 40% can cross
50% early and trip a breaker configured at 0.5. That is intended — a breaker
waiting for a long-run average would be useless at reacting to a burst — but
"average failure rate is below the threshold" is not the same claim as
"will not trip". `TestBurstyFailuresCanTripBelowTheAverageRatio` pins it.

## Retries

**Only what the proto declares safe.** Read from the descriptor at runtime:

```protobuf
rpc GetPayment(...) returns (...) { option idempotency_level = NO_SIDE_EFFECTS; }
rpc CancelPayment(...) returns (...) { option idempotency_level = IDEMPOTENT; }
rpc ChargeCard(...) returns (...);   // unannotated: never retried
```

**Fail closed.** An unannotated method is never retried, and neither is one
whose descriptor cannot be resolved. That makes the safe thing the default
and turns enabling retries into an explicit, reviewable change to the
service's published contract rather than a line in a config file nobody
reviews.

**Full jitter.** Sleep uniformly in `[0, backoff)` rather than
`backoff/2 + rand(backoff/2)`. Strictly better at desynchronising clients,
which is the entire reason backoff exists.

**Retries are marked.** Every retry carries `grpc-previous-rpc-attempts`, so
a downstream service can see a request is already a retry and decline to
retry its own dependencies. This one header is the cheapest defence there is
against multi-hop amplification.

## Retry budgets

Backoff bounds *timing*, not *volume*. During a partial outage every client
retrying three times means the struggling dependency receives three times
the traffic exactly when it can least handle it.

The budget caps retries as a fraction of total request volume. With a 20%
budget a total outage produces at most 1.2x load instead of 3x, and that
difference is often the difference between a degraded dependency and a dead
one.

Two details that are easy to get wrong:

- **The denominator counts logical calls, not attempts.** If attempts
  counted, a retry storm would keep granting itself more budget — the exact
  failure the budget exists to prevent.
- **There is a floor.** Without `MinPerSecond`, a service handling two
  requests per second has a budget of 0.4 retries and can never retry at
  all, which is the case where a retry is cheapest and most likely to help.

## Deadline budgets

A client sets a 1s deadline. Service A spends 300ms, then calls B with a
fresh 1s timeout. B spends 800ms and succeeds — but A's caller gave up 100ms
ago. All of B's work was wasted, and B never knew.

Each hop reserves a `Buffer` for its own processing and the response trip,
and passes the remainder down. The deadline strictly decreases across hops.

**The fail-fast branch is the valuable part.** When less than `Minimum`
remains, the call is not made at all. Under load, a system that declines to
start doomed work recovers; one that starts it anyway spends all its
capacity on requests nobody is waiting for. Almost no in-house framework
implements this.

A request with no deadline is passed through unchanged rather than given
one — inventing a deadline would silently cap call paths the operator never
configured, and the resulting timeouts would be blamed on the dependency.
The missing deadline is logged instead, because an unbounded call path is
worth knowing about.

## Load shedding

Shed on **queue latency**, not CPU. Time spent waiting to be handled is a
direct measure of whether the service is keeping up, it needs no
platform-specific instrumentation, and it stays correct when the bottleneck
is a lock or a connection pool rather than the processor.

Rejections return `ResourceExhausted`, never `Unavailable`.
`ResourceExhausted` is retryable with backoff and is counted by the breaker,
which is the behaviour you want: the caller should back off and the breaker
should notice the dependency is saturated. `Unavailable` would suggest the
service is gone.

Queueing instead of rejecting produces the classic death spiral: the queue
grows, latency grows, clients time out and retry, the queue grows faster.

## Turning it all off

Every mechanism has a flag, because in a mesh deployment the sidecar already
does this and doing it twice is worse than not doing it at all. See
`service-mesh.md`.
