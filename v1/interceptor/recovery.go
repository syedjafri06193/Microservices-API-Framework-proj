package interceptor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Recovery converts a panic in the handler into a CodeInternal error.
//
// This interceptor belongs **innermost**, closest to the handler, which is
// the opposite of where most people put it. Outermost, it would convert the
// panic to an error before the tracing, logging and metrics interceptors
// saw it, so they would record a tidy Internal error and you would lose the
// one signal that says a handler crashed.
//
// Innermost, the panic becomes a proper error that propagates outward and
// is recorded correctly at every layer: the span gets an error status with
// a stack trace, the log line carries the trace ID, and the metric counts
// code=internal.
func Recovery(logger *slog.Logger) connect.UnaryInterceptorFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		// The named return is what lets the deferred function replace the
		// panic with an error. Without it there is nothing to assign to and
		// the recover cannot produce a response.
		return func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}

				// http.ErrAbortHandler is a sentinel: net/http uses it to
				// abort a connection deliberately. Swallowing it would
				// break that mechanism and log a fake error for something
				// that is working as designed.
				if r == http.ErrAbortHandler {
					panic(r)
				}

				stack := debug.Stack()
				procedure := ""
				if req != nil && req.Spec().Procedure != "" {
					procedure = req.Spec().Procedure
				}

				logger.ErrorContext(ctx, "panic recovered in handler",
					slog.Any("panic", r),
					slog.String("procedure", procedure),
					slog.String("stack", string(stack)),
				)

				if span := oteltrace.SpanFromContext(ctx); span.IsRecording() {
					span.RecordError(fmt.Errorf("panic: %v", r), oteltrace.WithStackTrace(true))
					span.SetStatus(codes.Error, "panic")
				}

				// The caller gets nothing useful, on purpose. A panic
				// message is an internal detail and frequently contains a
				// pointer, a query, or a fragment of somebody's data.
				resp = nil
				err = connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}()

			return next(ctx, req)
		}
	}
}

// PanicGuard is the outermost interceptor. It exists because Recovery, being
// innermost, cannot protect against a panic in an interceptor — including in
// the tracing or metrics interceptors themselves.
//
// It does nothing but recover, log, and return Internal. Keeping it
// deliberately dumb is the point: an outermost guard that tried to record
// telemetry could panic again inside the handler for the first panic, and
// then the process really does go down.
func PanicGuard(logger *slog.Logger) connect.UnaryInterceptorFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				if r == http.ErrAbortHandler {
					panic(r)
				}
				logger.ErrorContext(ctx, "panic recovered in the interceptor chain",
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
				resp = nil
				err = connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}()
			return next(ctx, req)
		}
	}
}
