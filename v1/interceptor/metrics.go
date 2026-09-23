package interceptor

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/syedjafri06193/microservices-api-framework/telemetry"
)

// Metrics records RPC duration and counts, using only safe labels.
//
// It sits above load shedding, so that shed requests are measured. A
// shedder outside the metrics interceptor produces the worst possible
// dashboard: during an overload incident the service appears to be handling
// less traffic perfectly, because the rejected requests were never counted.
//
// Label values come from telemetry.MethodLabel, which cannot be constructed
// from an arbitrary string. That is what makes a cardinality explosion
// structurally impossible rather than merely discouraged — the route
// template is the only thing that can become a label.
type Metrics struct {
	duration metric.Float64Histogram
	inFlight metric.Int64UpDownCounter
	guard    *telemetry.CardinalityGuard
	base     []attribute.KeyValue
}

// NewMetrics builds the RPC instruments.
func NewMetrics(meter metric.Meter, base []attribute.KeyValue, guard *telemetry.CardinalityGuard) (*Metrics, error) {
	duration, err := meter.Float64Histogram(
		"rpc.server.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of inbound RPCs."),
	)
	if err != nil {
		return nil, err
	}
	inFlight, err := meter.Int64UpDownCounter(
		"rpc.server.active_requests",
		metric.WithDescription("Number of inbound RPCs currently being handled."),
	)
	if err != nil {
		return nil, err
	}
	return &Metrics{duration: duration, inFlight: inFlight, guard: guard, base: base}, nil
}

// Interceptor returns the metrics interceptor.
func (m *Metrics) Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			method := telemetry.FromProcedure(req.Spec().Procedure)

			inFlightAttrs := append(append([]attribute.KeyValue{}, m.base...), method.Attr())
			m.inFlight.Add(ctx, 1, metric.WithAttributes(inFlightAttrs...))
			start := time.Now()

			resp, err := next(ctx, req)

			// context.WithoutCancel: a cancelled request still has to
			// record its own duration, and a cancelled context would make
			// the metrics SDK drop the measurement — losing exactly the
			// data you need when a wave of cancellations starts.
			recordCtx := context.WithoutCancel(ctx)
			m.inFlight.Add(recordCtx, -1, metric.WithAttributes(inFlightAttrs...))

			attrs := append(append([]attribute.KeyValue{}, m.base...),
				method.Attr(),
				attribute.String("rpc.code", codeOf(err)),
			)
			if m.guard != nil {
				m.guard.Observe("rpc.server.duration", attrs)
			}
			m.duration.Record(recordCtx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))

			return resp, err
		}
	}
}

func codeOf(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}
