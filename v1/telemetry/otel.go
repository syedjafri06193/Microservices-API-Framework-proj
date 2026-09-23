// Package telemetry configures OpenTelemetry. It does not abstract it.
//
// The most common framework mistake is wrapping OTel in a house interface
// "in case we switch later". You will not switch, and the wrapper costs:
//
//   - Semantic conventions. OTel standardises rpc.system, rpc.method,
//     server.address, and backends ship dashboards built on those names. A
//     wrapper that emits "service" and "method" breaks every one of them.
//   - Propagation subtleties. W3C traceparent, baggage and span links have
//     edge cases the SDK handles and a wrapper will not.
//   - The contrib ecosystem. otelhttp, otelconnect and otelsql emit spans
//     into the OTel context directly; a wrapper cannot see them, so
//     database spans become orphans.
//
// So users import go.opentelemetry.io/otel directly for their own spans.
// That is a feature: their code is coupled to OTel, which is a standard,
// rather than to this framework, which is not.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// ExporterKind selects where traces go.
type ExporterKind string

const (
	// ExporterOTLP sends to an OTLP collector over HTTP.
	ExporterOTLP ExporterKind = "otlp"
	// ExporterStdout prints spans, for local development.
	ExporterStdout ExporterKind = "stdout"
	// ExporterNone disables tracing. Metrics still work.
	ExporterNone ExporterKind = "none"
)

// Config describes the telemetry setup.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string

	Exporter     ExporterKind
	OTLPEndpoint string
	// OTLPInsecure sends plaintext, which is normal inside a cluster where
	// a mesh or the collector sidecar terminates TLS.
	OTLPInsecure bool
	SampleRatio  float64

	// PrometheusRegistry, when non-nil, exposes metrics for scraping. The
	// admin server serves it on /metrics.
	EnablePrometheus bool
}

// Providers holds what was constructed, so the service can shut it down in
// the right order.
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	// PrometheusRegistry is non-nil when EnablePrometheus was set. The OTel
	// exporter registers itself into this registry rather than being a
	// Collector, so the admin server serves the registry directly.
	PrometheusRegistry *prometheus.Registry

	shutdowns []func(context.Context) error
}

// Shutdown flushes and stops everything, in reverse order of construction.
//
// Called last in the shutdown sequence, deliberately: flushing before the
// server has drained loses the spans for every request still in flight,
// which are exactly the spans that explain a shutdown-time incident.
func (p *Providers) Shutdown(ctx context.Context) error {
	var errs []error
	for i := len(p.shutdowns) - 1; i >= 0; i-- {
		if err := p.shutdowns[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Init configures OTel globally and returns the providers.
func Init(ctx context.Context, cfg Config) (*Providers, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: ServiceName is required")
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, fmt.Errorf("telemetry: SampleRatio must be in [0, 1], got %v", cfg.SampleRatio)
	}

	res, err := resource.New(ctx,
		// WithFromEnv honours OTEL_RESOURCE_ATTRIBUTES, so an operator can
		// add attributes without the framework knowing about them.
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		// A schema-URL conflict between detectors is reported as an error
		// but leaves a usable resource. Failing startup over it would make
		// the framework refuse to boot on a perfectly good cluster.
		if res == nil {
			return nil, fmt.Errorf("telemetry: resource: %w", err)
		}
	}

	providers := &Providers{}

	if cfg.Exporter != ExporterNone {
		var exporter sdktrace.SpanExporter
		switch cfg.Exporter {
		case ExporterStdout:
			exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		case ExporterOTLP, "":
			opts := []otlptracehttp.Option{}
			if cfg.OTLPEndpoint != "" {
				opts = append(opts, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
			}
			if cfg.OTLPInsecure {
				opts = append(opts, otlptracehttp.WithInsecure())
			}
			exporter, err = otlptracehttp.New(ctx, opts...)
		default:
			return nil, fmt.Errorf("telemetry: unknown exporter %q", cfg.Exporter)
		}
		if err != nil {
			return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(exporter,
				sdktrace.WithBatchTimeout(5*time.Second)),
			// ParentBased(TraceIDRatioBased) is the right default: honour
			// the caller's sampling decision when there is one, so a trace
			// is never half-sampled across services, and only make an
			// independent decision at the edge.
			sdktrace.WithSampler(sdktrace.ParentBased(
				sdktrace.TraceIDRatioBased(cfg.SampleRatio),
			)),
		)
		otel.SetTracerProvider(tp)
		providers.TracerProvider = tp
		providers.shutdowns = append(providers.shutdowns, tp.Shutdown)
	}

	if cfg.EnablePrometheus {
		// A dedicated registry, not the default one. The default registry
		// carries Go runtime and process collectors that this exporter
		// would duplicate, and a duplicate registration is a panic at
		// startup rather than a warning.
		registry := prometheus.NewRegistry()
		exp, err := promexporter.New(promexporter.WithRegisterer(registry))
		if err != nil {
			return nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(exp),
		)
		otel.SetMeterProvider(mp)
		providers.MeterProvider = mp
		providers.PrometheusRegistry = registry
		providers.shutdowns = append(providers.shutdowns, mp.Shutdown)
	}

	// W3C trace context and baggage, and nothing else. Every additional
	// propagator is more header parsing on every single request; B3 belongs
	// here only when an older fleet forces it.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return providers, nil
}

// ServiceAttrs returns the attributes every metric should carry, so that a
// dashboard can separate environments without the caller assembling them.
func ServiceAttrs(cfg Config) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
		semconv.DeploymentEnvironment(cfg.Environment),
	}
}
