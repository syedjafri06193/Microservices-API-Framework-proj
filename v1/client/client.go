// Package client builds outbound Connect clients with the framework's
// resilience and observability already attached.
//
// One of the four escape hatches: usable with none of the rest of the
// framework. A team that wants nothing but a correctly configured outbound
// client can take this and leave everything else.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/syedjafri06193/microservices-api-framework/interceptor"
	"github.com/syedjafri06193/microservices-api-framework/resilience"
	"github.com/syedjafri06193/microservices-api-framework/resilience/breaker"
	"github.com/syedjafri06193/microservices-api-framework/resilience/budget"
	"github.com/syedjafri06193/microservices-api-framework/resilience/deadline"
	"github.com/syedjafri06193/microservices-api-framework/resilience/retry"
)

// Config describes an outbound client.
type Config struct {
	// Target is the peer's name, used as the breaker key and as a metric
	// label. It comes from configuration, never from a request, so a
	// hostile caller cannot mint breaker entries or metric series.
	Target string

	// Timeout is a backstop on the whole call. It is not a substitute for
	// deadlines: a per-call deadline from the caller is what the deadline
	// budget works with, and this only catches calls that arrive without
	// one.
	Timeout time.Duration

	Retry    retry.Config
	Budget   budget.Config
	Breaker  breaker.Config
	Deadline deadline.Budget

	// DisableRetry and DisableBreaker turn the two mechanisms off
	// individually, which is what a mesh deployment wants.
	DisableRetry   bool
	DisableBreaker bool

	// Clock is injectable for tests.
	Clock resilience.Clock

	// OnBreakerChange is called when a breaker opens or closes. It should
	// be cheap and non-blocking: it runs while the breaker's lock is held.
	OnBreakerChange func(breaker.StateChange)
}

// DefaultConfig returns a client configuration for a named peer.
func DefaultConfig(target string) Config {
	return Config{
		Target:   target,
		Timeout:  30 * time.Second,
		Retry:    retry.DefaultConfig(),
		Budget:   budget.DefaultConfig(),
		Breaker:  breaker.DefaultConfig(),
		Deadline: deadline.DefaultBudget(),
	}
}

// Client holds an HTTP client and the Connect options to use with it.
type Client struct {
	HTTPClient *http.Client
	Options    []connect.ClientOption

	breakers *breaker.Group
	budget   *budget.Budget
}

// New builds a client for one peer.
//
// Use it with any generated Connect constructor:
//
//	c, _ := client.New(client.DefaultConfig("payments"))
//	payments := paymentv1connect.NewPaymentServiceClient(
//	    c.HTTPClient, addr, c.Options...)
func New(cfg Config) (*Client, error) {
	if cfg.Target == "" {
		return nil, fmt.Errorf("client: Target is required; it keys the breaker and labels metrics")
	}

	c := &Client{HTTPClient: NewHTTPClient(cfg.Timeout)}

	var retrier *retry.Retrier
	if !cfg.DisableRetry {
		b, err := budget.New(cfg.Budget, cfg.Clock)
		if err != nil {
			return nil, fmt.Errorf("client: retry budget: %w", err)
		}
		c.budget = b

		retrier, err = retry.New(cfg.Retry, retry.NewPolicy(), b, cfg.Deadline, nil)
		if err != nil {
			return nil, fmt.Errorf("client: retry: %w", err)
		}
	}

	if !cfg.DisableBreaker {
		g, err := breaker.NewGroup(cfg.Breaker, cfg.Clock, cfg.OnBreakerChange)
		if err != nil {
			return nil, fmt.Errorf("client: breaker: %w", err)
		}
		c.breakers = g
	}

	res := interceptor.Resilience{
		Target:   cfg.Target,
		Retrier:  retrier,
		Breakers: c.breakers,
	}
	chain := interceptor.ClientChain{Resilience: res.Interceptor()}
	c.Options = []connect.ClientOption{connect.WithInterceptors(chain.Interceptors()...)}

	return c, nil
}

// Breakers returns the breaker group, for an admin endpoint or a test.
func (c *Client) Breakers() *breaker.Group { return c.breakers }

// Budget returns the retry budget, or nil when retries are disabled.
func (c *Client) Budget() *budget.Budget { return c.budget }

// NewHTTPClient returns an HTTP client suitable for Connect and gRPC.
//
// The transport is the part worth getting right, and the defaults are wrong
// for a service that talks to a handful of peers at high volume:
//
//   - MaxIdleConnsPerHost defaults to 2. A service making hundreds of
//     concurrent calls to one peer spends its time in TCP and TLS
//     handshakes, and the symptom is latency that looks like the
//     dependency being slow.
//   - ForceAttemptHTTP2 is required for gRPC over TLS. Without it the
//     transport negotiates HTTP/1.1 and gRPC simply does not work.
//
// For h2c — plaintext HTTP/2, which is what you use behind a mesh or a load
// balancer that terminates TLS — use NewH2CClient instead.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// NewH2CClient returns a client that speaks plaintext HTTP/2.
//
// Needed whenever the peer is served by h2c — which is what this framework's
// own transport does, and what you want behind a sidecar or an ingress that
// has already terminated TLS.
func NewH2CClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			// Prior knowledge: speak HTTP/2 immediately rather than
			// negotiating it, because there is no TLS layer to negotiate in.
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
		Timeout: timeout,
	}
}
