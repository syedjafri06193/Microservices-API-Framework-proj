# Microservices API Framework — v1

An opinionated wiring layer over [connect-go](https://connectrpc.com) and
[OpenTelemetry](https://opentelemetry.io) that gives a Go service
correct-by-default observability, resilience and lifecycle handling in one
function call.

The implementation of the design in [`../Documentation/README.md`](../Documentation/README.md).
Where this departs from that document, [`docs/notes-on-the-spec.md`](docs/notes-on-the-spec.md)
says where and why.

## What this is, and what it is not

**It is not a new microservices framework.** Go's framework graveyard is
well populated, and the reason is cultural rather than technical: the
ecosystem rewards small composable libraries and punishes frameworks that
ask for an inversion of control.

**It is the wiring.** Every individual capability here exists in a mature
library. What does not exist in one place is the *correct composition* of
them — and that is a real gap, because every Go team writes this layer, most
write it subtly wrong, and none of them get to reuse it.

The subtle parts, done once:

- **Interceptor ordering**, where most hand-rolled setups are wrong in ways
  that make the telemetry lie during an incident
- **Breaker error classification**, so one caller with a bug cannot take a
  healthy service offline for everyone else
- **Retries only on methods the proto declares safe**, failing closed
- **Retry budgets**, so a partial outage produces 1.2× load rather than 3×
- **Deadline budget arithmetic**, which almost nothing implements
- **Shutdown sequencing**, where the ordering is counterintuitive twice

Being honest about the borrowed parts: the dual-mode routing is Connect's,
and it is done. `POST /user.v1.UserService/GetUser` is not REST, and calling
it REST would be the kind of overclaim that erodes trust in everything else
here.

## The whole of a user's main()

```go
func main() {
    svc, err := service.New(
        service.WithConnectService(func(opts ...connect.HandlerOption) (string, http.Handler) {
            return userv1connect.NewUserServiceHandler(newUserServer(), opts...)
        }),
        service.WithStartupHook("warm cache", warmCache),
        service.WithDependency("postgres", db.Close),
    )
    if err != nil {
        log.Fatal(err)
    }
    if err := svc.Run(context.Background()); err != nil {
        log.Fatal(err)
    }
}
```

That reads config from the environment, initializes OTel, builds the
interceptor chain in the correct order, starts two servers, registers the
health endpoints and installs the shutdown sequence. `Run` blocks until
SIGTERM, then drains.

One handler, one port, three protocols:

```bash
# Connect's JSON protocol: no client library, no proxy
curl -X POST localhost:8080/echo.v1.EchoService/Echo \
  -H 'Content-Type: application/json' -d '{"message":"hello"}'

# gRPC, same port
grpcurl -plaintext localhost:8080 echo.v1.EchoService/Echo

# gRPC-Web from a browser, same port
```

## Quick start

```bash
go build ./...
go test ./...
go test ./service/ -bench=ChainOverhead -benchmem

SERVICE_NAME=echo ENVIRONMENT=dev OTEL_TRACES_EXPORTER=stdout \
  go run ./examples/minimal
```

The `go.mod` carries a `replace` block because the machine this was built on
cannot reach `proxy.golang.org`. On a normal machine, delete it and run
`go mod tidy` — nothing here depends on it. See [`docs/building.md`](docs/building.md).

## Layout

```
service/       the convenience entry point, and the only part that is one
interceptor/   the ordered chain — usable standalone
resilience/    breaker, retry, budget, deadline, shed — usable standalone
telemetry/     OTel configuration, trace-correlated slog, cardinality guard
lifecycle/     health, signals, shutdown sequencer — usable standalone
transport/     the two servers, h2c
fwtest/        test helpers for users of the framework
examples/      minimal, and two services showing a trace across a hop
```

Every package is usable with none of the others. That is not decoration:
teams adopt tools they can abandon incrementally, and a framework that owns
`main()` with no way out is one nobody will risk. See
[`docs/escape-hatches.md`](docs/escape-hatches.md).

## The two ports

| Port | Serves | Exposed |
|---|---|---|
| 8080 | Connect / gRPC / gRPC-Web | Yes |
| 9090 | `/healthz` `/readyz` `/startupz` `/metrics` `/debug/pprof` `/debug/breakers` `/debug/cardinality` | Cluster-internal |

Separate `http.Server` instances, not two routes on one mux. Putting
`/metrics` and pprof on the public port means a heap profile is one
unauthenticated request away, and it means load shedding on the main port
can take down your ability to observe the incident.

## Using it with a service mesh

If Istio or Linkerd is already doing retries and circuit breaking, doing it
twice is worse than not doing it at all — three layers each retrying three
times turns one failure into 27× load across three hops.

The framework detects a sidecar at startup and says so:

```
WARN retries are enabled in-process AND a service mesh sidecar was detected;
     this may amplify load during a partial outage  mesh=istio
```

It warns rather than disabling anything, because detection is heuristic and
a false positive that silently removed your resilience would be worse. Set
`RETRY_ENABLED=false` and `BREAKER_ENABLED=false` and keep the rest — the
deadline budgets, semantic failure handling and per-method policy are things
a sidecar structurally cannot do. See [`docs/service-mesh.md`](docs/service-mesh.md).

## Documentation

| | |
|---|---|
| [interceptor-order.md](docs/interceptor-order.md) | Why the chain is ordered as it is, adjacency by adjacency |
| [resilience.md](docs/resilience.md) | Breaker classification, retry budgets, deadline budgets, shedding |
| [lifecycle.md](docs/lifecycle.md) | Startup, the three health questions, the shutdown sequence |
| [cardinality.md](docs/cardinality.md) | Why label values are a closed type |
| [service-mesh.md](docs/service-mesh.md) | When to turn resilience off |
| [escape-hatches.md](docs/escape-hatches.md) | Taking one piece and leaving the rest |
| [dependencies.md](docs/dependencies.md) | What is depended on, and what is deliberately not |
| [building.md](docs/building.md) | Including the offline `replace` block and codegen |
| [notes-on-the-spec.md](docs/notes-on-the-spec.md) | Where this departs from the design |

## Status

Built, tested and benchmarked. The test suite covers the properties that
matter rather than the lines that exist:

- the exact interceptor order, entries and exits
- a panicking handler observed as an error by the interceptors above it
- client faults never opening a breaker; server faults opening it and
  stopping traffic reaching the upstream
- a non-idempotent method called exactly once, read from the real generated
  descriptor
- a retry budget containing a storm to ~1.1× instead of 3×
- a deadline budget refusing a call that cannot finish
- shutdown running to completion from a cancelled context
- the same handler answering both JSON and gRPC on one port

Everything passes under `-race`.

**Not done**, from the design's later milestones: gRPC's standard health
checking protocol (§8.2), exemplars linking metrics to traces (§6.4),
streaming interceptors — the chain is unary-only, which covers the
framework's own surface but is a real gap for a service that streams — and
any deployment beyond a local `docker compose`.

The realistic goal, as the design document says plainly, is not adoption. It
is a reference implementation that demonstrates an understanding of
distributed systems failure modes. That framing is worth keeping.
