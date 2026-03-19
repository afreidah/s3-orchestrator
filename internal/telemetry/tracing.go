// -------------------------------------------------------------------------------
// Tracing - OpenTelemetry Instrumentation
//
// Author: Alex Freidah
//
// OpenTelemetry tracer setup and span helpers. Supports OTLP export to collectors
// like Jaeger, Tempo, or any OTLP-compatible backend. Provides context propagation
// for distributed tracing across service boundaries.
// -------------------------------------------------------------------------------

package telemetry

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

const (
	// TracerName identifies spans created by this service.
	TracerName = "s3-orchestrator"
)

// Version of the service for trace metadata. Set at build time via
// -ldflags "-X github.com/afreidah/s3-orchestrator/internal/telemetry.Version=..."
var Version = "dev"

// -------------------------------------------------------------------------
// TRACER SETUP
// -------------------------------------------------------------------------

// InitTracer initializes the OpenTelemetry tracer with OTLP export. Returns a
// shutdown function that should be called on service termination to flush spans.
func InitTracer(ctx context.Context, cfg config.TracingConfig) (func(context.Context) error, error) {
	if !cfg.Enabled {
		// Return no-op shutdown when tracing is disabled
		return func(context.Context) error { return nil }, nil
	}

	// --- Create OTLP exporter ---
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// --- Create resource with service info ---
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(TracerName),
			semconv.ServiceVersion(Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// --- Configure sampler ---
	var sampler sdktrace.Sampler
	switch {
	case cfg.SampleRate >= 1.0:
		sampler = sdktrace.AlwaysSample()
	case cfg.SampleRate <= 0:
		sampler = sdktrace.NeverSample()
	default:
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
	}

	// --- Create trace provider ---
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// --- Set global tracer provider ---
	otel.SetTracerProvider(tp)

	// --- Set propagator for context propagation ---
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// -------------------------------------------------------------------------
// SPAN HELPERS
// -------------------------------------------------------------------------

// Tracer returns the global tracer for this service.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// StartSpan creates a new internal span with the given name and attributes.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartServerSpan creates a span for inbound requests (HTTP handler entry points).
func StartServerSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...), trace.WithSpanKind(trace.SpanKindServer))
}

// StartClientSpan creates a span for outbound calls (backend S3 operations).
func StartClientSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...), trace.WithSpanKind(trace.SpanKindClient))
}

// -------------------------------------------------------------------------
// COMMON ATTRIBUTES
// -------------------------------------------------------------------------

// S3 orchestrator specific attribute keys.
var (
	AttrRequestID          = attribute.Key("s3o.request_id")
	AttrVirtualBucket      = attribute.Key("s3o.bucket.virtual")
	AttrBackendBucket      = attribute.Key("s3o.bucket.backend")
	AttrObjectKey          = attribute.Key("s3o.key")
	AttrBackendName        = attribute.Key("s3o.backend.name")
	AttrBackendEndpoint    = attribute.Key("s3o.backend.endpoint")
	AttrObjectSize         = attribute.Key("s3o.object.size")
	AttrContentType        = attribute.Key("s3o.object.content_type")
	AttrOperation          = attribute.Key("s3o.operation")
	AttrUploadID           = attribute.Key("s3o.upload_id")
	AttrPartNumber         = attribute.Key("s3o.part_number")
	AttrWriteFailover      = attribute.Key("s3o.write_failover")
	AttrFailoverAttempts   = attribute.Key("s3o.write_failover_attempts")
	AttrFailover           = attribute.Key("s3o.failover")
	AttrDegradedMode       = attribute.Key("s3o.degraded_mode")
	AttrCacheHit           = attribute.Key("s3o.cache_hit")
	AttrParallelBroadcast  = attribute.Key("s3o.parallel_broadcast")
)

// RequestAttributes returns common attributes for HTTP request spans.
func RequestAttributes(method, path, bucket, key, clientIP string) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method),
		semconv.URLPath(path),
		AttrVirtualBucket.String(bucket),
		AttrObjectKey.String(key),
		semconv.ClientAddress(clientIP),
	}
}

// BackendAttributes returns common attributes for backend operation spans.
func BackendAttributes(operation, backendName, endpoint, bucket, key string) []attribute.KeyValue {
	return []attribute.KeyValue{
		AttrOperation.String(operation),
		AttrBackendName.String(backendName),
		AttrBackendEndpoint.String(endpoint),
		AttrBackendBucket.String(bucket),
		AttrObjectKey.String(key),
	}
}
