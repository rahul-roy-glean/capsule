package otel

import (
	"os"
	"strings"
)

// Config holds the configuration for OpenTelemetry initialization.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	InstanceID     string
	Zone           string
	Endpoint       string // from OTEL_EXPORTER_OTLP_ENDPOINT
}

// ConfigFromEnv creates a Config by reading standard environment variables.
func ConfigFromEnv(serviceName string) Config {
	// The gRPC WithEndpoint option expects a bare host:port (e.g.
	// "10.0.16.17:4317"), not a URL with a scheme. Strip http:// or https://
	// so the dialer doesn't produce "too many colons in address" errors.
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")

	cfg := Config{
		ServiceName: serviceName,
		Endpoint:    endpoint,
		Environment: os.Getenv("ENVIRONMENT"),
	}

	cfg.InstanceID = os.Getenv("INSTANCE_ID")
	if cfg.InstanceID == "" {
		cfg.InstanceID = os.Getenv("GCE_INSTANCE_ID")
	}

	cfg.Zone = os.Getenv("ZONE")
	if cfg.Zone == "" {
		cfg.Zone = os.Getenv("GCE_ZONE")
	}

	return cfg
}
