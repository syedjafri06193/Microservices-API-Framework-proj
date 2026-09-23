// Package fwtest gives users of the framework the test helpers they would
// otherwise have to write themselves.
//
// This is not an afterthought. A framework that makes its users' tests
// harder will not be adopted, however good its runtime behaviour is. Being
// able to write
//
//	srv := fwtest.NewServer(t, handler)
//
// and then assert on the spans it emitted is a feature people will choose
// the framework for — telemetry is a contract, and contracts deserve tests.
package fwtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a running test server with an h2c listener, so gRPC clients
// work against it exactly as they would in production.
type Server struct {
	URL      string
	Recorder *SpanRecorder

	httptest *httptest.Server
}

// NewServer starts a server for a Connect handler.
//
// `build` is the generated constructor. The returned server is torn down
// through t.Cleanup, so a test never has to remember.
func NewServer(t testing.TB, build func(...connect.HandlerOption) (string, http.Handler), opts ...Option) *Server {
	t.Helper()

	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}

	s := &Server{}

	if cfg.recordSpans {
		s.Recorder = NewSpanRecorder(t)
		cfg.handlerOptions = append(cfg.handlerOptions,
			connect.WithInterceptors(s.Recorder.Interceptor()))
	}

	mux := http.NewServeMux()
	path, handler := build(cfg.handlerOptions...)
	mux.Handle(path, handler)

	// h2c, so a gRPC client can reach this server over plaintext. Without
	// it a test would silently exercise only the Connect protocol, and the
	// gRPC path — the one most likely to break — would go untested.
	s.httptest = httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	s.httptest.EnableHTTP2 = true
	s.httptest.Start()
	s.URL = s.httptest.URL

	t.Cleanup(s.httptest.Close)
	return s
}

// Client returns an HTTP client that speaks h2c to this server.
func (s *Server) Client() *http.Client { return s.httptest.Client() }

type config struct {
	handlerOptions []connect.HandlerOption
	recordSpans    bool
}

// Option configures a test server.
type Option func(*config)

// WithHandlerOptions passes Connect options to the handler — normally the
// framework's interceptor chain.
func WithHandlerOptions(opts ...connect.HandlerOption) Option {
	return func(c *config) { c.handlerOptions = append(c.handlerOptions, opts...) }
}

// WithSpanRecording attaches a SpanRecorder to the server.
func WithSpanRecording() Option {
	return func(c *config) { c.recordSpans = true }
}

// ---------------------------------------------------------------- spans

// SpanRecorder captures spans so a test can assert on them.
type SpanRecorder struct {
	exporter *tracetest.InMemoryExporter
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer
}

// NewSpanRecorder returns a recorder with its own TracerProvider.
//
// Its own, not the global one: tests run in parallel within a package, and
// a recorder that swapped the global provider would capture other tests'
// spans and lose its own.
func NewSpanRecorder(t testing.TB) *SpanRecorder {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		// AlwaysSample: a ratio sampler would make assertions flaky in
		// exactly the way tests must never be.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	return &SpanRecorder{
		exporter: exporter,
		provider: provider,
		tracer:   provider.Tracer("fwtest"),
	}
}

// Interceptor returns an interceptor that starts a span per RPC into this
// recorder.
func (r *SpanRecorder) Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, span := r.tracer.Start(ctx, req.Spec().Procedure)
			defer span.End()

			resp, err := next(ctx, req)
			if err != nil {
				span.RecordError(err)
				span.SetAttributes(attribute.String("rpc.code", connect.CodeOf(err).String()))
			}
			return resp, err
		}
	}
}

// Provider returns the recorder's TracerProvider, for code that needs to
// create spans of its own.
func (r *SpanRecorder) Provider() *sdktrace.TracerProvider { return r.provider }

// Spans returns everything recorded so far.
func (r *SpanRecorder) Spans() []Span {
	stubs := r.exporter.GetSpans()
	out := make([]Span, 0, len(stubs))
	for _, s := range stubs {
		out = append(out, Span{stub: s})
	}
	return out
}

// Reset clears the recorded spans.
func (r *SpanRecorder) Reset() { r.exporter.Reset() }

// Require returns the single span with the given name, failing the test if
// there is not exactly one.
func (r *SpanRecorder) Require(t testing.TB, name string) Span {
	t.Helper()
	var found []Span
	for _, s := range r.Spans() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no span named %q; recorded: %v", name, r.names())
	default:
		t.Fatalf("expected one span named %q, found %d", name, len(found))
	}
	return Span{}
}

func (r *SpanRecorder) names() []string {
	var out []string
	for _, s := range r.Spans() {
		out = append(out, s.Name())
	}
	return out
}

// Span is one recorded span.
type Span struct{ stub tracetest.SpanStub }

// Name returns the span name.
func (s Span) Name() string { return s.stub.Name }

// Attr returns a string attribute, or "".
func (s Span) Attr(key string) string {
	for _, kv := range s.stub.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// HasError reports whether an error was recorded on the span.
func (s Span) HasError() bool {
	if s.stub.Status.Code.String() == "Error" {
		return true
	}
	for _, e := range s.stub.Events {
		if e.Name == "exception" {
			return true
		}
	}
	return false
}

// Events returns the span's event names.
func (s Span) Events() []string {
	out := make([]string, 0, len(s.stub.Events))
	for _, e := range s.stub.Events {
		out = append(out, e.Name)
	}
	return out
}

// ------------------------------------------------------- fake upstream

// FakeUpstream is a dependency that can be told to misbehave.
//
// A resilience framework has to be tested against the failures it claims to
// handle. Asserting that a breaker opens requires something that can fail
// on demand, deterministically, a known number of times.
type FakeUpstream struct {
	mu sync.Mutex
	// failures is the number of remaining forced failures.
	failures int
	code     connect.Code
	delay    time.Duration
	calls    int
}

// NewFakeUpstream returns an upstream that succeeds until told otherwise.
func NewFakeUpstream() *FakeUpstream { return &FakeUpstream{} }

// FailNext makes the next n calls fail with the given code.
func (u *FakeUpstream) FailNext(n int, code connect.Code) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failures = n
	u.code = code
}

// FailAlways makes every call fail until Recover is called.
func (u *FakeUpstream) FailAlways(code connect.Code) {
	u.FailNext(1<<30, code)
}

// Recover stops forcing failures.
func (u *FakeUpstream) Recover() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failures = 0
}

// DelayBy makes every call sleep before returning.
func (u *FakeUpstream) DelayBy(d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.delay = d
}

// Calls returns the number of calls that reached the upstream — the number
// that matters when asserting that a breaker stopped traffic rather than
// merely returning errors.
func (u *FakeUpstream) Calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// Do records a call and returns whatever the upstream was told to.
func (u *FakeUpstream) Do(ctx context.Context) error {
	u.mu.Lock()
	u.calls++
	delay := u.delay
	var code connect.Code
	failing := u.failures > 0
	if failing {
		u.failures--
		code = u.code
	}
	u.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
		}
	}
	if failing {
		return connect.NewError(code, errFake)
	}
	return nil
}

type fakeError struct{}

func (fakeError) Error() string { return "fake upstream failure" }

var errFake = fakeError{}
