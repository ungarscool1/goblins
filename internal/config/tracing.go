package config

import (
	"time"

	"github.com/ungarscool1/goblins/internal/observability"
)

// Tracing sends spans to an OpenTelemetry collector over OTLP/HTTP. Without an endpoint,
// spans are only logged.
type Tracing struct {
	// OTLPEndpoint is the collector's base URL, such as http://collector:4318.
	OTLPEndpoint string `yaml:"otlp_endpoint,omitempty"`
	// HeadersEnv names a variable holding headers for the collector, in the
	// OTEL_EXPORTER_OTLP_HEADERS form (key=value,key=value). They can hold a login.
	HeadersEnv string        `yaml:"headers_env,omitempty"`
	Interval   time.Duration `yaml:"interval,omitempty"`
}

func (t Tracing) validate(add func(string, ...any)) {
	if t.OTLPEndpoint != "" {
		if _, err := observability.TracesURL(t.OTLPEndpoint); err != nil {
			add("tracing.otlp_endpoint: %v", err)
		}
	}
	if t.OTLPEndpoint == "" && t.HeadersEnv != "" {
		add("tracing.headers_env: needs tracing.otlp_endpoint")
	}
	if t.Interval < 0 {
		add("tracing.interval: cannot be negative")
	}
}
