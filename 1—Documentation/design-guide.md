# Microservices API Framework — Design & Build Guide

**Project:** Opinionated Go framework for observable microservices — tracing, circuit breaking, gRPC/REST dual-mode routing
**Language:** Go
**Status of this document:** planning + reference

---

## Table of contents

1. [Executive summary and scope](#1-executive-summary-and-scope)
2. [Reality check](#2-reality-check)
3. [The dual-mode routing decision](#3-the-dual-mode-routing-decision)
4. [System architecture](#4-system-architecture)
5. [The interceptor chain](#5-the-interceptor-chain)
6. [Observability](#6-observability)
7. [Resilience](#7-resilience)
8. [Lifecycle: startup, health, shutdown](#8-lifecycle-startup-health-shutdown)
9. [Configuration](#9-configuration)
10. [The public API](#10-the-public-api)
11. [Tech stack and setup](#11-tech-stack-and-setup)
12. [Repository layout](#12-repository-layout)
13. [Milestone ladder](#13-milestone-ladder)
14. [Reference implementations](#14-reference-implementations)
15. [Testing strategy](#15-testing-strategy)
16. [API stability and versioning](#16-api-stability-and-versioning)
17. [Stretch goals](#17-stretch-goals)
18. [References](#18-references)

---

## 1. Executive summary and scope

### The original statement

> Opinionated Go framework for building observable microservices with tracing, circuit-breaking, and gRPC/REST dual-mode routing.

Three findings reshape this before you write any code:

1. **Connect (connectrpc) already solves dual-mode routing, and solves it better than you will.** A single `http.Handler`, built on `net/http`, serves gRPC, gRPC-Web, and a plain HTTP/JSON protocol on one port, with full gRPC compatibility including streaming, trailers, and error details. Connect clients work with any gRPC server; Connect handlers work with any gRPC client. If dual-mode routing is your headline feature, it's done. See section 3.
2. **Go frameworks have a graveyard, and it's cultural, not technical.** Go's ecosystem rewards small composable libraries and punishes frameworks. go-micro, Jupiter, rpcx, Sponge, go-zero, Kratos, go-kit — many are unmaintained or barely moving. The things that thrive are libraries: `grpc-go`, `connect-go`, `chi`, `otel-go`, `grpc-middleware`. A search for Go microservice frameworks returns dozens of projects whose most recent commits are from 2024. See section 2.2.
3. **A naive resilience layer makes outages worse, not better.** Stacking in-process retries on top of a service mesh that also retries produces multiplicative amplification — three layers each retrying three times turns one failure into 27× load. And a circuit breaker that trips on `InvalidArgument` will take down a healthy service because one caller has a bug. See section 7.

### Revised project statement

> An opinionated wiring layer over `connect-go` and OpenTelemetry that gives a Go service correct-by-default observability, resilience, and lifecycle handling in one function call — encoding the 800 lines of boilerplate every team copy-pastes between services, with the subtle parts (interceptor ordering, deadline budgets, retry budgets, breaker error classification, shutdown sequencing) done right once.

The reframe matters. "Another Go microservices framework" is a crowded, unpromising category. "The wiring layer over Connect + OTel, with opinions" is narrow, useful, and honest about what it's built on.

### What "opinionated" has to mean

A framework with two hundred configuration options is not opinionated; it's a worse version of assembling the libraries yourself. Opinionated means **the decisions are made and you can't easily unmake them**:

- Protobuf is the source of truth. There is no non-proto path.
- OpenTelemetry is the telemetry system. There is no pluggable telemetry interface.
- Structured logs are `log/slog` JSON with trace correlation. There is no logger interface.
- Retries only happen on methods the proto declares side-effect-free.
- Metric labels come from route templates. Raw paths are impossible to emit.
- Shutdown sequence is fixed.

The test: if someone can configure their way into a broken setup, the framework isn't opinionated enough.

### Explicit non-goals

- **Not a service mesh.** No mTLS, no sidecar, no L7 traffic policy.
- **Not a service registry.** Use DNS, Kubernetes Services, or Consul. Building discovery is how these projects balloon.
- **Not an ORM or data layer.** The framework should have no opinion about your database.
- **Not a code generator beyond protobuf.** `buf` generates from `.proto`. You add nothing.
- **Not a replacement for `net/http`.** Users must be able to drop down to a raw `http.Handler` at any point. A framework you can't escape is a framework nobody adopts.

---

## 2. Reality check

### 2.1 What already exists

| Project | What it is | Overlap |
|---|---|---|
| **connect-go** (Buf) | One server handling REST/JSON, gRPC, gRPC-Web, and Connect clients. Built on `net/http`. One package. | **Your dual-mode feature, complete** |
| **grpc-gateway** | Codegen'd reverse proxy translating JSON→gRPC from proto annotations | Your dual-mode feature, the older way |
| **grpc-go** | The reference gRPC implementation | The transport you'd otherwise build on |
| **go-grpc-middleware** | Interceptor chaining, auth, logging, retries | A large fraction of your interceptor work |
| **otel-go** + contrib instrumentation | Tracing, metrics, logs, propagation, semantic conventions | All of your observability |
| **sony/gobreaker**, `failsafe-go` | Circuit breaking | Your resilience primitives |
| **Kratos** (Bilibili) | Protobuf-first Go microservices framework | The whole project |
| **go-kit** | The original Go microservices toolkit; widely considered heavy and boilerplate-y | The whole project |
| **go-zero** | Codegen-heavy framework | The whole project |
| **Encore** | Go framework with infrastructure inference | The whole project, plus more |
| **Istio / Linkerd** | Retries, timeouts, circuit breaking, mTLS at the sidecar | Your resilience layer, at a different layer |

**The honest conclusion:** virtually every individual capability in the project statement exists in a mature library. What does *not* exist in one place is the correct wiring of all of them — and that is a real gap, because every Go team writes it, most write it subtly wrong, and none of them get to reuse it.

That is what you should build. Not new primitives. Correct composition of existing ones, with the subtleties documented.

### 2.2 Why Go frameworks fail

Worth internalizing before you spend six months on this:

- **Go culture prefers libraries.** "A little copying is better than a little dependency" is idiomatic. A framework asks for a large dependency and an inversion of control, both of which the community resists.
- **Frameworks calcify.** Once someone builds a service on yours, every change is a breaking change. Libraries can be swapped; frameworks cannot.
- **The escape hatch determines adoption.** Teams adopt tools they can abandon incrementally. If your framework owns `main()` and there's no way to get at the raw `http.Handler`, nobody will risk it.
- **Maintenance burden is the real cost.** The graveyard is full of projects that were good and then got abandoned because one person couldn't keep up with gRPC, OTel, and Go release churn.

**Implication for design:** build it as a *set of composable packages with a convenience constructor on top*, not a monolith. Someone should be able to use just your interceptor chain, or just your shutdown sequencer, without the rest. The `service.New()` entry point is a convenience, not the only door.

**Implication for framing:** the realistic goal is not adoption. It's a reference implementation that demonstrates you understand distributed systems failure modes deeply. Say that plainly in the README.

### 2.3 The service mesh overlap

If the target runs on Kubernetes with Istio or Linkerd, the sidecar already does retries, timeouts, circuit breaking, mTLS, and traffic-level metrics. Duplicating that in-process is not just redundant — it's actively dangerous:

| Hazard | Mechanism |
|---|---|
| **Retry amplification** | App retries 3×, mesh retries 3× → 9× load per logical call, per hop. Across three service hops, 27×. A minor blip becomes a cascading failure. |
| **Deadline confusion** | Both layers enforce timeouts with different values; you get inconsistent, hard-to-debug cancellation. |
| **Double-counted metrics** | Mesh RPS and app RPS disagree; nobody knows which to trust. |
| **Conflicting breakers** | Mesh ejects an endpoint while the app breaker is closed, or vice versa. |

**What in-process resilience is still good for**, and what the framework should lean into:

- Non-mesh deployments (VMs, ECS, Cloud Run, Nomad, local dev)
- **Semantic failures the mesh cannot see** — a `200 OK` containing `{"error": ...}`, a slow-but-successful response, a partial result
- Per-method policy, where the mesh only sees paths
- Deadline *budget* math, which no mesh does

**Design requirement:** every resilience feature must be switchable off with one flag, and the framework must detect a mesh sidecar and warn loudly at startup if both layers are retrying. A single startup log line — "retries enabled in-process AND a mesh sidecar was detected; this may amplify load" — is worth more than any amount of documentation.

### 2.4 The failure modes a naive framework introduces

These are the ones worth building the project around, because they're where real systems break.

| Naive behavior | Consequence | Correct behavior |
|---|---|---|
| Breaker counts all errors | One caller sending bad input trips the breaker; healthy service goes dark for everyone | Only count *server-fault* errors (§7.2) |
| Retry everything | Duplicate payments, duplicate emails | Retry only proto-declared side-effect-free methods (§7.3) |
| Unbounded retries | Retry storm during partial outage | Retry budget as a fraction of total traffic (§7.4) |
| Timeout per-hop, not per-request | 5 hops × 1s = 5s, but the client gave up at 2s; 3s of work is wasted on a dead request | Deadline budget propagation (§7.5) |
| Path as a metric label | `/users/12345` × millions → cardinality explosion → Prometheus OOM | Route template labels only, enforced by types (§6.3) |
| Readiness probe checks the database | DB blips → every replica marked unready → total outage from a partial degradation | Readiness reflects *this instance's* ability to serve (§8.2) |
| Shutdown closes the server, then flushes traces | Spans for in-flight requests lost | Fixed shutdown sequence (§8.3) |
| Exit on SIGTERM | Kubernetes still routes traffic for seconds after SIGTERM | preStop delay → fail readiness → drain (§8.3) |

---

## 3. The dual-mode routing decision

### 3.1 The three options

**Option A — grpc-gateway.** Annotate protos with `google.api.http`, generate a reverse proxy, run it alongside the gRPC server.

| | |
|---|---|
| Pros | Mature, widely deployed, generates OpenAPI, gives genuinely RESTful URLs (`GET /v1/users/{id}`) |
| Cons | Two servers, or an in-process mux. Double serialization (JSON → proto → gRPC → proto → JSON). Extra codegen step. Streaming over HTTP/1.1 is awkward. Errors translate lossily. |

**Option B — Connect (recommended).** One `http.Handler` speaking gRPC, gRPC-Web, and Connect's HTTP/JSON protocol.

| | |
|---|---|
| Pros | One server, one port, no proxy, no protocol sniffing. Plain `net/http`, so every piece of HTTP middleware in Go works. `curl` works out of the box. Full gRPC wire compatibility both directions. Streaming works over HTTP/1.1. |
| Cons | URLs are RPC-shaped (`POST /package.Service/Method`), not RESTful. If you need `GET /v1/users/{id}` for a public API, this isn't it. |

**Option C — hand-rolled dual servers with `cmux`.** Sniff the protocol on a single port and route to a gRPC server or an HTTP server.

| | |
|---|---|
| Verdict | **Don't.** See 3.2. |

### 3.2 Why protocol sniffing is a trap

`cmux` works by peeking at initial bytes to guess the protocol. It breaks in ways that are painful to debug:

- **HTTP/2 prior knowledge** (h2c) starts with a preface that looks different from a TLS handshake, which looks different from HTTP/1.1. Three cases, and the matcher order matters.
- **TLS ALPN** is supposed to negotiate `h2` vs `http/1.1` at the TLS layer. Sniffing after TLS termination loses that; sniffing before it can't see anything.
- **Load balancers interfere.** Many L7 balancers terminate HTTP/2 and re-originate HTTP/1.1, so what your sniffer sees isn't what the client sent.
- **The failure mode is a hang**, not an error. A mismatched matcher leaves the connection waiting for bytes that never come.

Connect sidesteps all of this because everything is HTTP — gRPC is `POST` with `application/grpc` content type, Connect-JSON is `POST` with `application/json`, both to the same path. Routing is a content-type check inside one handler, not a byte-level guess on a raw socket.

### 3.3 The recommendation

**Build on Connect. Offer grpc-gateway as an optional add-on for teams that need RESTful URLs.**

```go
// This is the whole dual-mode story.
mux := http.NewServeMux()
path, handler := userv1connect.NewUserServiceHandler(
    svc,
    connect.WithInterceptors(fw.ServerInterceptors()...),
)
mux.Handle(path, handler)

// h2c so HTTP/2 works without TLS (behind a mesh or LB that terminates it).
srv := &http.Server{
    Handler: h2c.NewHandler(mux, &http2.Server{}),
}
```

That handler now serves:

```bash
# gRPC client
grpcurl -plaintext localhost:8080 user.v1.UserService/GetUser

# Connect JSON — no client library, no proxy
curl -X POST localhost:8080/user.v1.UserService/GetUser \
  -H 'Content-Type: application/json' -d '{"id":"123"}'

# gRPC-Web from a browser, same port
```

**Be honest in the docs about the URL shape.** `POST /user.v1.UserService/GetUser` is not REST. If someone needs a public API with resource URLs and proper verbs, they want grpc-gateway or a hand-written HTTP layer. Framing Connect's protocol as "REST" is the kind of overclaim that erodes trust in everything else the project says.

---

## 4. System architecture

```
┌───────────────────────────────────────────────────────────────┐
│ main.go — the user's code, ~20 lines                          │
│                                                               │
│   svc := service.New("user-api",                              │
│       service.WithConfig(cfg),                                │
│       service.WithHandler(userv1connect.NewUserServiceHandler)│
│   )                                                           │
│   svc.Run(ctx)                                                │
└───────────────────────────┬───────────────────────────────────┘
                            │
┌───────────────────────────▼───────────────────────────────────┐
│ service — lifecycle orchestration                             │
│   config load → validate → telemetry init → handler register  │
│   → listeners up → readiness on → block → signal → drain      │
└───┬──────────────┬──────────────┬──────────────┬──────────────┘
    │              │              │              │
┌───▼──────┐ ┌─────▼──────┐ ┌─────▼──────┐ ┌────▼──────────────┐
│ transport│ │ telemetry  │ │ resilience │ │ lifecycle         │
│ ──────── │ │ ────────── │ │ ────────── │ │ ─────────────     │
│ connect  │ │ tracing    │ │ breaker    │ │ signals           │
│ h2c      │ │ metrics    │ │ retry      │ │ readiness/health  │
│ admin    │ │ slog       │ │ budget     │ │ drain sequencer   │
│ mux      │ │ propagate  │ │ deadline   │ │ background tasks  │
└──────────┘ └────────────┘ └────────────┘ └───────────────────┘
    │              │              │              │
┌───▼──────────────▼──────────────▼──────────────▼──────────────┐
│ interceptor — the ordered chain (server and client)            │
└────────────────────────────────────────────────────────────────┘
    │
┌───▼────────────────────────────────────────────────────────────┐
│ Underlying libraries: connect-go · otel-go · net/http · slog    │
│ The framework configures these. It does not abstract them.      │
└─────────────────────────────────────────────────────────────────┘
```

### Two ports, always

| Port | Serves | Exposed |
|---|---|---|
| **8080** (main) | Connect/gRPC/gRPC-Web handlers | Yes — to clients and the mesh |
| **9090** (admin) | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof`, `/debug/vars` | No — cluster-internal only |

Separating them is not cosmetic. Putting `/metrics` and `pprof` on the public port means a heap profile is one unauthenticated request away, and it means load shedding on the main port can take down your ability to observe the incident. The admin port must stay responsive when the main port is saturated — which means **separate `http.Server` instances with separate goroutine pools**, not two routes on one mux.

---

## 5. The interceptor chain

Ordering is the single most load-bearing design decision in the framework, and it's where most hand-rolled setups are subtly wrong. Order determines whether your telemetry is truthful.

### 5.1 Server chain

Outermost (first to see the request) → innermost (closest to the handler):

```
 1. panic guard        catch panics in the interceptors themselves; process safety only
 2. tracing            extract propagation headers, start the server span
 3. logging            attach a request logger carrying trace_id/span_id from (2)
 4. metrics            measure everything inside, including rejections below
 5. load shed          reject early under overload — before expensive auth
 6. timeout            enforce the server-side deadline
 7. auth               authenticate/authorize
 8. validation         protovalidate on the request message
 9. recovery           panic → connect.CodeInternal, so (2)-(4) observe it correctly
10. ── handler ──
```

The reasoning behind each adjacency:

- **Tracing above logging.** Log lines are useless in a distributed system without a trace ID. Tracing must establish context before any logger is constructed.
- **Metrics above load shedding.** If shedding sits outside metrics, shed requests are invisible, and your dashboards will show a healthy service during an overload incident.
- **Load shedding above auth.** Auth can be expensive — a JWT verification with a JWKS fetch, or a call to an authz service. Under a traffic flood you want to reject before paying that cost.
- **Recovery innermost.** This is the counterintuitive one. If recovery is outermost, a panic is converted to an error *before* the metrics, logging, and tracing interceptors see it, so they record a clean error rather than a panic — or worse, never run their deferred code. Innermost means the panic becomes a proper `Internal` error that propagates outward and gets recorded correctly at every layer.
- **The outer panic guard exists** because recovery-innermost doesn't protect against a panic in an interceptor. It should do nothing but recover, log at `ERROR`, and return `Internal`.

### 5.2 Client chain

```
 1. tracing (logical)   one span for the whole logical call, spanning all attempts
 2. metrics (logical)   one observation per logical call
 3. deadline budget     compute the per-attempt deadline from remaining budget
 4. retry               the loop
    ├─ 5. tracing (attempt)   a child span per attempt
    ├─ 6. metrics (attempt)   per-attempt counters
    ├─ 7. circuit breaker     per-attempt; open → terminal, non-retryable
    └─ 8. ── transport ──
```

### 5.3 Why the breaker goes *inside* the retry loop

This is worth thinking through, because both orderings look defensible.

**Breaker outside retry:** one logical call produces one breaker observation. Three failing retries look like a single failure. The breaker trips slowly, and meanwhile you're sending 3× traffic to a dying dependency. Wrong.

**Breaker inside retry (correct):** each attempt is a breaker observation. The breaker opens quickly, and once open, remaining retry attempts fail instantly at near-zero cost. **The breaker becomes the natural cap on retry amplification** rather than a bystander to it.

The requirement that makes this work: **a breaker-open error must be classified non-retryable.** Otherwise the retry loop treats it as a transient failure, backs off, and retries into a breaker that's still open — burning the retry budget for nothing.

```go
func isRetryable(err error) bool {
    if errors.Is(err, ErrBreakerOpen) {
        return false   // retrying an open breaker is pure waste
    }
    switch connect.CodeOf(err) {
    case connect.CodeUnavailable, connect.CodeResourceExhausted:
        return true
    case connect.CodeDeadlineExceeded:
        // Only if budget remains; the budget interceptor decides.
        return true
    default:
        return false
    }
}
```

### 5.4 Interceptors are ordinary Connect interceptors

Do not invent a middleware type. `connect.Interceptor` already exists, composes, and works with every Connect handler in the ecosystem. Inventing `framework.Middleware` means users can't reuse anything.

```go
// ServerInterceptors returns the chain in the correct order.
// Exported so users can take just this and skip the rest of the framework.
func (f *Framework) ServerInterceptors() []connect.Interceptor {
    return []connect.Interceptor{
        f.panicGuard(),
        otelconnect.NewInterceptor(...),
        f.logging(),
        f.metrics(),
        f.loadShed(),
        f.timeout(),
        f.auth(),
        f.validate(),
        f.recovery(),
    }
}
```

That single exported method is the framework's most reusable artifact. Someone who wants nothing else can still take a correctly-ordered chain.

---

## 6. Observability

### 6.1 Configure OpenTelemetry; do not abstract it

The most common framework mistake is wrapping OTel in a house interface "in case we switch later." You will not switch, and the wrapper costs you:

- **Semantic conventions.** OTel has standardized attribute names (`rpc.system`, `rpc.method`, `server.address`). Backends build dashboards on them. A custom wrapper emits `service` and `method` instead, and every off-the-shelf dashboard breaks.
- **Context propagation subtleties.** W3C `traceparent`, baggage, and span links have edge cases the OTel SDK handles and your wrapper won't.
- **The contrib ecosystem.** `otelhttp`, `otelconnect`, `otelsql`, `otelgrpc` all emit spans into the OTel context directly. A wrapper cannot see them, so your database spans become orphans.

**The framework's job is to configure OTel correctly and get out of the way.** Users import `go.opentelemetry.io/otel` directly for custom spans. That's a feature: it means their code isn't coupled to your framework.

```go
func initTelemetry(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
    res, err := resource.New(ctx,
        resource.WithFromEnv(),
        resource.WithTelemetrySDK(),
        resource.WithAttributes(
            semconv.ServiceName(cfg.ServiceName),
            semconv.ServiceVersion(cfg.Version),
            semconv.DeploymentEnvironmentName(cfg.Environment),
        ),
    )
    if err != nil {
        return nil, err
    }

    exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint))
    if err != nil {
        return nil, err
    }

    tp := trace.NewTracerProvider(
        trace.WithResource(res),
        trace.WithBatcher(exp),
        trace.WithSampler(trace.ParentBased(
            trace.TraceIDRatioBased(cfg.SampleRatio),
        )),
    )
    otel.SetTracerProvider(tp)

    // W3C trace context + baggage. Add B3 only if you must interoperate
    // with an older fleet — every extra propagator is more header parsing
    // on every request.
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{},
        propagation.Baggage{},
    ))

    return tp.Shutdown, nil
}
```

`ParentBased(TraceIDRatioBased(...))` is the right default: honor the caller's sampling decision when there is one, so a trace is never half-sampled across services, and only make an independent decision at the edge.

### 6.2 Correlating logs with traces

A log line without a trace ID is nearly useless in a distributed system. Make correlation automatic and impossible to forget:

```go
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
    if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
        r.AddAttrs(
            slog.String("trace_id", sc.TraceID().String()),
            slog.String("span_id", sc.SpanID().String()),
        )
    }
    return h.Handler.Handle(ctx, r)
}
```

Then require `slog.InfoContext(ctx, ...)` rather than `slog.Info(...)`. Add a linter rule for it — `sloglint` can enforce context-passing variants — because the non-context form silently drops correlation and nobody notices until an incident.

### 6.3 Cardinality: the thing that kills metrics backends

A Prometheus time series exists for every unique label combination. A `path` label containing `/users/12345` produces one series per user. At a million users that's a million series, and your Prometheus dies.

Make it structurally impossible:

```go
// Label values are a closed type. There is no way to pass a raw string.
type MethodLabel struct{ value string }

// FromProcedure takes the Connect procedure name, which is always the
// static template "/package.Service/Method" — never a user-supplied path.
func FromProcedure(p string) MethodLabel { return MethodLabel{value: p} }

func (f *Framework) recordRPC(m MethodLabel, code connect.Code, d time.Duration) {
    f.rpcDuration.Record(ctx, d.Seconds(),
        metric.WithAttributes(
            attribute.String("rpc.method", m.value),
            attribute.String("rpc.code", code.String()),
        ),
    )
}
```

Making `MethodLabel` an unexported-field struct means a user *cannot* pass an arbitrary string, even by accident. This is the kind of thing a framework can do that a library can't, and it's a genuinely good argument for the project existing.

Safe label sets, for the docs:

| Safe | Never |
|---|---|
| RPC method (static from proto) | Raw URL path |
| Status code | User ID, tenant ID, request ID |
| Service name | Error message text |
| Environment / region | Timestamps |
| Client name (from a closed allowlist) | Arbitrary headers |

Ship a cardinality check in the admin endpoint: count active series per metric and log a warning above a threshold. Catching this in staging is worth a lot.

### 6.4 Exemplars

Link metrics to traces so a latency spike on a dashboard is one click from an example trace of a slow request. This is a small amount of work with a large payoff during incidents, and it's a differentiator over hand-rolled setups, which almost never bother.

---

## 7. Resilience

### 7.1 Circuit breaker design

**Per-method, not per-service.** One slow method should not black-hole every other method on the same target. Key the breaker on `(target, procedure)`.

**Trip on a failure *ratio* over a minimum request volume**, not an absolute count. An absolute threshold means a method serving 5 rps behaves completely differently from one serving 5000 rps.

```go
type BreakerConfig struct {
    // Don't evaluate until we've seen this many requests in the window.
    // Prevents a single failure on a low-traffic method from tripping.
    MinimumRequests  int            // default 20
    FailureRatio     float64        // default 0.5
    Window           time.Duration  // default 10s, sliding
    OpenDuration     time.Duration  // default 5s before probing
    HalfOpenMaxCalls int            // default 3 concurrent probes
    HalfOpenSuccesses int           // default 3 to close
}
```

States:

```
        ┌──────────┐  failures/total ≥ ratio
        │  CLOSED  │───────────────────────────┐
        └────▲─────┘  (and total ≥ minimum)    │
             │                                  ▼
  N consecutive successes              ┌────────────┐
             │                         │    OPEN    │
        ┌────┴──────┐  any failure     └──────┬─────┘
        │ HALF_OPEN │◀────────────────────────┘
        └───────────┘   after OpenDuration
              │          (limited probes)
              └─ any failure → back to OPEN
```

### 7.2 What counts as a failure ★

**This is the most important decision in the resilience layer, and getting it wrong is actively harmful.**

If the breaker counts every error, then a single caller sending malformed requests will trip the breaker and make a perfectly healthy service unavailable to everyone else. The breaker must distinguish *server faults* from *client faults*.

```go
func countsAsFailure(err error) bool {
    if err == nil {
        return false
    }
    switch connect.CodeOf(err) {

    // Server faults — the dependency is unhealthy. Count these.
    case connect.CodeUnavailable,      // can't reach it
         connect.CodeDeadlineExceeded, // too slow
         connect.CodeResourceExhausted,// overloaded
         connect.CodeInternal,         // it broke
         connect.CodeDataLoss,
         connect.CodeUnknown:
        return true

    // Client faults — the dependency is fine, the CALLER is wrong.
    // Counting these lets one buggy client take down a healthy service.
    case connect.CodeInvalidArgument,
         connect.CodeNotFound,
         connect.CodeAlreadyExists,
         connect.CodePermissionDenied,
         connect.CodeUnauthenticated,
         connect.CodeFailedPrecondition,
         connect.CodeOutOfRange:
        return false

    // Ambiguous — the caller gave up, which says nothing about the server.
    case connect.CodeCanceled:
        return false
    }
    return true
}
```

`CodeCanceled` deserves its own note: a client cancelling because the *user* closed the browser tab is not evidence that the downstream service is sick. Counting cancellations means a spike in user-abandoned requests trips your breakers. This catches people out surprisingly often.

### 7.3 Retries: only what the proto declares safe

Retrying a non-idempotent method is how you get duplicate charges. Rather than trusting a config file or a comment, read it from protobuf, which already has the field:

```protobuf
service PaymentService {
  // Safe to retry — no side effects at all.
  rpc GetPayment(GetPaymentRequest) returns (Payment) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // Safe to retry — repeating it produces the same state.
  rpc CancelPayment(CancelPaymentRequest) returns (Payment) {
    option idempotency_level = IDEMPOTENT;
  }

  // NOT annotated → framework will never auto-retry this.
  rpc ChargeCard(ChargeCardRequest) returns (Payment);
}
```

```go
// Read at registration time from the proto descriptor, not at call time.
func retryable(md protoreflect.MethodDescriptor) bool {
    opts, ok := md.Options().(*descriptorpb.MethodOptions)
    if !ok || opts.IdempotencyLevel == nil {
        return false   // fail closed — unannotated means unsafe
    }
    switch opts.GetIdempotencyLevel() {
    case descriptorpb.MethodOptions_NO_SIDE_EFFECTS,
         descriptorpb.MethodOptions_IDEMPOTENT:
        return true
    }
    return false
}
```

**Fail closed.** An unannotated method is never retried. This makes the safe thing the default and forces an explicit, reviewable decision to enable retries — which is exactly the property you want on a payments API.

This is one of the strongest "opinionated" decisions available, and it's the kind of thing a reviewer will notice.

### 7.4 Retry budgets

Exponential backoff with jitter bounds *timing*, not *volume*. During a partial outage, every client retrying 3× means the struggling dependency receives 3× traffic precisely when it can least handle it. Backoff doesn't prevent this; it just spreads it slightly.

The fix (as used by Linkerd and gRPC's retry design): **cap retries as a fraction of total request volume.**

```go
type RetryBudget struct {
    mu       sync.Mutex
    ratio    float64        // e.g. 0.2 → retries ≤ 20% of total traffic
    minRetriesPerSec float64 // floor, so low-traffic services can still retry
    window   time.Duration
    requests *slidingCounter
    retries  *slidingCounter
}

func (b *RetryBudget) Allow() bool {
    b.mu.Lock()
    defer b.mu.Unlock()

    total := b.requests.Sum()
    used  := b.retries.Sum()

    allowed := total*b.ratio + b.minRetriesPerSec*b.window.Seconds()
    if used >= allowed {
        return false
    }
    b.retries.Add(1)
    return true
}
```

With a 20% budget, a total outage produces at most 1.2× load instead of 3×. That difference is often the difference between a degraded dependency and a dead one.

**Also mark retried requests.** Set a header (gRPC uses `grpc-previous-rpc-attempts`) so downstream services can see that a request is a retry and decline to retry it again themselves. This is the single cheapest defense against multi-hop amplification.

### 7.5 Deadline budgets

gRPC propagates deadlines automatically; plain HTTP does not. And almost nobody does the arithmetic correctly.

The problem: a client sets a 1s deadline. Service A spends 300ms, then calls B with a fresh 1s timeout. B spends 800ms and succeeds — but A's caller gave up 100ms ago. All of B's work was wasted, and B has no idea.

```go
type DeadlineBudget struct {
    // Reserve time for this hop's own processing and the response trip.
    Buffer  time.Duration   // default 50ms
    Minimum time.Duration   // default 20ms — below this, don't bother calling
}

func (b *DeadlineBudget) Derive(ctx context.Context) (context.Context, context.CancelFunc, error) {
    deadline, ok := ctx.Deadline()
    if !ok {
        // No inbound deadline. This is itself a smell — log it once per
        // method so users can find unbounded call paths.
        return ctx, func() {}, nil
    }

    remaining := time.Until(deadline)
    budget := remaining - b.Buffer

    if budget < b.Minimum {
        // Fail fast. Making the call would waste the dependency's capacity
        // on work whose result nobody will wait for.
        return nil, nil, connect.NewError(connect.CodeDeadlineExceeded,
            fmt.Errorf("insufficient deadline budget: %v remaining", remaining))
    }

    ctx, cancel := context.WithTimeout(ctx, budget)
    return ctx, cancel, nil
}
```

That fail-fast branch is the valuable part. Under load, a system that declines to start doomed work recovers; one that starts it anyway spends all its capacity on requests nobody is waiting for. This is a meaningful, measurable contribution and almost no in-house framework implements it.

### 7.6 Load shedding

Rejecting requests when overloaded keeps latency bounded for the requests you do accept. Queueing them instead produces the classic death spiral: queue grows, latency grows, clients time out and retry, queue grows faster.

Shed based on **queue latency**, not CPU. Time spent waiting to be handled is a direct signal of overload, and it's available without any platform-specific instrumentation:

```go
func (s *Shedder) Allow(queuedFor time.Duration) bool {
    return queuedFor < s.maxQueueLatency   // e.g. 100ms
}
```

Return `CodeResourceExhausted` (which is retryable with backoff and counted by the breaker), never `CodeUnavailable`, and always emit a metric so shedding is visible on a dashboard rather than silently degrading.

---

## 8. Lifecycle: startup, health, shutdown

### 8.1 Startup: fail fast and loudly

```
1. Load config from env
2. Validate exhaustively — unknown keys are errors, not warnings
3. Initialize telemetry (so startup failures are traced)
4. Construct dependencies (DB pools, clients) — connect eagerly, fail if unreachable
5. Register handlers
6. Start admin server (9090) — /healthz passes, /readyz does NOT yet
7. Run startup hooks (cache warm, migrations check, leader election)
8. Start main server (8080)
9. Flip /readyz to ready
10. Block
```

Ordering notes: the admin server starts *before* the main server so Kubernetes' startup probe can observe progress, and `/readyz` only flips after the main listener is actually accepting. Flipping readiness before the listener is up sends traffic to a connection-refused.

A service that starts successfully with a misconfigured database and fails on the first request is worse than one that refuses to start. **Validate everything at startup, exit non-zero with a message naming the exact field.**

### 8.2 Health endpoints: three different questions

| Endpoint | Question | Should it check dependencies? |
|---|---|---|
| `/healthz` (liveness) | Is this process wedged and in need of a restart? | **No.** Never. |
| `/readyz` (readiness) | Should this instance receive traffic right now? | **Almost never.** |
| `/startupz` (startup) | Has initialization finished? | Only its own init |

**The cascading-failure trap.** If `/readyz` checks the database, then a 10-second database blip marks *every replica* unready simultaneously. The load balancer removes all of them. A partial degradation — where the service could still serve cached reads and return clean errors for the rest — becomes a total outage. And now the service can't recover, because there are no healthy endpoints to route recovery traffic to.

**Readiness answers: "can *this instance* serve?"** Not "is the whole system healthy?" Correct readiness signals are: initialization finished, not draining, local resources available (connection pool not exhausted *for this instance*).

Also expose the standard gRPC health checking protocol (`grpc.health.v1.Health`) so gRPC clients and meshes can use it.

### 8.3 Graceful shutdown

Two non-obvious facts drive the sequence:

**Fact one: SIGTERM arrives before traffic stops.** Kubernetes sends SIGTERM and removes the pod's endpoint concurrently. Endpoint removal propagates through kube-proxy and every ingress and sidecar asynchronously, taking seconds. A process that exits immediately on SIGTERM drops in-flight requests and receives new ones after death.

**Fact two: telemetry must flush after the server stops, not before.** Flushing first loses every span from requests still draining.

```go
func (s *Service) shutdown(ctx context.Context) error {
    // 1. Fail readiness FIRST. Load balancers begin removing us.
    s.ready.Store(false)
    slog.InfoContext(ctx, "shutdown: readiness disabled")

    // 2. Wait for that to propagate. This sleep is not optional and not
    //    a hack — it is the only way to bridge eventually-consistent
    //    endpoint removal. Match it to your platform's propagation time.
    time.Sleep(s.cfg.PreStopDelay) // default 5s

    // 3. Stop accepting new connections; let in-flight requests finish.
    shutCtx, cancel := context.WithTimeout(ctx, s.cfg.DrainTimeout)
    defer cancel()
    if err := s.mainServer.Shutdown(shutCtx); err != nil {
        slog.ErrorContext(ctx, "drain timed out; forcing close", "error", err)
        _ = s.mainServer.Close()
    }

    // 4. Stop background workers, now that nothing is producing new work.
    s.workers.Stop(shutCtx)

    // 5. Close dependencies.
    s.deps.Close()

    // 6. Flush telemetry LAST, so spans from drained requests are exported.
    flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    defer cancelFlush()
    return s.telemetryShutdown(flushCtx)
}
```

Note `context.WithoutCancel` on the flush: if shutdown was triggered by a cancelled context, using it for the flush means the flush is cancelled immediately and you lose the spans that explain why you shut down.

**The timing relationship must hold:** `PreStopDelay + DrainTimeout < terminationGracePeriodSeconds`. If it doesn't, Kubernetes SIGKILLs you mid-drain. Validate this at startup by reading the grace period from the downward API, and log a warning if the arithmetic doesn't work.

---

## 9. Configuration

Environment variables only. No config files, no flag/file/env precedence puzzle.

```go
type Config struct {
    ServiceName string `env:"SERVICE_NAME,required"`
    Version     string `env:"SERVICE_VERSION" envDefault:"dev"`
    Environment string `env:"ENVIRONMENT,required" validate:"oneof=dev staging prod"`

    MainAddr  string `env:"MAIN_ADDR"  envDefault:":8080"`
    AdminAddr string `env:"ADMIN_ADDR" envDefault:":9090"`

    OTLPEndpoint string  `env:"OTEL_EXPORTER_OTLP_ENDPOINT,required"`
    SampleRatio  float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"0.1" validate:"gte=0,lte=1"`

    PreStopDelay time.Duration `env:"PRESTOP_DELAY" envDefault:"5s"`
    DrainTimeout time.Duration `env:"DRAIN_TIMEOUT" envDefault:"25s"`

    RetryEnabled bool    `env:"RETRY_ENABLED" envDefault:"true"`
    RetryBudget  float64 `env:"RETRY_BUDGET"  envDefault:"0.2" validate:"gte=0,lte=1"`

    BreakerEnabled bool `env:"BREAKER_ENABLED" envDefault:"true"`
}
```

Two rules:

**Reuse standard env var names where they exist.** `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_TRACES_SAMPLER_ARG` are OTel spec variables. Inventing `MYFW_TRACE_URL` means the OTel SDK's own defaults stop working and every operator has to learn your names.

**Validate at startup, exit non-zero with a precise message.** Not `invalid config` — `ENVIRONMENT must be one of [dev staging prod], got "production"`.

---

## 10. The public API

The entire point of the framework is that this is all the user writes:

```go
package main

import (
    "context"
    "log"

    "github.com/you/fw/service"
    userv1connect "example.com/gen/user/v1/userv1connect"
)

func main() {
    svc, err := service.New(
        service.WithConnectHandler(userv1connect.NewUserServiceHandler(
            newUserServer(),
        )),
        service.WithStartupHook(warmCache),
        service.WithDependency("postgres", db),
    )
    if err != nil {
        log.Fatal(err)
    }
    if err := svc.Run(context.Background()); err != nil {
        log.Fatal(err)
    }
}
```

`service.New` reads config from env, initializes OTel, builds the interceptor chain, starts both servers, registers health endpoints, and installs the shutdown sequence. `Run` blocks until SIGTERM, then drains.

### Escape hatches are mandatory

Every framework that gets adopted can be partially abandoned. Ship these from day one:

```go
// Get the raw mux to add arbitrary handlers.
func (s *Service) Mux() *http.ServeMux

// Take just the interceptor chain, use it with your own server.
func (f *Framework) ServerInterceptors() []connect.Interceptor
func (f *Framework) ClientInterceptors() []connect.Interceptor

// Take just the shutdown sequencer.
func NewShutdownSequencer(steps ...Step) *Sequencer

// Take just the resilient HTTP client.
func NewHTTPClient(opts ...ClientOption) *http.Client
```

Each of these should be usable with zero other parts of the framework, and each should be documented as a standalone package. Realistically, `ServerInterceptors()` and `NewHTTPClient()` are what people will actually take — and that's fine. A framework whose best-used part is one function is still a framework that helped.

---

## 11. Tech stack and setup

### 11.1 Dependencies

Keep the list short. A framework's dependency tree becomes every user's dependency tree, and every addition is supply-chain surface plus a future upgrade obligation.

```
RPC            connectrpc.com/connect
HTTP/2 (h2c)   golang.org/x/net/http2/h2c
Telemetry      go.opentelemetry.io/otel (+ sdk, otlp exporters)
               connectrpc.com/otelconnect
Logging        log/slog                      (stdlib)
Metrics        OTel metrics → Prometheus exporter
Validation     buf.build/go/protovalidate
Config         caarlos0/env + go-playground/validator
Proto tooling  buf (CLI, not a Go dependency)
Testing        stdlib + testify
```

Notably absent: a DI container, a logging library, an HTTP router, a config file parser. Each would be a large dependency for something the stdlib or one small package handles.

### 11.2 Protobuf toolchain

```yaml
# buf.yaml
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

```yaml
# buf.gen.yaml
version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt: paths=source_relative
  - remote: buf.build/connectrpc/go
    out: gen
    opt: paths=source_relative
```

```bash
buf lint                          # style
buf breaking --against '.git#branch=main'   # API compatibility, in CI
buf generate
```

`buf breaking` in CI is worth setting up on day one. Protobuf's compatibility rules are subtle — renaming a field is fine, renumbering it is catastrophic — and the tool catches what review misses.

### 11.3 Local development

```yaml
# docker-compose.yaml
services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib
    ports: ["4317:4317"]
  jaeger:
    image: jaegertracing/all-in-one
    ports: ["16686:16686"]
  prometheus:
    image: prom/prometheus
    ports: ["9091:9090"]
```

Ship this in `examples/`. A framework whose observability story requires standing up infrastructure before you can see a single span will not get evaluated past the README.

### 11.4 Go version and useful stdlib

Target a recent Go and use what's in the box:

- `log/slog` — structured logging, no dependency
- `net/http.ServeMux` enhanced routing — method and wildcard patterns, no router needed
- `context.WithoutCancel` — essential in shutdown and failure paths
- `errors.Join` — aggregating shutdown errors
- `sync.OnceValue` — lazy singletons without the boilerplate
- `testing/synctest` — **virtual time for tests.** Check whether it's stable in your Go version; if so, it makes testing circuit breakers, retry budgets, and shutdown timing deterministic instead of flaky. This one is worth checking for specifically — most of this framework's hard-to-test logic is time-dependent.

---

## 12. Repository layout

```
Microservices-API-Framework/
├── README.md
├── docs/
│   ├── design.md               ← this document
│   ├── interceptor-order.md    ← why the chain is ordered as it is
│   ├── resilience.md           ← breaker classification, retry budgets
│   ├── cardinality.md
│   └── service-mesh.md         ← when to turn resilience OFF
├── service/                    ← the convenience entry point
│   ├── service.go
│   ├── options.go
│   └── run.go
├── transport/
│   ├── connect.go
│   ├── admin.go
│   └── h2c.go
├── interceptor/                ← usable standalone
│   ├── chain.go
│   ├── tracing.go
│   ├── logging.go
│   ├── metrics.go
│   ├── recovery.go
│   ├── timeout.go
│   ├── auth.go
│   ├── validate.go
│   └── loadshed.go
├── resilience/                 ← usable standalone
│   ├── breaker/
│   ├── retry/
│   ├── budget/
│   └── deadline/
├── telemetry/
│   ├── otel.go
│   ├── slog.go
│   └── cardinality.go
├── lifecycle/                  ← usable standalone
│   ├── signals.go
│   ├── health.go
│   └── shutdown.go
├── client/
│   └── client.go               ← resilient Connect/HTTP client
├── fwtest/                     ← test helpers for USERS of the framework
│   ├── server.go
│   ├── recorder.go             ← assert on emitted spans/metrics
│   └── clock.go
├── examples/
│   ├── minimal/
│   ├── two-services/           ← shows propagation across a hop
│   └── docker-compose.yaml
└── proto/
```

`fwtest/` is not an afterthought. **A framework that makes its users' tests harder will not be adopted**, no matter how good the runtime behavior is. Being able to write `srv := fwtest.NewServer(t, handler)` and then assert on the spans it emitted is a feature people will choose the framework for.

---

## 13. Milestone ladder

Ordered so that the hard, subtle parts come early and the surface area comes last.

---

### M0 — Scope decision and a walking skeleton
**Est. 3–4 days**

Prototype the Connect handler serving all three protocols. Prove with `grpcurl`, `curl`, and a browser gRPC-Web client that one port and one handler genuinely does it. Write the "why not grpc-gateway" doc.

**Done when:** you've confirmed with your own hands that the headline feature is one line of Connect setup, and you've decided what the framework adds on top.

This milestone exists to kill the project early if the answer is "nothing."

---

### M1 — Config, lifecycle, health
**Est. 1 week**

Env config with strict validation, two servers, signal handling, the three health endpoints, and the full shutdown sequence.

**Test it for real:** deploy to a kind cluster, send continuous traffic, delete the pod, assert zero dropped requests.

**Done when:** a rolling restart under load drops nothing, and `PreStopDelay + DrainTimeout` is validated against the pod's grace period at startup.

---

### M2 — Interceptor chain and observability ★
**Est. 2 weeks**

The ordered server chain, OTel wiring, slog trace correlation, cardinality-safe metrics, exemplars.

**Done when:** a two-service example produces one connected trace across the hop, every log line carries the trace ID, and `/metrics` cardinality stays flat while you hammer it with a million distinct request payloads.

That last check is the real test. Write a load generator with random IDs and watch the series count.

---

### M3 — Circuit breaker
**Est. 1.5 weeks**

Sliding window, ratio-based tripping with a minimum-volume gate, half-open probing, per-method keying, and the error classification from §7.2.

**Write the classification tests first.** A table test asserting that `InvalidArgument` and `Canceled` do *not* trip the breaker while `Unavailable` does is the highest-value test in the project.

**Done when:** a flood of `InvalidArgument` from one caller leaves the breaker closed, and a genuine dependency outage opens it within the configured window.

---

### M4 — Retry with budget and idempotency enforcement ★
**Est. 1.5 weeks**

Proto `idempotency_level` read from the descriptor, fail-closed default, exponential backoff with full jitter, retry budget token bucket, retry marking header, breaker-open as terminal.

**Done when:** an unannotated method is provably never retried (assert on the transport call count), and a simulated total outage produces ≤1.2× load rather than 3×.

---

### M5 — Deadline budgets and load shedding
**Est. 1 week**

Budget derivation with buffer and minimum, fail-fast on insufficient budget, queue-latency-based shedding.

**Done when:** a three-hop call chain with a 1s client deadline never has a downstream service working past the client's deadline, and an overload test shows bounded p99 with visible shed metrics rather than an unbounded latency climb.

---

### M6 — The `service.New()` convenience layer
**Est. 1 week**

Wire everything into the one-call entry point. Options pattern. Escape hatches.

**Done when:** the minimal example is under 20 lines and every subsystem is still independently importable.

---

### M7 — Test helpers
**Est. 1 week**

`fwtest.NewServer`, span and metric recorders, a fake clock, and an in-memory client.

**Done when:** you can write a test that asserts "this handler emitted a span named X with attribute Y and recorded a `CodeUnavailable`" in five lines.

---

### M8 — Documentation and examples
**Est. 1.5 weeks**

This is not padding. For a framework, docs *are* the product — nobody evaluates a framework by reading its source.

Priority order: the two-service example with docker-compose and Jaeger; the interceptor-ordering rationale; the service-mesh guidance on when to disable resilience; a migration guide from raw grpc-go.

---

### M9 — Hardening
**Est. 2 weeks**

Benchmarks (overhead per RPC vs bare Connect), fuzzing the interceptors, race detector in CI, `buf breaking`, API stability commitment.

**Publish the overhead number honestly.** If your chain adds 40µs per call, say so. A framework that hides its cost will be found out by the first person who benchmarks it.

---

## 14. Reference implementations

### 14.1 Sliding-window breaker

```go
type Breaker struct {
    cfg   BreakerConfig
    mu    sync.Mutex
    state State
    buckets   []bucket          // ring, one per sub-interval of the window
    openedAt  time.Time
    halfOpen  int               // in-flight probes
    consecutiveOK int
    clock Clock                 // injectable — makes tests deterministic
}

type bucket struct {
    start          time.Time
    total, failures int
}

func (b *Breaker) Allow() (func(err error), error) {
    b.mu.Lock()
    defer b.mu.Unlock()

    now := b.clock.Now()
    b.evictOld(now)

    switch b.state {
    case StateOpen:
        if now.Sub(b.openedAt) < b.cfg.OpenDuration {
            return nil, ErrBreakerOpen
        }
        b.state = StateHalfOpen
        b.halfOpen = 0
        b.consecutiveOK = 0
        fallthrough

    case StateHalfOpen:
        if b.halfOpen >= b.cfg.HalfOpenMaxCalls {
            return nil, ErrBreakerOpen
        }
        b.halfOpen++
    }

    return func(err error) { b.record(err) }, nil
}

func (b *Breaker) record(err error) {
    b.mu.Lock()
    defer b.mu.Unlock()

    failed := countsAsFailure(err)   // §7.2 — the important part
    cur := b.currentBucket(b.clock.Now())
    cur.total++
    if failed {
        cur.failures++
    }

    switch b.state {
    case StateHalfOpen:
        b.halfOpen--
        if failed {
            b.trip()
            return
        }
        b.consecutiveOK++
        if b.consecutiveOK >= b.cfg.HalfOpenSuccesses {
            b.state = StateClosed
            b.reset()
        }

    case StateClosed:
        total, fails := b.sums()
        // The minimum-volume gate: without it, one failure out of two
        // requests on a quiet method trips the breaker.
        if total >= b.cfg.MinimumRequests &&
            float64(fails)/float64(total) >= b.cfg.FailureRatio {
            b.trip()
        }
    }
}
```

The injectable `Clock` matters more than it looks. Without it, every breaker test needs real sleeps, the suite takes minutes, and the tests are flaky on loaded CI machines.

### 14.2 Retry with full jitter

```go
func (r *Retrier) Do(ctx context.Context, md protoreflect.MethodDescriptor,
    fn func(context.Context) error) error {

    if !retryable(md) {
        return fn(ctx)   // not annotated safe → exactly one attempt, ever
    }

    var lastErr error
    for attempt := 0; attempt <= r.cfg.MaxAttempts; attempt++ {
        if attempt > 0 {
            if !r.budget.Allow() {
                r.metrics.budgetExhausted.Add(ctx, 1)
                return lastErr
            }
            // Full jitter: sleep uniformly in [0, backoff]. Beats
            // "backoff/2 + rand(backoff/2)" at desynchronizing clients.
            backoff := min(
                r.cfg.Base*time.Duration(1<<uint(attempt-1)),
                r.cfg.MaxBackoff,
            )
            select {
            case <-time.After(time.Duration(rand.Int63n(int64(backoff)))):
            case <-ctx.Done():
                return ctx.Err()
            }
        }

        attemptCtx, cancel, err := r.budgetDeadline.Derive(ctx)
        if err != nil {
            return err   // no budget left; don't burden the dependency
        }
        lastErr = fn(withRetryHeader(attemptCtx, attempt))
        cancel()

        if lastErr == nil || !isRetryable(lastErr) {
            return lastErr
        }
        r.budget.RecordRequest()
    }
    return lastErr
}
```

Full jitter rather than the more common "half the backoff plus random half": it's strictly better at spreading a thundering herd, which is the entire reason backoff exists.

### 14.3 Recovery interceptor (innermost)

```go
func Recovery() connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (
            resp connect.AnyResponse, err error) {

            defer func() {
                r := recover()
                if r == nil {
                    return
                }
                // http.ErrAbortHandler is a sentinel — re-panic so net/http
                // handles it as intended rather than logging a fake error.
                if r == http.ErrAbortHandler {
                    panic(r)
                }

                stack := debug.Stack()
                slog.ErrorContext(ctx, "panic recovered",
                    "panic", fmt.Sprint(r),
                    "procedure", req.Spec().Procedure,
                    "stack", string(stack),
                )
                if span := oteltrace.SpanFromContext(ctx); span.IsRecording() {
                    span.RecordError(fmt.Errorf("panic: %v", r),
                        oteltrace.WithStackTrace(true))
                    span.SetStatus(codes.Error, "panic")
                }
                // Never leak internals to the caller.
                err = connect.NewError(connect.CodeInternal,
                    errors.New("internal error"))
            }()

            return next(ctx, req)
        }
    }
}
```

Three details: the named return `err` is what lets the deferred function convert a panic into an error; `http.ErrAbortHandler` must be re-panicked or you break `net/http`'s connection-abort mechanism; and the error returned to the caller must be generic while the full stack goes to logs and the span.

---

## 15. Testing strategy

### 15.1 Test the ordering, not just the parts

Each interceptor working in isolation proves nothing. The bugs live in composition.

```go
func TestChainOrder(t *testing.T) {
    var calls []string
    rec := func(name string) connect.UnaryInterceptorFunc { /* append to calls */ }

    chain := connect.WithInterceptors(rec("trace"), rec("log"), rec("metrics"))
    // ... invoke ...

    require.Equal(t, []string{
        "trace:enter", "log:enter", "metrics:enter",
        "metrics:exit", "log:exit", "trace:exit",
    }, calls)
}

func TestPanicIsObservedByOuterInterceptors(t *testing.T) {
    // The real requirement: a panicking handler must produce
    // a recorded error span, an error log with a trace ID, AND
    // an incremented metric with code=internal.
    // If recovery were outermost, none of these would fire.
}
```

### 15.2 Use virtual time

Circuit breaker windows, retry backoff, budget decay, and shutdown delays are all time-dependent. Real sleeps make the suite slow and flaky.

Either inject a `Clock` interface everywhere (works on any Go version) or use `testing/synctest` if your Go version has it stable — it gives a fake clock that advances when all goroutines block, which makes concurrent time-dependent code testable without any injection at all. For this framework specifically it's close to purpose-built.

### 15.3 Telemetry assertions

```go
func TestSpanAttributes(t *testing.T) {
    rec := fwtest.NewSpanRecorder(t)
    srv := fwtest.NewServer(t, handler, fwtest.WithSpanRecorder(rec))

    _, _ = srv.Client().GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{}))

    span := rec.Require(t, "user.v1.UserService/GetUser")
    require.Equal(t, "user.v1.UserService", span.Attr("rpc.service"))
    require.Equal(t, codes.Error, span.Status())
}
```

Ship `SpanRecorder` in `fwtest/` so users can do this too. Telemetry is a contract; contracts deserve tests.

### 15.4 Failure injection

A resilience framework must be tested against the failures it claims to handle. Build a fake upstream that can be told to fail, hang, return partial responses, or close connections mid-stream, and assert on breaker state transitions and retry counts.

```go
up := fwtest.NewFakeUpstream(t)
up.FailNext(5, connect.CodeUnavailable)
// assert the breaker opened, and that exactly N attempts reached the upstream
```

### 15.5 Benchmark the overhead

```go
func BenchmarkChainOverhead(b *testing.B) {
    b.Run("bare-connect", ...)
    b.Run("with-framework", ...)
}
```

Track it in CI and publish it. This number is the first thing a skeptical Go developer will ask for, and having it ready is worth more than a paragraph of prose.

---

## 16. API stability and versioning

A framework is a promise. Decide what you're promising before anyone depends on you.

**Stay on v0 until the API has survived three real services.** Go module semantics treat v0 as explicitly unstable, and that's an honest signal. Rushing to v1 means either breaking your promise later or freezing a design you haven't validated.

**When you do commit to v1:**

- Options structs, never positional parameters — adding a field is non-breaking, adding a parameter isn't
- Interfaces stay small. Every method is a future obligation. Prefer concrete types with exported fields where you can.
- Add an unexported field to every exported struct so users can't construct them with positional literals, which would break when you add a field
- `buf breaking` on protos, and an API-diff tool on the Go surface, both in CI
- Deprecate with `// Deprecated:` for at least two minor versions before removal

**Document the support policy explicitly** — which Go versions, which OTel versions, what the upgrade cadence is. The single most common reason people avoid a framework is fear of abandonment, and a written policy addresses it directly.

---

## 17. Stretch goals

| Feature | Effort | Value |
|---|---|---|
| **Adaptive concurrency limiting** | Medium | Gradient/Vegas algorithms auto-tune the shed threshold. Better than a fixed limit and genuinely differentiating. |
| **Request hedging** | Medium | Send a second request at p99 latency, take the first response. Big p99 win; only safe for side-effect-free methods — which you already track. |
| **Outlier ejection** | Medium | Per-endpoint breaker across a load-balanced set |
| **Deadline propagation over plain HTTP** | Small | A header carrying remaining budget for non-Connect hops |
| **OpenAPI generation** | Medium | For teams that need REST-shaped docs |
| **grpc-gateway add-on** | Medium | For teams that need RESTful URLs |
| **Structured error catalog** | Small | Typed errors with stable codes, mapped to Connect codes and documented |
| **Chaos interceptor** | Small | Inject latency and errors by config, for testing *users'* resilience |
| **Multi-tenancy via baggage** | Medium | Propagate tenant context through OTel baggage |
| **`fwctl` scaffolding CLI** | Medium | New-service generation. Do this last — it's the most visible and least important part. |

---

## 18. References

### Books and papers

- *Release It!* (Nygard) — circuit breaker, bulkhead, and the failure patterns this framework exists to prevent. Read it before writing the resilience layer.
- *Site Reliability Engineering* (Google), "Handling Overload" and "Addressing Cascading Failures" — the source of the retry-budget and load-shedding thinking
- *Designing Data-Intensive Applications* (Kleppmann), ch. 8 — timeouts and unbounded delays
- AWS Architecture Blog, "Exponential Backoff and Jitter" — the full-jitter result
- Google's *Dapper* paper — distributed tracing foundations

### Specifications

| Spec | For |
|---|---|
| OpenTelemetry semantic conventions (RPC) | Attribute names your dashboards depend on |
| W3C Trace Context | `traceparent` / `tracestate` propagation |
| gRPC Health Checking Protocol | `grpc.health.v1.Health` |
| Connect protocol specification | Wire format and error mapping |
| gRPC over HTTP/2 | What Connect is compatible with |
| Protobuf `MethodOptions.idempotency_level` | The basis of retry safety |

### Code worth reading

- **connect-go** — read the interceptor and error-handling code. Small enough to read fully, and it's what you're building on.
- **grpc-go**'s retry and `balancer` packages — a production retry implementation with budgets
- **otelconnect** / **otelhttp** — how instrumentation is done idiomatically
- **go-grpc-middleware** v2 — a mature interceptor collection; borrow the composition patterns
- **Linkerd's retry budget implementation** — the canonical reference for §7.4
- **Netflix `concurrency-limits`** — adaptive limiting algorithms, if you attempt that stretch goal
- **sony/gobreaker** — a small, readable breaker. Read it, then note what it doesn't do: error classification (§7.2).

---

## Appendix A — Decision record

| Decision | Rationale |
|---|---|
| Build on connect-go, not a custom transport | One `http.Handler` already serves gRPC, gRPC-Web, and HTTP/JSON on one port with full gRPC wire compatibility. The headline feature is solved. |
| Reject cmux protocol sniffing | Breaks on h2c, TLS ALPN, and LB protocol re-origination; the failure mode is a hang, not an error |
| Don't claim "REST" | Connect's URLs are `POST /pkg.Service/Method`. Calling that REST is an overclaim that undermines everything else. |
| Configure OTel, never abstract it | A wrapper loses semantic conventions, propagation edge cases, and the entire contrib ecosystem |
| Composable packages + a convenience constructor | Frameworks you can't partially abandon don't get adopted. `ServerInterceptors()` must work standalone. |
| Recovery is the *innermost* interceptor | Outermost recovery converts panics to errors before metrics/logging/tracing observe them, so the panic never appears in telemetry |
| Tracing above logging | Log lines without a trace ID are near-useless in a distributed system |
| Metrics above load shedding | Shed requests must be counted, or dashboards look healthy during an overload incident |
| Breaker *inside* the retry loop | Per-attempt observations make the breaker open fast and become the natural cap on retry amplification |
| `ErrBreakerOpen` is non-retryable | Retrying into an open breaker burns budget for guaranteed failure |
| Breaker ignores client-fault codes | Counting `InvalidArgument` lets one buggy caller take down a healthy service for everyone |
| Breaker ignores `Canceled` | A user closing a browser tab is not evidence the dependency is sick |
| Ratio-based tripping with a minimum-volume gate | Absolute thresholds behave completely differently at 5 rps and 5000 rps |
| Retry only proto-declared safe methods, fail closed | Makes duplicate charges structurally impossible and forces an explicit reviewable decision |
| Retry budget as a fraction of traffic | Backoff bounds timing, not volume. A 20% budget turns 3× amplification into 1.2×. |
| Full jitter, not half-backoff-plus-random | Strictly better at desynchronizing a thundering herd |
| Deadline budget with fail-fast below a minimum | Prevents spending a struggling dependency's capacity on work nobody will wait for |
| Shed on queue latency, not CPU | Queue wait is the direct overload signal and is platform-independent |
| Metric labels as a closed type | Makes a cardinality explosion structurally impossible rather than a documented warning |
| Readiness never checks downstream dependencies | Otherwise a dependency blip marks every replica unready and turns partial degradation into total outage |
| Shutdown: readiness off → delay → drain → flush telemetry | SIGTERM precedes endpoint removal; flushing before drain loses in-flight spans |
| Separate admin server on its own port | `pprof` must not be publicly reachable, and observability must survive main-port saturation |
| Env-var config with standard OTel names | Inventing names breaks the OTel SDK's own defaults and every operator's muscle memory |
| Detect a mesh sidecar and warn about double retries | Stacked retries across layers multiply; a startup log line prevents more outages than a docs page |
| Ship `fwtest/` as a first-class package | A framework that makes users' tests harder will not be adopted |
| Stay on v0 until three real services use it | v0 is an honest signal; rushing v1 freezes an unvalidated design |

---

## Appendix B — Quick reference card

```
Server chain (outermost → innermost)
  panic guard → tracing → logging → metrics → load shed
  → timeout → auth → validation → recovery → handler

Client chain (outermost → innermost)
  tracing(logical) → metrics(logical) → deadline budget
  → retry → [ tracing(attempt) → metrics(attempt)
              → breaker → transport ]

Breaker counts as FAILURE
  Unavailable · DeadlineExceeded · ResourceExhausted
  Internal · DataLoss · Unknown
Breaker IGNORES
  InvalidArgument · NotFound · AlreadyExists · PermissionDenied
  Unauthenticated · FailedPrecondition · OutOfRange · Canceled

Retry
  only idempotency_level = NO_SIDE_EFFECTS | IDEMPOTENT
  unannotated → never retried (fail closed)
  budget: retries ≤ 20% of total traffic + floor
  full jitter: sleep ~ U(0, backoff)
  ErrBreakerOpen → terminal, never retried
  mark attempts via header so downstream won't re-retry

Deadline budget
  remaining = deadline − now
  downstream = remaining − buffer(50ms)
  if downstream < minimum(20ms) → fail fast, don't call

Ports
  8080  main    connect/gRPC/gRPC-Web
  9090  admin   /healthz /readyz /metrics /debug/pprof

Health
  /healthz   liveness   — NEVER checks dependencies
  /readyz    readiness  — this instance only, not the system
  /startupz  startup    — own init only

Shutdown (order is load-bearing)
  1 readiness off
  2 sleep PreStopDelay        (endpoint removal is eventually consistent)
  3 server.Shutdown(drain)
  4 stop workers
  5 close dependencies
  6 flush telemetry           (use context.WithoutCancel)
  invariant: PreStopDelay + DrainTimeout < terminationGracePeriodSeconds

Metric labels
  OK     rpc.method (proto-static) · rpc.code · service · env
  NEVER  raw path · user/tenant id · error text · timestamps
```
