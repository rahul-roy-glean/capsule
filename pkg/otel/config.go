package otel

import "os"

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
	cfg := Config{
		ServiceName: serviceName,
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
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
