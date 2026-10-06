// Package otelgrizzle provides an OpenTelemetry trace instrumentation adapter for Grizzle.
//
// By isolating this in a separate subpackage module, applications not using OpenTelemetry
// avoid downloading or compiling any OpenTelemetry dependencies.
//
// # Usage
//
// Pass WithTracer with an OpenTelemetry trace.Tracer to grizzle.Sync or grizzle.PlanDiff:
//
//	tp := otel.GetTracerProvider()
//	tracer := tp.Tracer("my-service")
//
//	err := grizzle.Sync(ctx, db, grizzle.Options{
//	    SchemaSQL: schemaSQL,
//	}, otelgrizzle.WithTracer(tracer))
package otelgrizzle
