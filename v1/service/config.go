package service

import (
	"fmt"
	"net"
	"time"

	"github.com/syedjafri06193/microservices-api-framework/internal/config"
	"github.com/syedjafri06193/microservices-api-framework/lifecycle"
)

// Config is the framework's configuration, read entirely from the
// environment.
//
// Two rules shape the names:
//
// **Reuse standard variables where they exist.** OTEL_EXPORTER_OTLP_ENDPOINT
// and OTEL_TRACES_SAMPLER_ARG are OTel spec variables. Inventing
// MYFW_TRACE_URL means the OTel SDK's own defaults stop working, every
// operator has to learn a second vocabulary, and a sidecar that configures
// the standard variables is silently ignored.
//
// **Validate at startup, exit non-zero with a precise message.** Not
// "invalid config" but `ENVIRONMENT must be one of [dev staging prod], got
// "production"`. A service that starts with a misconfiguration and fails on
// the first request is worse than one that refuses to start.
type Config struct {
	ServiceName string `env:"SERVICE_NAME,required"`
	Version     string `env:"SERVICE_VERSION" envDefault:"dev"`
	Environment string `env:"ENVIRONMENT,required" validate:"oneof=dev staging prod"`

	MainAddr  string `env:"MAIN_ADDR" envDefault:":8080"`
	AdminAddr string `env:"ADMIN_ADDR" envDefault:":9090"`

	// Telemetry. OTEL_* names are the specification's.
	TraceExporter string  `env:"OTEL_TRACES_EXPORTER" envDefault:"otlp" validate:"oneof=otlp stdout none"`
	OTLPEndpoint  string  `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTLPInsecure  bool    `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"false"`
	SampleRatio   float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"0.1" validate:"gte=0,lte=1"`
	EnableMetrics bool    `env:"METRICS_ENABLED" envDefault:"true"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json" validate:"oneof=json text"`

	// Lifecycle.
	PreStopDelay time.Duration `env:"PRESTOP_DELAY" envDefault:"5s"`
	DrainTimeout time.Duration `env:"DRAIN_TIMEOUT" envDefault:"25s"`
	// GracePeriod is the platform's terminationGracePeriodSeconds. Supply
	// it from the downward API so the framework can check the arithmetic at
	// startup instead of discovering it during a deploy.
	GracePeriod    time.Duration `env:"TERMINATION_GRACE_PERIOD"`
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"30s"`
	SlowRequest    time.Duration `env:"SLOW_REQUEST_THRESHOLD" envDefault:"1s"`

	// Resilience. Every feature here is switchable off with one flag,
	// because in a mesh deployment the sidecar already does this and doing
	// it twice is worse than not doing it at all.
	RetryEnabled   bool    `env:"RETRY_ENABLED" envDefault:"true"`
	RetryBudget    float64 `env:"RETRY_BUDGET" envDefault:"0.2" validate:"gte=0,lte=1"`
	RetryAttempts  int     `env:"RETRY_MAX_ATTEMPTS" envDefault:"2" validate:"gte=0,lte=5"`
	BreakerEnabled bool    `env:"BREAKER_ENABLED" envDefault:"true"`
	BreakerRatio   float64 `env:"BREAKER_FAILURE_RATIO" envDefault:"0.5" validate:"gte=0,lte=1"`
	BreakerMinReqs int     `env:"BREAKER_MIN_REQUESTS" envDefault:"20" validate:"gte=1"`

	ShedEnabled     bool          `env:"LOADSHED_ENABLED" envDefault:"true"`
	ShedMaxLatency  time.Duration `env:"LOADSHED_MAX_QUEUE_LATENCY" envDefault:"100ms"`
	ShedMaxInFlight int           `env:"LOADSHED_MAX_INFLIGHT" envDefault:"0" validate:"gte=0"`

	DeadlineBuffer  time.Duration `env:"DEADLINE_BUFFER" envDefault:"50ms"`
	DeadlineMinimum time.Duration `env:"DEADLINE_MINIMUM" envDefault:"20ms"`
}

// LoadConfig reads and validates the configuration.
func LoadConfig() (Config, error) {
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks the cross-field constraints that struct tags cannot see.
func (c Config) Validate() error {
	// Sharing a port would put /metrics and pprof on the public listener,
	// which means a heap profile is one unauthenticated request away — and
	// it means load shedding on the main port can take down your ability to
	// observe the incident.
	//
	// Port 0 is exempt: it asks the OS for an ephemeral port, so two
	// listeners configured identically still end up on different ports.
	// Tests rely on this, and rejecting it would be the framework refusing
	// a configuration that works.
	if c.MainAddr == c.AdminAddr && !endsInEphemeralPort(c.MainAddr) {
		return fmt.Errorf("MAIN_ADDR and ADMIN_ADDR must differ; both are %q", c.MainAddr)
	}

	if c.TraceExporter == "otlp" && c.OTLPEndpoint == "" {
		// The OTel SDK has its own default, but a service that silently
		// exports to localhost:4318 and finds nothing there looks healthy
		// and produces no traces, which is the worst of both.
		return fmt.Errorf(
			"OTEL_TRACES_EXPORTER is %q but OTEL_EXPORTER_OTLP_ENDPOINT is unset; "+
				"set the endpoint, or set OTEL_TRACES_EXPORTER=stdout for local development",
			c.TraceExporter)
	}

	timing := lifecycle.Timing{
		PreStopDelay: c.PreStopDelay,
		DrainTimeout: c.DrainTimeout,
		GracePeriod:  c.GracePeriod,
	}
	if err := timing.Validate(); err != nil {
		return err
	}

	if c.DeadlineMinimum >= c.RequestTimeout {
		return fmt.Errorf(
			"DEADLINE_MINIMUM (%v) is not below REQUEST_TIMEOUT (%v); no outbound call "+
				"could ever start", c.DeadlineMinimum, c.RequestTimeout)
	}

	return nil
}

// endsInEphemeralPort reports whether an address asks the OS to pick a port.
func endsInEphemeralPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port == "0"
}

// Timing returns the shutdown timings.
func (c Config) Timing() lifecycle.Timing {
	return lifecycle.Timing{
		PreStopDelay: c.PreStopDelay,
		DrainTimeout: c.DrainTimeout,
		GracePeriod:  c.GracePeriod,
	}
}
