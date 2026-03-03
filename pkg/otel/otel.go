package otel

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Client holds the configured OpenTelemetry providers.
type Client struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
	shutdown       func(context.Context) error
}

// Init initializes OpenTelemetry. When cfg.Endpoint is empty, it returns a
// no-op client so callers never need nil checks.
func Init(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return newNoopClient(), nil
	}

	res, err := buildResource(cfg)
	if err != nil {
		return nil, err
	}

	tp, err := newTracerProvider(ctx, cfg, res)
	if err != nil {
		return nil, err
	}

	mp, err := newMeterProvider(ctx, cfg, res)
	if err != nil {
		// Shut down the already-created tracer provider before returning.
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(propagator)

	c := &Client{
		TracerProvider: tp,
		MeterProvider:  mp,
		Propagator:     propagator,
		shutdown: func(ctx context.Context) error {
			return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
		},
	}

	return c, nil
}

// Shutdown flushes and shuts down all providers.
func (c *Client) Shutdown(ctx context.Context) error {
	if c.shutdown == nil {
		return nil
	}
	return c.shutdown(ctx)
}

// Tracer returns a named tracer from the configured provider.
func (c *Client) Tracer(name string) trace.Tracer {
	return c.TracerProvider.Tracer(name)
}

// Meter returns a named meter from the configured provider.
func (c *Client) Meter(name string) metric.Meter {
	return c.MeterProvider.Meter(name)
}
