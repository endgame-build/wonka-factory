package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"go.opentelemetry.io/otel"
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
// malformed flags (unknown --otel-protocol, --otel-insecure against a
// non-loopback host) fail here at startup. Note that network reachability
// is NOT checked at startup: the OTel SDK's exporter constructors use
// grpc.NewClient / non-blocking HTTP dials, so an unreachable collector
// only surfaces via the OTel global error handler on the first failed
// export (see otel.SetErrorHandler in run.go).
func BuildTelemetry(flags CLIFlags) (*orch.Telemetry, TelemetryShutdown, error) {
	if flags.OTelEndpoint == "" {
		return nil, noopShutdown, nil
	}

	if flags.OTelInsecure && !isLoopbackEndpoint(flags.OTelEndpoint) {
		return nil, noopShutdown, fmt.Errorf(
			"refusing --otel-insecure against non-loopback endpoint %q — "+
				"insecure OTLP transmits branch names, task IDs, and error text in cleartext. "+
				"Use a loopback endpoint (localhost / 127.0.0.1 / ::1) for local dev, or drop --otel-insecure",
			flags.OTelEndpoint)
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

	// Install an OTel global error handler so asynchronous export failures
	// (unreachable collector, TLS handshake error, auth reject) surface
	// once to stderr instead of vanishing. The exporter constructors are
	// non-blocking, so a misconfigured endpoint only shows up here. We
	// install it lazily (exactly once per process, first endpoint wins)
	// to avoid clobbering a handler the test harness may have set.
	installOTelErrorHandlerOnce()

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

// otelErrorHandlerOnce ensures the global OTel error handler is installed
// at most once. Tests that run BuildTelemetry multiple times in a single
// process (the non-loopback guard test uses a table of cases) would
// otherwise reset the handler on each call.
var otelErrorHandlerOnce sync.Once

func installOTelErrorHandlerOnce() {
	otelErrorHandlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			// Async SDK failures land here. Rate-limiting isn't necessary
			// because OTel's BatchSpanProcessor / PeriodicReader log once
			// per batch, not per span/point. Tag with [OBS-04] so
			// operators can grep regardless of exporter diagnostics shape.
			fmt.Fprintf(os.Stderr, "[OBS-04] otel async error: %v\n", err)
		}))
	})
}

// isLoopbackEndpoint reports whether the host portion of an OTLP endpoint
// refers to the local machine. Accepts bare hosts ("localhost"), host:port
// ("localhost:14317"), IPv4 ("127.0.0.1:4317"), and IPv6 ("[::1]:4317").
// Used to gate the --otel-insecure flag: transmitting telemetry in
// cleartext is a local-dev convenience; it should not silently apply when
// an operator aims a dev flag at a production collector.
func isLoopbackEndpoint(endpoint string) bool {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

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
