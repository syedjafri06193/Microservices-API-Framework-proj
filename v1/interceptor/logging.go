package interceptor

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

type loggerKey struct{}

// LoggerFrom returns the request-scoped logger, or the default.
//
// Handlers use this rather than a package-level logger so that every line
// they write carries the trace ID the tracing interceptor established.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Logging attaches a request-scoped logger and logs the outcome.
//
// It sits directly below tracing, because a log line without a trace ID is
// nearly useless in a distributed system, and the span has to exist before
// the logger is built. The correlation itself is automatic: the logger's
// handler reads the span from the context on every record (see
// telemetry.ContextHandler), so a handler that logs with the *Context
// variants gets trace IDs without doing anything.
func Logging(logger *slog.Logger, slowThreshold time.Duration) connect.UnaryInterceptorFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			procedure := req.Spec().Procedure

			// Procedure only. Never a URL path, a header, or anything else
			// a caller controls: this logger's attributes end up on every
			// line for the request, and unbounded attribute values are the
			// log-volume equivalent of a cardinality explosion.
			reqLogger := logger.With(slog.String("procedure", procedure))
			ctx = context.WithValue(ctx, loggerKey{}, reqLogger)

			resp, err := next(ctx, req)
			elapsed := time.Since(start)

			switch {
			case err != nil:
				code := connect.CodeOf(err)
				level := slog.LevelWarn
				if isServerFaultCode(code) {
					// A client fault is the caller's problem and should not
					// page anyone. A server fault is ours.
					level = slog.LevelError
				}
				reqLogger.Log(ctx, level, "rpc failed",
					slog.String("code", code.String()),
					slog.Duration("duration", elapsed),
					slog.String("error", err.Error()),
				)
			case slowThreshold > 0 && elapsed >= slowThreshold:
				reqLogger.WarnContext(ctx, "rpc was slow",
					slog.Duration("duration", elapsed),
					slog.Duration("threshold", slowThreshold),
				)
			default:
				// Successful calls log at debug. A line per successful RPC
				// at info is how a service generates more log volume than
				// traffic, and it buries the lines that matter.
				reqLogger.DebugContext(ctx, "rpc completed",
					slog.Duration("duration", elapsed),
				)
			}

			return resp, err
		}
	}
}

func isServerFaultCode(code connect.Code) bool {
	switch code {
	case connect.CodeInternal, connect.CodeUnknown, connect.CodeDataLoss,
		connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return true
	}
	return false
}
