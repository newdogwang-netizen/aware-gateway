package trace

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// ShutdownFunc flushes and shuts down the trace provider.
type ShutdownFunc func(ctx context.Context) error

// Config holds tracing configuration.
type Config struct {
	Enabled     bool
	Exporter    string // stdout | otlp | none
	Endpoint    string // OTLP collector endpoint (e.g. "localhost:4318")
	Insecure    bool   // use HTTP instead of HTTPS for OTLP
	ServiceName string
	SampleRate  float64
}

// Setup initializes the OpenTelemetry tracer provider.
// Supports stdout (local dev) and otlp (HTTP to Jaeger/Tempo/Grafana).
func Setup(cfg Config) (ShutdownFunc, error) {
	if !cfg.Enabled || cfg.Exporter == "none" || cfg.Exporter == "" {
		return func(ctx context.Context) error { return nil }, nil
	}

	var exporter sdktrace.SpanExporter
	var err error

	switch cfg.Exporter {
	case "stdout":
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("create stdout exporter: %w", err)
		}

	case "otlp":
		if cfg.Endpoint == "" {
			return nil, fmt.Errorf("otlp exporter requires tracing.endpoint")
		}
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		// Support custom URL paths (some collectors use /v1/traces)
		if strings.Contains(cfg.Endpoint, "/") {
			// Split host:port from path
			idx := strings.Index(cfg.Endpoint, "/")
			hostPort := cfg.Endpoint[:idx]
			urlPath := cfg.Endpoint[idx:]
			opts = []otlptracehttp.Option{
				otlptracehttp.WithEndpoint(hostPort),
				otlptracehttp.WithURLPath(urlPath),
			}
			if cfg.Insecure {
				opts = append(opts, otlptracehttp.WithInsecure())
			}
		}
		exporter, err = otlptracehttp.New(context.Background(), opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp exporter: %w", err)
		}

	default:
		slog.Warn("unsupported trace exporter, disabling tracing", "exporter", cfg.Exporter)
		return func(ctx context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		// Schema URL conflict between resource.Default() and our semconv version.
		// Fall back to just our resource — the default resource isn't critical.
		slog.Warn("resource merge conflict, using service-only resource", "error", err)
		res = resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRate)),
	)
	otel.SetTracerProvider(tp)

	slog.Info("tracing initialized",
		"exporter", cfg.Exporter,
		"endpoint", cfg.Endpoint,
		"service_name", cfg.ServiceName,
		"sample_rate", cfg.SampleRate,
	)

	return func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}, nil
}
