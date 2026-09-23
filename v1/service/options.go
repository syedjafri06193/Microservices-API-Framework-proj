package service

import (
	"context"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	"github.com/syedjafri06193/microservices-api-framework/interceptor"
)

type handlerRegistration struct {
	pattern string
	handler http.Handler
}

type options struct {
	logger            *slog.Logger
	handlers          []handlerRegistration
	startupHooks      []Hook
	dependencies      []namedCloser
	extraInterceptors []connect.Interceptor
	authenticator     interceptor.Authenticator
	publicProcedures  map[string]bool
	validator         interceptor.Validator
	// deferredServices are registered after the interceptor chain exists,
	// since the generated constructors take the options by value.
	deferredServices []func(...connect.HandlerOption) (string, http.Handler)
}

func defaultOptions() options {
	return options{publicProcedures: map[string]bool{}}
}

// Option configures a Service.
type Option func(*options)

// WithConnectHandler registers a Connect handler.
//
// Connect's generated constructors return (path, handler), so this composes
// directly with them:
//
//	service.WithConnectHandler(userv1connect.NewUserServiceHandler(impl))
//
// Note that a handler registered this way does *not* get the framework's
// interceptors, because they have to be passed to the generated constructor
// itself. Use Service.HandlerOptions() for that, or WithConnectService
// below, which wires it for you.
func WithConnectHandler(pattern string, handler http.Handler) Option {
	return func(o *options) {
		o.handlers = append(o.handlers, handlerRegistration{pattern, handler})
	}
}

// WithConnectService registers a handler built from the framework's
// interceptor chain.
//
// `build` is the generated constructor, called with the framework's options
// appended. This is the form that gets the ordering right without the user
// having to remember to ask for it:
//
//	service.WithConnectService(func(opts ...connect.HandlerOption) (string, http.Handler) {
//	    return userv1connect.NewUserServiceHandler(impl, opts...)
//	})
func WithConnectService(build func(...connect.HandlerOption) (string, http.Handler)) Option {
	return func(o *options) { o.deferredServices = append(o.deferredServices, build) }
}

// WithStartupHook adds a function that must succeed before the service
// starts accepting traffic: a cache warm, a migration check, a leader
// election. A hook that fails aborts startup, which is the point — a
// service that starts anyway has converted a deploy-time failure into a
// user-facing one.
func WithStartupHook(name string, run func(context.Context) error) Option {
	return func(o *options) { o.startupHooks = append(o.startupHooks, Hook{Name: name, Run: run}) }
}

// WithDependency registers something to close during shutdown, after the
// drain. Dependencies are closed in reverse registration order, so a pool
// registered before the thing that uses it outlives it.
func WithDependency(name string, close func(context.Context) error) Option {
	return func(o *options) {
		o.dependencies = append(o.dependencies, namedCloser{name: name, close: close})
	}
}

// WithLogger replaces the framework's logger. The replacement should wrap
// telemetry.ContextHandler, or log lines lose their trace correlation.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithInterceptor adds a user interceptor. It runs inside the framework's
// observability and outside the handler, so it is measured by the metrics
// interceptor and protected by the recovery interceptor.
func WithInterceptor(i connect.Interceptor) Option {
	return func(o *options) { o.extraInterceptors = append(o.extraInterceptors, i) }
}

// WithAuthenticator enables the auth interceptor.
func WithAuthenticator(a interceptor.Authenticator) Option {
	return func(o *options) { o.authenticator = a }
}

// WithPublicProcedure exempts a procedure from authentication.
//
// Matched exactly, never by prefix. A prefix match on "/pkg.Service/Get"
// would also exempt "/pkg.Service/GetAdminSecrets", and an authentication
// bypass that comes from a string prefix is how a CVE starts.
func WithPublicProcedure(procedures ...string) Option {
	return func(o *options) {
		for _, p := range procedures {
			o.publicProcedures[p] = true
		}
	}
}

// WithValidator enables request validation.
//
// protovalidate.Validator satisfies the interface directly:
//
//	v, _ := protovalidate.New()
//	service.WithValidator(v)
func WithValidator(v interceptor.Validator) Option {
	return func(o *options) { o.validator = v }
}
