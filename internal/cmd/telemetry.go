package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// TelemetryShutdown flushes pending exports and closes providers. Callers
// MUST invoke it before process exit or the last batch of metrics/spans is
// lost.
type TelemetryShutdown func(context.Context) error

// BuildTelemetry constructs an orch.Telemetry bound to the OTLP exporter
// configured by the flags. Empty OTelEndpoint → (nil, noopShutdown, nil);
// a misconfigured endpoint errors at startup rather than silently dropping
// telemetry later.
func BuildTelemetry(flags CLIFlags) (*orch.Telemetry, TelemetryShutdown, error) {
	if flags.OTelEndpoint == "" {
		return nil, noopShutdown, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("wonka-orchestrator"),
			semconv.ServiceNamespace("wonka-factory"),
		),
	)
	if err != nil {
		return nil, noopShutdown, fmt.Errorf("telemetry: resource: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	metricExp, err := buildMetricExporter(ctx, flags)
	if err != nil {
		return nil, noopShutdown, fmt.Errorf("telemetry: metric exporter: %w", err)
	}

	traceExp, err := buildTraceExporter(ctx, flags)
	if err != nil {
		// Partial init: release the metric exporter we already opened.
		_ = metricExp.Shutdown(context.Background())
		return nil, noopShutdown, fmt.Errorf("telemetry: trace exporter: %w", err)
	}

	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExp,
			metric.WithInterval(15*time.Second),
		)),
	)
	tp := trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithBatcher(traceExp),
	)

	telem, err := orch.NewTelemetry(mp, tp)
	if err != nil {
		_ = mp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
		return nil, noopShutdown, fmt.Errorf("telemetry: build: %w", err)
	}

	shutdown := func(ctx context.Context) error {
		// Force-flush before Shutdown so the final batch reaches the
		// collector; attempt all four operations so a partial failure
		// doesn't leak goroutines.
		var firstErr error
		record := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		record(mp.ForceFlush(ctx))
		record(tp.ForceFlush(ctx))
		record(mp.Shutdown(ctx))
		record(tp.Shutdown(ctx))
		return firstErr
	}
	return telem, shutdown, nil
}

func noopShutdown(context.Context) error { return nil }

func buildMetricExporter(ctx context.Context, flags CLIFlags) (metric.Exporter, error) {
	switch flags.OTelProtocol {
	case "", "grpc":
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(flags.OTelEndpoint),
		}
		if flags.OTelInsecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, opts...)
	case "http":
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(flags.OTelEndpoint),
		}
		if flags.OTelInsecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		return otlpmetrichttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown --otel-protocol %q (expected grpc or http)", flags.OTelProtocol)
	}
}

func buildTraceExporter(ctx context.Context, flags CLIFlags) (*otlptrace.Exporter, error) {
	switch flags.OTelProtocol {
	case "", "grpc":
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(flags.OTelEndpoint),
		}
		if flags.OTelInsecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	case "http":
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(flags.OTelEndpoint),
		}
		if flags.OTelInsecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown --otel-protocol %q (expected grpc or http)", flags.OTelProtocol)
	}
}
