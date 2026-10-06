// Package otelgrizzle provides an OpenTelemetry trace instrumentation adapter for Grizzle.
// By isolating this in a separate subpackage, applications not using OpenTelemetry
// avoid downloading or compiling any OpenTelemetry dependencies.
package otelgrizzle

import (
	"context"

	"github.com/muandane/grizzle"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type otelTracer struct {
	tracer trace.Tracer
}

type otelSpan struct {
	span trace.Span
}

// End completes the OpenTelemetry span.
func (s *otelSpan) End() {
	if s.span != nil {
		s.span.End()
	}
}

// RecordError records an error on the OpenTelemetry span.
func (s *otelSpan) RecordError(err error) {
	if s.span != nil && err != nil {
		s.span.RecordError(err)
	}
}

// SetAttribute sets a key-value attribute on the OpenTelemetry span.
func (s *otelSpan) SetAttribute(key string, val any) {
	if s.span == nil {
		return
	}
	switch v := val.(type) {
	case string:
		s.span.SetAttributes(attribute.String(key, v))
	case int:
		s.span.SetAttributes(attribute.Int(key, v))
	case int64:
		s.span.SetAttributes(attribute.Int64(key, v))
	case bool:
		s.span.SetAttributes(attribute.Bool(key, v))
	case float64:
		s.span.SetAttributes(attribute.Float64(key, v))
	}
}

// Start begins a new child span.
func (t *otelTracer) Start(ctx context.Context, spanName string) (context.Context, grizzle.Span) {
	newCtx, span := t.tracer.Start(ctx, spanName)
	return newCtx, &otelSpan{span: span}
}

// New creates a grizzle.Tracer adapter from an OpenTelemetry trace.Tracer.
func New(t trace.Tracer) grizzle.Tracer {
	return &otelTracer{tracer: t}
}

// WithTracer returns a grizzle.Option configuring OpenTelemetry tracing.
func WithTracer(t trace.Tracer) grizzle.Option {
	return grizzle.WithTracer(New(t))
}
