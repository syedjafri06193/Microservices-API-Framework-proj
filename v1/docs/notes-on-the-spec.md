# Notes on the spec

Where this implementation departs from `Documentation/README.md`, and why.
Each entry says what the document specifies, what happened when it was
built, and what the code does instead.

The document is unusually good — most of it is right, and the parts that
matter most (interceptor ordering, breaker classification, retry budgets,
deadline budgets, the shutdown sequence) are right for the right reasons.
What follows is the residue.

---

## 1. `sony/gobreaker` and `failsafe-go` are listed, then not used

**The document** lists both under "what already exists", and the project's
stated philosophy is to compose existing libraries rather than build new
primitives.

**What this does** implements the breaker directly, and the reason is the
one the document itself supplies two sections later: §7.2, the failure
classification.

A breaker library takes a `ReadyToTrip(counts)` callback. What it does *not*
take is an opinion about which errors reach the counter at all — that is the
caller's job, and it is the decision the document calls "the most important
in the resilience layer". Wrapping a library would leave the important half
outside the library and the unimportant half inside it, and the resulting
type would be a thin shim around a counter.

The other half is the injectable `Clock`. §14.1's reference implementation
has one, §15.2 explains why, and it is not optional: without it every
breaker test needs real sleeps, the suite takes minutes, and it is flaky on
the loaded machine CI runs on. The breaker here is about 300 lines including
the group, and it makes both properties testable.

This is a deviation from the composition rule, so it is worth being explicit
that it is the *only* one. Connect, OTel, protobuf and the Prometheus
exporter are all used as-is.

---

## 2. Two bytes of config libraries for work the standard library does

**The document** (§11.1) names `caarlos0/env` and `go-playground/validator`.

**What this does** is a ~200-line reflective loader in `internal/config`,
with the same struct-tag shape, for the reason the document gives in the
same paragraph: *"Keep the list short. A framework's dependency tree becomes
every user's dependency tree, and every addition is supply-chain surface
plus a future upgrade obligation."*

Those two libraries bring six modules between them. The loader needs
`env`, `envDefault`, `oneof`, `gte` and `lte` — that is the whole surface
the framework's own config uses. The tags are deliberately identical, so
switching to the real libraries later is a matter of deleting a file.

The same reasoning applies more strongly to `protovalidate`, which the
document also names: it pulls in `cel-go`, which is large. The validation
interceptor takes an interface that `protovalidate.Validator` satisfies
directly, so a team that wants it writes two lines and pays for it, and a
team that validates in the handler pays nothing. See `dependencies.md`.

---

## 3. OTLP over HTTP, not gRPC

**The document's** `initTelemetry` uses `otlptracegrpc`.

**What this does** uses `otlptracehttp`. The gRPC exporter pulls `grpc-go`
and `genproto` into the tree of every service that uses the framework, for
a transport that carries only telemetry. The HTTP exporter speaks the same
OTLP protocol to the same collectors on port 4318.

The relevant document principle again: a framework's dependency tree is
every user's dependency tree.

---

## 4. The metrics interceptor must survive a cancelled context

Not a deviation, but a detail the document's sketch does not cover and that
is easy to get wrong.

`recordRPC` in §6.3 records against the request's context. When a client
cancels, that context is already done, and the OTel metrics SDK drops
measurements made with a cancelled context. The result is that a wave of
cancellations — precisely the thing you want to see on a dashboard —
produces no data at all.

The fix is one call:

```go
recordCtx := context.WithoutCancel(ctx)
m.duration.Record(recordCtx, ...)
```

The same reasoning as the document's own `context.WithoutCancel` on the
telemetry flush in §8.3, applied one layer down.

---

## 5. `IsRetryable` is narrower than `CountsAsFailure`

**The document** gives both functions (§5.3 and §7.2) without commenting on
the relationship between them.

They are deliberately not the same set, and the difference is worth stating:

| Code | Counts as a breaker failure | Retryable |
|---|---|---|
| `Unavailable` | yes | yes |
| `ResourceExhausted` | yes | yes |
| `DeadlineExceeded` | yes | yes, subject to the budget |
| `Internal` | **yes** | **no** |
| `InvalidArgument` | no | no |
| `Canceled` | no | no |

`Internal` is the interesting row. It is a genuine server fault and should
open a breaker — the dependency is broken. But it usually means the callee
hit a bug, and repeating the same request hits the same bug while adding
load. So it counts and is not retried.

---

## 6. `Unimplemented` and `Aborted` need classifying

**The document's** `countsAsFailure` lists eleven codes and falls through to
`return true` for the rest. Two of the omissions matter.

`Unimplemented` means the callee does not have this method — a version
skew, or a client calling something that was never deployed. Counting it
would let a client calling a method that does not exist trip the breaker for
every method that does. It is classified as a client fault here.

`Aborted` is a concurrency conflict, which says the caller should retry with
different state, not that the callee is unhealthy. Also a client fault.

The fallthrough is kept as `true`: an error we cannot classify is more
likely a broken dependency than a broken caller, so the breaker should
protect rather than ignore.

---

## 7. The retry budget must count logical calls, not attempts

**The document's** `RetryBudget.Allow` reads `b.requests.Sum()` for the
denominator, and §14.2's retrier calls `r.budget.RecordRequest()` at the
*end* of each loop iteration — so a call that makes three attempts records
three requests.

That inverts the budget. The denominator grows with retries, so a retry
storm keeps granting itself more budget, which is exactly the failure the
budget exists to prevent. With a ratio of 0.2 and attempts counted, three
failing attempts raise the allowance instead of exhausting it.

Here `RecordRequest` is called once per logical call, before the loop, and
there is a test (`TestBudgetCountsLogicalCallsNotAttempts`) that pins it.

---

## 8. The two-port rule needs an exception for port 0

A small thing, found by the tests. §4 requires the main and admin ports to
differ, for good reasons — `/metrics` and `pprof` on the public listener
means a heap profile is one unauthenticated request away.

Implemented as a literal string comparison, it rejects `MAIN_ADDR` and
`ADMIN_ADDR` both being `127.0.0.1:0`, which is what every test wants: port
0 asks the OS for an ephemeral port, so two listeners configured identically
still land on different ports. The check now exempts port 0.

---

## 9. Mesh detection warns; it does not disable

**The document** (§2.3) requires that the framework "detect a mesh sidecar
and warn loudly at startup if both layers are retrying", and it is right
that a single startup line is worth more than any amount of documentation.

Worth being explicit about what the code does *not* do: it never disables
retries on its own. Detection is heuristic — environment variables and a
loopback probe — and a false positive that silently removed the service's
resilience would be far worse than a false positive that printed a warning.
The operator gets the line and one environment variable.

---

## Things the document gets exactly right

Recorded because each one shaped the code and several are non-obvious:

- **Connect over grpc-gateway or cmux (§3).** The dual-mode feature really
  is done, and the honesty about the URL shape not being REST is the kind
  of thing that makes the rest of the document trustworthy.
- **Recovery innermost (§5.1).** Counterintuitive and correct. There is a
  test that asserts an outer interceptor observes a panicking handler as an
  error, which is the property that would silently disappear if someone
  "fixed" the ordering.
- **Breaker inside the retry loop (§5.3).** With the requirement that
  breaker-open be non-retryable, which is what turns the breaker into the
  cap on retry amplification rather than a bystander to it.
- **Client faults must not trip the breaker (§7.2).** The single most
  consequential decision in the project.
- **Retry only what the proto declares safe (§7.3), failing closed.** The
  strongest "opinionated" decision available, and it works: the
  `Charge` method in the example proto is unannotated, and there is a test
  that proves the framework calls it exactly once.
- **Deadline budgets with a fail-fast branch (§7.5).** Almost nothing
  implements this and it is the most valuable thing here under load.
- **Readiness must not check dependencies (§8.2).** The cascading-failure
  trap, explained better in the document than in most postmortems.
- **PreStopDelay is not optional (§8.3).** Endpoint removal is eventually
  consistent and there is no signal for it; you wait.
- **Telemetry flushes last (§8.3).** With `context.WithoutCancel`, or you
  lose the spans that explain the shutdown.
- **Cardinality as a closed type (§6.3).** A genuinely good argument for
  the project existing — it is a guarantee a framework can make and a
  library cannot.
- **Escape hatches are mandatory (§10).** And realistically
  `ServerInterceptors()` is what people will take. That is fine.

---

## The benchmark, honestly

§15.5 asks for the chain overhead, tracked in CI. It is there
(`BenchmarkChainOverhead`), with three rungs: bare Connect, the framework's
server chain, and both chains together.

One caveat that belongs next to the number rather than buried: the benchmark
drives a real loopback HTTP round trip, so the wire dominates and the
framework's own cost is inside the noise on a shared machine. The
allocations per operation are the reliable figure — roughly 65 extra on the
server side — and the nanoseconds should be read as "not measurably worse"
rather than as a precise overhead. A microbenchmark that invoked the chain
directly would give a cleaner number for the chain and a less honest answer
to the question a user is actually asking.
