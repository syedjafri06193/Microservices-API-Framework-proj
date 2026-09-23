module github.com/syedjafri06193/microservices-api-framework

go 1.24.0

toolchain go1.24.7

require (
	connectrpc.com/connect v1.19.2
	connectrpc.com/otelconnect v0.9.0
	github.com/prometheus/client_golang v1.23.2
	github.com/stretchr/testify v1.12.1
	go.opentelemetry.io/otel v1.40.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.38.0
	go.opentelemetry.io/otel/exporters/prometheus v0.60.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.38.0
	go.opentelemetry.io/otel/metric v1.39.0
	go.opentelemetry.io/otel/sdk v1.39.0
	go.opentelemetry.io/otel/sdk/metric v1.39.0
	go.opentelemetry.io/otel/trace v1.40.0
	golang.org/x/net v0.50.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/otlptranslator v0.0.2 // indirect
	github.com/prometheus/procfs v0.17.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.38.0 // indirect
	go.opentelemetry.io/proto/otlp v1.7.1 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260209200024-4cfbd4190f57 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260209200024-4cfbd4190f57 // indirect
	google.golang.org/grpc v1.79.2 // indirect
)

// The module proxy is unreachable from the build environment, so every
// dependency is a local checkout of its upstream repository at a release
// tag. See docs/building.md — the framework itself depends on nothing
// unusual, and `go mod tidy` against a working proxy removes this block
// entirely.
replace (
	connectrpc.com/connect => ../../godeps/connect-go
	connectrpc.com/otelconnect => ../../godeps/otelconnect
	github.com/beorn7/perks => ../../godeps/perks
	github.com/cenkalti/backoff/v5 => ../../godeps/backoff
	github.com/cespare/xxhash/v2 => ../../godeps/xxhash
	github.com/davecgh/go-spew => ../../godeps/go-spew
	github.com/go-logr/logr => ../../godeps/logr
	github.com/go-logr/stdr => ../../godeps/stdr
	github.com/google/go-cmp => ../../godeps/go-cmp
	github.com/google/uuid => ../../godeps/uuid
	github.com/grpc-ecosystem/grpc-gateway/v2 => ../../godeps/grpc-gateway
	github.com/munnerz/goautoneg => ../../godeps/goautoneg
	github.com/pmezard/go-difflib => ../../godeps/go-difflib
	github.com/prometheus/client_golang => ../../godeps/client_golang
	github.com/prometheus/client_model => ../../godeps/client_model
	github.com/prometheus/common => ../../godeps/prom-common
	github.com/prometheus/procfs => ../../godeps/procfs
	github.com/stretchr/testify => ../../godeps/testify
	go.opentelemetry.io/auto/sdk => ../../godeps/otel-auto/sdk
	go.opentelemetry.io/otel => ../../godeps/otel
	go.opentelemetry.io/otel/exporters/otlp/otlptrace => ../../godeps/otel/exporters/otlp/otlptrace
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp => ../../godeps/otel/exporters/otlp/otlptrace/otlptracehttp
	go.opentelemetry.io/otel/exporters/prometheus => ../../godeps/otel/exporters/prometheus
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace => ../../godeps/otel/exporters/stdout/stdouttrace
	go.opentelemetry.io/otel/metric => ../../godeps/otel/metric
	go.opentelemetry.io/otel/sdk => ../../godeps/otel/sdk
	go.opentelemetry.io/otel/sdk/metric => ../../godeps/otel/sdk/metric
	go.opentelemetry.io/otel/trace => ../../godeps/otel/trace
	go.opentelemetry.io/proto/otlp => ../../godeps/otlp-proto/otlp
	golang.org/x/net => ../../godeps/net
	golang.org/x/sys => ../../godeps/sys
	golang.org/x/text => ../../godeps/text
	google.golang.org/genproto/googleapis/api => ../../godeps/genproto/googleapis/api
	google.golang.org/genproto/googleapis/rpc => ../../godeps/genproto/googleapis/rpc
	google.golang.org/grpc => ../../godeps/grpc-go
	google.golang.org/protobuf => ../../godeps/protobuf-go
	gopkg.in/yaml.v3 => ../../godeps/yaml3
)

replace (
	github.com/prometheus/otlptranslator => ../../godeps/otlptranslator
	go.uber.org/goleak => ../../godeps/goleak
	go.yaml.in/yaml/v2 => ../../godeps/goyaml2
	go.yaml.in/yaml/v3 => ../../godeps/goyaml
)

replace github.com/kylelemons/godebug => ../../godeps/godebug

replace github.com/golang/protobuf => ../../godeps/golang-protobuf
