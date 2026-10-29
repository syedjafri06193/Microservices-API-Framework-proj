# Microservices API Framework

An opinionated Go wiring layer over [connect-go](https://connectrpc.com) and
[OpenTelemetry](https://opentelemetry.io): one function call gives a service
correct-by-default tracing, metrics, circuit breaking, retry budgets, deadline
budgets, health checks and graceful shutdown — and serves Connect, gRPC and
gRPC-Web on one port.

[![CI](https://github.com/syedjafri06193/Microservices-API-Framework-proj/actions/workflows/ci.yml/badge.svg)](https://github.com/syedjafri06193/Microservices-API-Framework-proj/actions/workflows/ci.yml)

## Highlights

- **Ordered interceptor chain** — tracing, metrics, recovery, validation and
  resilience composed in the order that keeps telemetry honest during incidents
- **Breaker error classification** — client faults never open a breaker
- **Retries only for idempotent methods**, read from the proto descriptor, capped by a retry budget
- **Deadline budgets** — refuse calls that cannot finish in the time left
- **Shutdown sequencing** — readiness drops first, in-flight work drains, dependencies close last
- **Every package usable on its own** — adopt one piece, leave the rest

## Quick start

Requires Go 1.24+.

```bash
cd v1
go test -race ./...
SERVICE_NAME=echo ENVIRONMENT=dev OTEL_TRACES_EXPORTER=stdout go run ./examples/minimal

# In another terminal — plain JSON over HTTP, no client library needed
curl -X POST localhost:8080/echo.v1.EchoService/Echo \
  -H 'Content-Type: application/json' -d '{"message":"hello"}'
```

The full walkthrough is in [`v1/README.md`](v1/README.md).

## Repository layout

```
.
├── 1—Documentation/   all documentation (start with its README index)
│   ├── design-guide.md     full design & build guide (also as PDF)
│   └── v1/                 interceptor order, resilience, lifecycle, …
└── v1/                first implementation
    ├── service/       convenience entry point
    ├── interceptor/   the ordered chain
    ├── resilience/    breaker, retry, budget, deadline, shed
    ├── telemetry/     OTel setup, trace-correlated slog, cardinality guard
    ├── lifecycle/     health, signals, shutdown sequencer
    ├── transport/     the two HTTP servers (h2c)
    ├── fwtest/        test helpers
    └── examples/      minimal service and a two-hop traced example
```

## Documentation

All documentation lives in [`1—Documentation/`](1—Documentation/README.md).

## Versions and feedback

| Version | Summary | Feedback |
|---|---|---|
| [v1](v1/) | Connect + OTel wiring, ordered interceptors, breaker/retry/budget/deadline/shed, lifecycle | — |

Add a row per version as new iterations land.

<!-- project-metrics:start -->
## Project Metrics

[![Stars](https://img.shields.io/github/stars/syedjafri06193/Microservices-API-Framework-proj?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj/stargazers) [![Forks](https://img.shields.io/github/forks/syedjafri06193/Microservices-API-Framework-proj?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj/forks) [![Open issues](https://img.shields.io/github/issues/syedjafri06193/Microservices-API-Framework-proj?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj/issues) [![Last commit](https://img.shields.io/github/last-commit/syedjafri06193/Microservices-API-Framework-proj/improvements?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj/commits/improvements) [![Commits](https://img.shields.io/github/commit-activity/t/syedjafri06193/Microservices-API-Framework-proj?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj/commits/improvements) [![Top language](https://img.shields.io/github/languages/top/syedjafri06193/Microservices-API-Framework-proj?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj) [![Repo size](https://img.shields.io/github/repo-size/syedjafri06193/Microservices-API-Framework-proj?style=flat-square)](https://github.com/syedjafri06193/Microservices-API-Framework-proj)

### Code

| Metric | Value |
|---|---|
| Lines of code | 3,828 |
| Source files | 48 |
| Tracked files | 67 |
| Documentation | 13 Markdown files (16,719 words), 1 PDF |
| Commits | 11 |
| Contributors | 2 |
| Active | 2026-09-11 → 2026-10-28 |

| Language | Lines | Share |
|---|---:|---:|
| Go | 3,727 | 97.4% |
| Modula-2 | 42 | 1.1% |
| Protocol Buffer | 38 | 1.0% |
| YAML | 21 | 0.5% |

<sub>Code stats as of 2026-10-28, counted with pygount (comments and blank lines excluded). Badges update live.</sub>
<!-- project-metrics:end -->
