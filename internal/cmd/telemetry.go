package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
// configured by the flags. Empty OTelEndpoint → (nil, noopShutdown, nil).
// Flag errors are eager; network reachability is lazy (OTel exporter
// constructors use grpc.NewClient / non-blocking HTTP dials), so an
// unreachable collector surfaces via the global error handler on first
// failed export.
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

	// Surface async OTel export failures to stderr (OBS-04).
	installOTelErrorHandlerOnce()

	// ForceFlush and Shutdown are independent across providers; run each
	// pair concurrently so shutdown doesn't serialize two round-trips to
	// an unreachable collector on Ctrl-C. All errors are joined — a TLS
	// failure on the metrics pipeline must not mask a DNS failure on
	// traces, else an operator "fixes" the wrong layer next run.
	shutdown := func(ctx context.Context) error {
		var mu sync.Mutex
		var errs []error
		record := func(err error) {
			if err == nil {
				return
			}
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); record(mp.ForceFlush(ctx)) }()
		go func() { defer wg.Done(); record(tp.ForceFlush(ctx)) }()
		wg.Wait()
		wg.Add(2)
		go func() { defer wg.Done(); record(mp.Shutdown(ctx)) }()
		go func() { defer wg.Done(); record(tp.Shutdown(ctx)) }()
		wg.Wait()
		return errors.Join(errs...)
	}
	return telem, shutdown, nil
}

func noopShutdown(context.Context) error { return nil }

// otelErrorHandlerOnce guards otel.SetErrorHandler so repeated
// BuildTelemetry calls (table tests) don't clobber a prior handler.
var otelErrorHandlerOnce sync.Once

func installOTelErrorHandlerOnce() {
	otelErrorHandlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			fmt.Fprintf(os.Stderr, "[OBS-04] otel async error: %v\n", err)
		}))
	})
}

// isLoopbackEndpoint reports whether the host portion of "host:port" (or a
// bare host) refers to the local machine. "localhost" is accepted without
// DNS resolution. Uses netip.ParseAddr so IPv6 zone IDs (e.g. "::1%lo0")
// and bare-bracket forms like "[::1]" classify correctly.
func isLoopbackEndpoint(endpoint string) bool {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	// Bare-bracket form like "[::1]" (no port): SplitHostPort rejects it,
	// so strip a single balanced bracket pair before parsing.
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback()
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
