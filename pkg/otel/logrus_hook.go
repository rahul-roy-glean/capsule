package otel

import (
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/trace"
)

// TraceCorrelationHook is a logrus hook that injects trace_id and span_id
// from the entry's context into log fields for log-trace correlation.
type TraceCorrelationHook struct{}

// Levels returns all log levels — we want trace correlation on every log line.
func (h *TraceCorrelationHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire extracts the span from entry.Context (set via logger.WithContext(ctx))
// and adds trace_id and span_id fields if a valid span is recording.
func (h *TraceCorrelationHook) Fire(entry *logrus.Entry) error {
	if entry.Context == nil {
		return nil
	}
	span := trace.SpanFromContext(entry.Context)
	if !span.SpanContext().IsValid() {
		return nil
	}
	sc := span.SpanContext()
	entry.Data["trace_id"] = sc.TraceID().String()
	entry.Data["span_id"] = sc.SpanID().String()
	if sc.IsSampled() {
		entry.Data["trace_flags"] = "01"
	}
	return nil
}
