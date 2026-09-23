// Package transport builds the two HTTP servers.
//
// There are always two, and separating them is not cosmetic.
//
//	8080 (main)   Connect / gRPC / gRPC-Web handlers, exposed to clients
//	9090 (admin)  /healthz /readyz /metrics /debug/pprof, cluster-internal
//
// Putting /metrics and pprof on the public port means a heap profile is one
// unauthenticated request away. It also means load shedding on the main
// port can take down your ability to observe the incident — which is the
// moment you need it. The admin port must stay responsive while the main
// port is saturated, and that requires separate http.Server instances with
// separate goroutine pools, not two routes on one mux.
package transport

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// ServerConfig describes one HTTP server.
type ServerConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	// WriteTimeout is deliberately left at zero for the main server. A
	// write deadline applies to the whole response, so on a streaming RPC
	// it cuts the stream off mid-flight regardless of how healthy it is.
	// Request deadlines are enforced by the timeout interceptor, which
	// understands the difference.
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	// MaxHeaderBytes bounds header size, so a malicious client cannot make
	// the process allocate arbitrarily before any handler runs.
	MaxHeaderBytes int
}

// DefaultMainServer returns the main server's settings.
func DefaultMainServer(addr string) ServerConfig {
	return ServerConfig{
		Addr: addr,
		// ReadHeaderTimeout, not ReadTimeout: it bounds the slowloris
		// window without capping how long a legitimate streaming request
		// may take to send its body.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// DefaultAdminServer returns the admin server's settings. These are
// tighter: nothing on the admin port streams, and everything on it should
// answer immediately or not at all.
func DefaultAdminServer(addr string) ServerConfig {
	return ServerConfig{
		Addr:              addr,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second, // pprof profiles take time
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}

// NewMainServer wraps a handler for h2c and returns an http.Server.
//
// h2c means HTTP/2 without TLS, which is what you want behind a mesh or a
// load balancer that terminates TLS for you. Without it, gRPC clients
// cannot talk to the server at all over plaintext — they require HTTP/2,
// and net/http will only negotiate it over TLS.
//
// This is also the whole of the dual-mode routing story. One handler serves
// gRPC, gRPC-Web and Connect's HTTP/JSON protocol on one port, because all
// three are HTTP: gRPC is a POST with content-type application/grpc,
// Connect-JSON is a POST with application/json, both to the same path.
// Routing is a content-type check inside one handler rather than a
// byte-level guess on a raw socket — which is why this needs no cmux and
// cannot fail the way protocol sniffing fails.
func NewMainServer(cfg ServerConfig, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h2c.NewHandler(handler, &http2.Server{}),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
}

// AdminOptions describes what the admin server exposes.
type AdminOptions struct {
	Health http.Handler
	// Metrics is the Prometheus handler, or nil.
	Metrics http.Handler
	// EnablePprof exposes /debug/pprof. Safe here precisely because this
	// is a separate, cluster-internal port.
	EnablePprof bool
	// Extra adds arbitrary admin routes — breaker state, cardinality
	// counts, a build-info endpoint.
	Extra map[string]http.Handler
}

// NewAdminServer builds the admin server.
func NewAdminServer(cfg ServerConfig, opts AdminOptions) *http.Server {
	mux := http.NewServeMux()

	if opts.Health != nil {
		mux.Handle("/healthz", opts.Health)
		mux.Handle("/readyz", opts.Health)
		mux.Handle("/startupz", opts.Health)
	}
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics)
	}
	if opts.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.Handle("GET /debug/vars", expvar.Handler())
	}
	for pattern, h := range opts.Extra {
		mux.Handle(pattern, h)
	}

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
}

// Listen opens a listener and reports a useful error when the port is
// taken. "bind: address already in use" with no port in it is a bad way to
// start a debugging session.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// Serve runs a server on a listener until it is shut down, translating the
// expected shutdown sentinel into a nil error.
func Serve(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown drains a server, forcing it closed if the drain times out.
//
// Forcing matters: Shutdown alone waits for idle connections forever if a
// client holds one open, and the process would sit there until the platform
// kills it — losing the telemetry flush that comes after.
func Shutdown(ctx context.Context, srv *http.Server) error {
	if err := srv.Shutdown(ctx); err != nil {
		closeErr := srv.Close()
		return errors.Join(err, closeErr)
	}
	return nil
}
