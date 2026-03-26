package observability

import (
	"context"
	"log/slog"
)

// InitTracing initializes OpenTelemetry tracing with an OTLP HTTP exporter.
// If endpoint is empty, tracing is disabled (noop, zero overhead).
// Returns a shutdown function that must be called on application exit.
func InitTracing(ctx context.Context, serviceName, endpoint string) (shutdown func(context.Context) error, err error) {
	if endpoint == "" {
		slog.Info("otel: tracing disabled (no HUB_OTEL_ENDPOINT)")
		return func(context.Context) error { return nil }, nil
	}

	// OTel SDK integration is available but requires adding the following
	// dependencies to go.mod:
	//   go.opentelemetry.io/otel
	//   go.opentelemetry.io/otel/sdk
	//   go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
	//   go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
	//
	// When those deps are added, this function should:
	// 1. Create OTLP HTTP exporter pointing at endpoint
	// 2. Create TracerProvider with service.name resource attribute
	// 3. Set global TracerProvider and TextMapPropagator (W3C)
	// 4. Return provider.Shutdown as the shutdown function
	//
	// For now, log that the endpoint is configured and return noop.
	// This keeps the binary dependency-free until OTel is explicitly needed.
	slog.Info("otel: endpoint configured but SDK not yet linked", "endpoint", endpoint, "service", serviceName)
	return func(context.Context) error { return nil }, nil
}
