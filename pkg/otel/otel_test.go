package otel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

func TestInitNoopWhenEndpointEmpty(t *testing.T) {
	ctx := context.Background()
	client, err := Init(ctx, Config{ServiceName: "test-service"})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	defer client.Shutdown(ctx)

	// Verify no-op providers
	if _, ok := client.TracerProvider.(nooptrace.TracerProvider); !ok {
		t.Error("expected noop TracerProvider when endpoint is empty")
	}
	if _, ok := client.MeterProvider.(noop.MeterProvider); !ok {
		t.Error("expected noop MeterProvider when endpoint is empty")
	}
}

func TestShutdownNoop(t *testing.T) {
	ctx := context.Background()
	client, err := Init(ctx, Config{ServiceName: "test-service"})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

func TestTracerAndMeter(t *testing.T) {
	ctx := context.Background()
	client, err := Init(ctx, Config{ServiceName: "test-service"})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	defer client.Shutdown(ctx)

	tracer := client.Tracer("test")
	if tracer == nil {
		t.Error("Tracer returned nil")
	}

	meter := client.Meter("test")
	if meter == nil {
		t.Error("Meter returned nil")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("INSTANCE_ID", "123")
	t.Setenv("ZONE", "us-central1-a")

	cfg := ConfigFromEnv("my-service")

	if cfg.ServiceName != "my-service" {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, "my-service")
	}
	if cfg.Endpoint != "localhost:4317" {
		t.Errorf("Endpoint = %q, want %q", cfg.Endpoint, "localhost:4317")
	}
	if cfg.Environment != "test" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "test")
	}
	if cfg.InstanceID != "123" {
		t.Errorf("InstanceID = %q, want %q", cfg.InstanceID, "123")
	}
	if cfg.Zone != "us-central1-a" {
		t.Errorf("Zone = %q, want %q", cfg.Zone, "us-central1-a")
	}
}
