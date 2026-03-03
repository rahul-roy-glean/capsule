package otel

import (
	"context"

	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

func newNoopClient() *Client {
	return &Client{
		TracerProvider: nooptrace.NewTracerProvider(),
		MeterProvider:  noopmetric.NewMeterProvider(),
		Propagator:     propagation.NewCompositeTextMapPropagator(),
		shutdown:       func(context.Context) error { return nil },
	}
}
