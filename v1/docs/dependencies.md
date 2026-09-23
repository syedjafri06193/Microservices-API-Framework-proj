# Dependencies

A framework's dependency tree becomes every user's dependency tree. Every
addition is supply-chain surface plus a future upgrade obligation, so the
list is short and each entry earns its place.

## What the framework depends on

| Purpose | Module | Why it is unavoidable |
|---|---|---|
| RPC | `connectrpc.com/connect` | The dual-mode routing, and the whole reason the project is viable |
| Tracing and metrics | `go.opentelemetry.io/otel` + sdk | The telemetry standard; wrapping it is the mistake |
| Connect ↔ OTel | `connectrpc.com/otelconnect` | Semantic conventions done right, by the people who own both |
| HTTP/2 without TLS | `golang.org/x/net/http2/h2c` | gRPC over plaintext behind a mesh or LB |
| Protobuf | `google.golang.org/protobuf` | The source of truth |
| Scrape endpoint | OTel Prometheus exporter | Optional in spirit; one flag away |
| Logging | `log/slog` | Standard library |

## What it deliberately does not depend on

**A config library.** The design document names `caarlos0/env` and
`go-playground/validator`; between them that is six modules for `env`,
`envDefault`, `oneof`, `gte` and `lte`. `internal/config` is ~200 lines with
the same struct tags, so switching later is deleting a file.

**`protovalidate`.** It brings `cel-go`, which is large. The validation
interceptor takes an interface that `protovalidate.Validator` satisfies
directly:

```go
v, err := protovalidate.New()
svc, err := service.New(service.WithValidator(v))
```

Teams that want it pay for it. Teams that validate in the handler pay
nothing.

**A circuit breaker library.** `sony/gobreaker` and `failsafe-go` are both
fine, and this framework implements its own for a reason the design document
supplies: the important decision is *which errors reach the counter*, and no
library takes an opinion on that. See `notes-on-the-spec.md` §1.

**The OTLP gRPC exporter.** The HTTP one speaks the same protocol to the
same collectors without dragging `grpc-go` and `genproto` into every
service.

## Building offline

The `replace` block in `go.mod` points every dependency at a local checkout,
because the machine this was built on cannot reach `proxy.golang.org`. On a
machine with a working proxy, delete the block and run `go mod tidy`; the
framework itself depends on nothing unusual. See `building.md`.
