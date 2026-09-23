package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"

	"github.com/syedjafri06193/microservices-api-framework/interceptor"
	"github.com/syedjafri06193/microservices-api-framework/lifecycle"
	"github.com/syedjafri06193/microservices-api-framework/resilience/breaker"
	"github.com/syedjafri06193/microservices-api-framework/resilience/budget"
	"github.com/syedjafri06193/microservices-api-framework/resilience/deadline"
	"github.com/syedjafri06193/microservices-api-framework/resilience/retry"
	"github.com/syedjafri06193/microservices-api-framework/resilience/shed"
	"github.com/syedjafri06193/microservices-api-framework/telemetry"
	"github.com/syedjafri06193/microservices-api-framework/transport"
)

// Service is a configured, runnable service.
//
// The entire point of the framework is that a user's main() is about twenty
// lines: construct, register a handler, Run. But New is a convenience, not
// the only door — every part it assembles is exported and usable alone. See
// the escape hatches at the bottom of this file, and docs/escape-hatches.md.
type Service struct {
	cfg    Config
	logger *slog.Logger

	mux       *http.ServeMux
	providers *telemetry.Providers
	health    *lifecycle.Health

	breakers *breaker.Group
	budget   *budget.Budget
	shedder  *shed.Shedder
	metrics  *interceptor.Metrics
	guard    *telemetry.CardinalityGuard

	serverInterceptors []connect.Interceptor

	startupHooks []Hook
	dependencies []namedCloser
	extraAdmin   map[string]http.Handler

	mainServer  *http.Server
	adminServer *http.Server
}

// Hook runs during startup.
type Hook struct {
	Name string
	Run  func(context.Context) error
}

type namedCloser struct {
	name  string
	close func(context.Context) error
}

// New builds a Service from the environment plus options.
//
// It reads config, initializes OTel, builds the interceptor chain in the
// correct order, constructs the resilience primitives and prepares both
// servers. It does not listen — Run does that, so that a caller can inspect
// or extend the service first.
func New(opts ...Option) (*Service, error) {
	cfg, err := LoadConfig()
	if err != nil {
		// Exit non-zero with a precise message. A service that starts with
		// a misconfiguration and fails on the first request is worse than
		// one that refuses to start.
		return nil, err
	}
	return NewWithConfig(cfg, opts...)
}

// NewWithConfig builds a Service from an explicit config, skipping the
// environment. Used by tests and by anyone embedding the framework in a
// process that configures itself some other way.
func NewWithConfig(cfg Config, opts ...Option) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	options := defaultOptions()
	for _, o := range opts {
		o(&options)
	}

	level, err := telemetry.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("config: LOG_LEVEL: %w", err)
	}
	logger := options.logger
	if logger == nil {
		logger = telemetry.NewLogger(
			telemetry.LogConfig{Level: level, Format: cfg.LogFormat},
			cfg.ServiceName, cfg.Version, cfg.Environment)
	}

	s := &Service{
		cfg:          cfg,
		logger:       logger,
		mux:          http.NewServeMux(),
		health:       lifecycle.NewHealth(),
		startupHooks: options.startupHooks,
		dependencies: options.dependencies,
		extraAdmin:   map[string]http.Handler{},
	}

	// Telemetry first, so that failures during the rest of startup are
	// traced rather than invisible.
	providers, err := telemetry.Init(context.Background(), telemetry.Config{
		ServiceName:      cfg.ServiceName,
		ServiceVersion:   cfg.Version,
		Environment:      cfg.Environment,
		Exporter:         telemetry.ExporterKind(cfg.TraceExporter),
		OTLPEndpoint:     cfg.OTLPEndpoint,
		OTLPInsecure:     cfg.OTLPInsecure,
		SampleRatio:      cfg.SampleRatio,
		EnablePrometheus: cfg.EnableMetrics,
	})
	if err != nil {
		return nil, err
	}
	s.providers = providers

	s.guard = telemetry.NewCardinalityGuard(1000, 10000, func(metric string, series int) {
		logger.Warn("metric cardinality is high",
			slog.String("metric", metric),
			slog.Int("series", series),
			slog.String("hint", "check for an unbounded label; see docs/cardinality.md"))
	})

	if err := s.buildResilience(); err != nil {
		return nil, err
	}
	if err := s.buildInterceptors(options); err != nil {
		return nil, err
	}

	s.warnOnMeshOverlap()

	for _, h := range options.handlers {
		s.Handle(h.pattern, h.handler)
	}
	// Registered after the chain exists, so the generated constructor gets
	// the framework's interceptors rather than the user having to remember
	// to ask for them.
	for _, build := range options.deferredServices {
		pattern, handler := build(s.HandlerOptions()...)
		s.Handle(pattern, handler)
	}

	return s, nil
}

func (s *Service) buildResilience() error {
	var err error

	if s.cfg.BreakerEnabled {
		bcfg := breaker.DefaultConfig()
		bcfg.FailureRatio = s.cfg.BreakerRatio
		bcfg.MinimumRequests = s.cfg.BreakerMinReqs
		s.breakers, err = breaker.NewGroup(bcfg, nil, func(c breaker.StateChange) {
			// A breaker changing state is one of the highest-signal events
			// a service emits, and it is worth a line at info with the
			// numbers that caused it.
			s.logger.Info("circuit breaker state change",
				slog.String("key", c.Key),
				slog.String("from", c.From.String()),
				slog.String("to", c.To.String()),
				slog.Int("failures", c.Failures),
				slog.Int("total", c.Total))
		})
		if err != nil {
			return fmt.Errorf("breaker: %w", err)
		}
	}

	if s.cfg.RetryEnabled {
		bcfg := budget.DefaultConfig()
		bcfg.Ratio = s.cfg.RetryBudget
		s.budget, err = budget.New(bcfg, nil)
		if err != nil {
			return fmt.Errorf("retry budget: %w", err)
		}
	}

	if s.cfg.ShedEnabled {
		s.shedder, err = shed.New(shed.Config{
			MaxQueueLatency: s.cfg.ShedMaxLatency,
			MaxInFlight:     s.cfg.ShedMaxInFlight,
		})
		if err != nil {
			return fmt.Errorf("load shedder: %w", err)
		}
	}

	return nil
}

func (s *Service) buildInterceptors(options options) error {
	otelInterceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTrustRemote(),
	)
	if err != nil {
		return fmt.Errorf("otelconnect: %w", err)
	}

	meter := otel.Meter("github.com/syedjafri06193/microservices-api-framework")
	metrics, err := interceptor.NewMetrics(meter,
		telemetry.ServiceAttrs(telemetry.Config{
			ServiceName: s.cfg.ServiceName,
			Environment: s.cfg.Environment,
		}), s.guard)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	s.metrics = metrics

	chain := interceptor.ServerChain{
		PanicGuard: interceptor.PanicGuard(s.logger),
		Tracing:    otelInterceptor,
		Logging:    interceptor.Logging(s.logger, s.cfg.SlowRequest),
		Metrics:    metrics.Interceptor(),
		Timeout:    interceptor.Timeout(s.cfg.RequestTimeout),
		Recovery:   interceptor.Recovery(s.logger),
		Extra:      options.extraInterceptors,
	}
	if s.shedder != nil {
		chain.LoadShed = interceptor.LoadShed(s.shedder, nil)
	}
	if options.authenticator != nil {
		chain.Auth = interceptor.Auth(options.authenticator, options.publicProcedures)
	}
	if options.validator != nil {
		chain.Validation = interceptor.Validation(options.validator)
	}

	s.serverInterceptors = chain.Interceptors()
	return nil
}

// warnOnMeshOverlap emits the single most valuable startup line the
// framework has.
//
// If the app retries three times and a mesh sidecar retries three times,
// one logical call becomes nine requests per hop — 27x across three hops. A
// minor blip cascades, and the people debugging it see a traffic spike with
// no obvious source, because no individual layer is misbehaving.
//
// The framework warns rather than disabling anything. Guessing wrong about
// which layer should own retries would be worse than either answer, and an
// operator who reads this line can make the call in one environment
// variable.
func (s *Service) warnOnMeshOverlap() {
	if !s.cfg.RetryEnabled {
		return
	}
	mesh, found := lifecycle.DetectMesh()
	if !found {
		return
	}
	s.logger.Warn(
		"retries are enabled in-process AND a service mesh sidecar was detected; "+
			"this may amplify load during a partial outage",
		slog.String("mesh", mesh),
		slog.String("hint", "set RETRY_ENABLED=false to let the mesh own retries, "+
			"or disable retries in the mesh; see docs/service-mesh.md"))
}

// ---------------------------------------------------------------- public

// Handle registers a Connect handler, or any other http.Handler.
//
// The Connect handler constructors return (path, handler), so this is
// usually `s.Handle(userv1connect.NewUserServiceHandler(impl, s.HandlerOptions()...))`.
func (s *Service) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

// HandlerOptions returns the Connect options every handler should be built
// with — principally the correctly ordered interceptor chain.
func (s *Service) HandlerOptions(extra ...connect.HandlerOption) []connect.HandlerOption {
	opts := []connect.HandlerOption{connect.WithInterceptors(s.serverInterceptors...)}
	return append(opts, extra...)
}

// Run starts both servers and blocks until a signal arrives, then drains.
//
// The startup order is not arbitrary:
//
//  1. validate config          (done in New)
//  2. initialize telemetry     (done in New, so startup failures are traced)
//  3. start the admin server   /healthz passes, /readyz does NOT yet
//  4. run startup hooks        cache warm, migrations check, leader election
//  5. start the main server
//  6. flip /readyz to ready
//  7. block
//
// The admin server comes up before the main one so a startup probe can
// observe progress, and readiness only flips after the main listener is
// actually accepting — flipping it earlier sends traffic to a connection
// refused.
func (s *Service) Run(ctx context.Context) error {
	ctx, stop := lifecycle.NotifyContext(ctx)
	defer stop()

	adminLn, err := transport.Listen(s.cfg.AdminAddr)
	if err != nil {
		return err
	}
	s.adminServer = transport.NewAdminServer(
		transport.DefaultAdminServer(s.cfg.AdminAddr), s.adminOptions())

	adminErr := make(chan error, 1)
	go func() { adminErr <- transport.Serve(s.adminServer, adminLn) }()
	s.logger.InfoContext(ctx, "admin server listening", slog.String("addr", s.cfg.AdminAddr))

	for _, hook := range s.startupHooks {
		start := time.Now()
		if err := hook.Run(ctx); err != nil {
			// Fail fast and loudly. A service that starts with a
			// half-warmed cache or an unreachable database and discovers it
			// on the first request has turned a deploy-time failure into a
			// user-facing one.
			return fmt.Errorf("startup hook %q: %w", hook.Name, err)
		}
		s.logger.InfoContext(ctx, "startup hook complete",
			slog.String("hook", hook.Name),
			slog.Duration("duration", time.Since(start)))
	}
	s.health.SetStarted(true)

	mainLn, err := transport.Listen(s.cfg.MainAddr)
	if err != nil {
		return err
	}
	s.mainServer = transport.NewMainServer(
		transport.DefaultMainServer(s.cfg.MainAddr), s.mux)

	mainErr := make(chan error, 1)
	go func() { mainErr <- transport.Serve(s.mainServer, mainLn) }()
	s.logger.InfoContext(ctx, "main server listening", slog.String("addr", s.cfg.MainAddr))

	// Only now. The listener is accepting, so traffic sent here will be
	// answered.
	s.health.SetReady(true)
	s.logger.InfoContext(ctx, "service ready",
		slog.String("service", s.cfg.ServiceName),
		slog.String("version", s.cfg.Version),
		slog.String("env", s.cfg.Environment))

	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "signal received, shutting down")
	case err := <-mainErr:
		if err != nil {
			s.logger.ErrorContext(ctx, "main server failed", slog.String("error", err.Error()))
			return errors.Join(err, s.Shutdown(ctx))
		}
	case err := <-adminErr:
		if err != nil {
			s.logger.ErrorContext(ctx, "admin server failed", slog.String("error", err.Error()))
			return errors.Join(err, s.Shutdown(ctx))
		}
	}

	return s.Shutdown(ctx)
}

// Shutdown runs the drain sequence.
func (s *Service) Shutdown(ctx context.Context) error {
	seq := lifecycle.NewSequencer(s.logger,
		lifecycle.Step{
			Name: "fail readiness",
			Run: func(context.Context) error {
				// First, always. Load balancers begin removing this
				// instance from rotation the moment /readyz starts failing.
				s.health.SetReady(false)
				return nil
			},
		},
		lifecycle.Step{
			Name: "wait for endpoint removal to propagate",
			Run: func(ctx context.Context) error {
				// Not a hack, and not optional. Endpoint removal is
				// eventually consistent across kube-proxy, every ingress
				// and every sidecar, and there is no signal that says "they
				// have all stopped sending you traffic". So you wait for as
				// long as your platform takes.
				select {
				case <-time.After(s.cfg.PreStopDelay):
				case <-ctx.Done():
				}
				return nil
			},
		},
		lifecycle.Step{
			Name:            "drain the main server",
			ContinueOnError: true,
			Run: func(ctx context.Context) error {
				if s.mainServer == nil {
					return nil
				}
				drainCtx, cancel := context.WithTimeout(ctx, s.cfg.DrainTimeout)
				defer cancel()
				return transport.Shutdown(drainCtx, s.mainServer)
			},
		},
		lifecycle.Step{
			Name:            "close dependencies",
			ContinueOnError: true,
			Run: func(ctx context.Context) error {
				// After the drain: a dependency closed while requests are
				// still in flight turns a clean shutdown into a burst of
				// errors on the way out.
				var errs []error
				for i := len(s.dependencies) - 1; i >= 0; i-- {
					d := s.dependencies[i]
					if err := d.close(ctx); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", d.name, err))
					}
				}
				return errors.Join(errs...)
			},
		},
		lifecycle.Step{
			Name:            "drain the admin server",
			ContinueOnError: true,
			Run: func(ctx context.Context) error {
				if s.adminServer == nil {
					return nil
				}
				// Last of the servers, so that a scrape or a probe can
				// still reach this instance while the main port drains.
				drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return transport.Shutdown(drainCtx, s.adminServer)
			},
		},
		lifecycle.Step{
			Name:            "flush telemetry",
			ContinueOnError: true,
			Run: func(ctx context.Context) error {
				// Last. Flushing earlier loses every span from the requests
				// that were still draining, which are exactly the spans
				// that explain a shutdown-time incident.
				if s.providers == nil {
					return nil
				}
				flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return s.providers.Shutdown(flushCtx)
			},
		},
	)

	// The sequencer detaches from cancellation itself, so a shutdown
	// triggered by a cancelled context still gets to run.
	return seq.Run(ctx, s.cfg.Timing().Total()+10*time.Second)
}

func (s *Service) adminOptions() transport.AdminOptions {
	opts := transport.AdminOptions{
		Health:      s.health.Handler(),
		EnablePprof: true,
		Extra:       map[string]http.Handler{},
	}

	if s.providers != nil && s.providers.PrometheusRegistry != nil {
		opts.Metrics = promhttp.HandlerFor(s.providers.PrometheusRegistry, promhttp.HandlerOpts{})
	}

	// Breaker state and cardinality counts on the admin port. Both are the
	// sort of thing you want during an incident and cannot get afterwards.
	opts.Extra["GET /debug/breakers"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := map[string]string{}
		if s.breakers != nil {
			for k, v := range s.breakers.Snapshot() {
				state[k] = v.String()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})
	opts.Extra["GET /debug/cardinality"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.guard.Snapshot())
	})

	for k, v := range s.extraAdmin {
		opts.Extra[k] = v
	}
	return opts
}

// --------------------------------------------------------- escape hatches
//
// Every framework that gets adopted can be partially abandoned. Teams adopt
// tools they can walk away from incrementally, so each of these is usable
// with none of the rest.

// Mux returns the raw ServeMux, for arbitrary handlers.
func (s *Service) Mux() *http.ServeMux { return s.mux }

// ServerInterceptors returns the correctly ordered server chain, for use
// with a server this framework did not build.
//
// This is realistically the most reusable thing here: someone who wants
// nothing else can take this one slice and get the ordering right.
func (s *Service) ServerInterceptors() []connect.Interceptor { return s.serverInterceptors }

// ClientInterceptors returns the client chain for calling a named peer.
func (s *Service) ClientInterceptors(target string) ([]connect.Interceptor, error) {
	return s.clientChain(target)
}

// Health returns the health tracker, so a caller can register local checks.
func (s *Service) Health() *lifecycle.Health { return s.health }

// Logger returns the framework's logger.
func (s *Service) Logger() *slog.Logger { return s.logger }

// Config returns the loaded configuration.
func (s *Service) Config() Config { return s.cfg }

// Breakers returns the breaker group, or nil when breakers are disabled.
func (s *Service) Breakers() *breaker.Group { return s.breakers }

// AdminHandle adds a route to the admin server. Must be called before Run.
func (s *Service) AdminHandle(pattern string, h http.Handler) { s.extraAdmin[pattern] = h }

func (s *Service) clientChain(target string) ([]connect.Interceptor, error) {
	otelInterceptor, err := otelconnect.NewInterceptor(otelconnect.WithTrustRemote())
	if err != nil {
		return nil, err
	}

	d := deadline.Budget{
		Buffer:  s.cfg.DeadlineBuffer,
		Minimum: s.cfg.DeadlineMinimum,
		OnMissing: func(procedure string) {
			s.logger.Warn("outbound call has no deadline",
				slog.String("procedure", procedure),
				slog.String("hint", "an unbounded call path; see docs/resilience.md"))
		},
	}

	var retrier *retry.Retrier
	if s.cfg.RetryEnabled {
		rcfg := retry.DefaultConfig()
		rcfg.MaxAttempts = s.cfg.RetryAttempts
		retrier, err = retry.New(rcfg, retry.NewPolicy(), s.budget, d, nil)
		if err != nil {
			return nil, err
		}
	}

	res := interceptor.Resilience{Target: target, Retrier: retrier, Breakers: s.breakers}

	chain := interceptor.ClientChain{
		Tracing:    otelInterceptor,
		Resilience: res.Interceptor(),
	}
	return chain.Interceptors(), nil
}
