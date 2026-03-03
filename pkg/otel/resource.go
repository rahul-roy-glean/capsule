package otel

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func buildResource(cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
	}

	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}

	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", cfg.Environment))
	}

	if cfg.InstanceID != "" {
		attrs = append(attrs, semconv.HostID(cfg.InstanceID))
	}

	if cfg.Zone != "" {
		attrs = append(attrs, semconv.CloudAvailabilityZone(cfg.Zone))
	}

	return resource.NewWithAttributes(semconv.SchemaURL, attrs...), nil
}
