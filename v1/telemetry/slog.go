package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// ContextHandler adds trace correlation to every log record.
//
// A log line without a trace ID is nearly useless in a distributed system:
// you can see that something failed and you cannot see what else was part
// of the same request. Making the correlation automatic is the only way it
// survives contact with a codebase — asking people to remember is asking
// them to fail during the incident where it matters.
//
// The catch is that it only works through the *Context variants. slog.Info
// has no context to read a span from, so it silently drops correlation and
// nobody notices until an incident. Enforce the context variants with a
// linter; sloglint has a rule for exactly this.
type ContextHandler struct{ slog.Handler }

// Handle adds trace_id and span_id when the context carries a valid span.
func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs preserves the wrapper. Without this override, slog.With would
// return the bare inner handler and correlation would quietly stop for
// every logger derived from this one — a bug that looks like "some log
// lines have trace IDs and some do not".
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup preserves the wrapper, for the same reason.
func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithGroup(name)}
}

// LogConfig describes the logger.
type LogConfig struct {
	Level  slog.Level
	Format string // "json" or "text"
	// Writer defaults to stderr. Logs go to stderr rather than stdout so
	// that a program which also writes data to stdout stays parseable.
	Writer io.Writer
}

// NewLogger builds the framework's logger: structured, trace-correlated,
// and carrying the service identity on every line.
func NewLogger(cfg LogConfig, serviceName, version, environment string) *slog.Logger {
	w := cfg.Writer
	if w == nil {
		w = os.Stderr
	}

	opts := &slog.HandlerOptions{Level: cfg.Level}

	var base slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}

	handler := ContextHandler{Handler: base}
	return slog.New(handler).With(
		slog.String("service", serviceName),
		slog.String("version", version),
		slog.String("env", environment),
	)
}

// ParseLevel maps a config string to a slog level, rejecting anything else
// rather than silently defaulting — a service started with LOG_LEVEL=warning
// should say so, not run at info and leave someone puzzled.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if s == "" {
		return slog.LevelInfo, nil
	}
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, err
	}
	return l, nil
}
