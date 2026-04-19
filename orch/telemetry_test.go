//go:build verify

package orch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTelemetry_NilSafe verifies every Telemetry method is safe to invoke on
// a nil receiver. Production code paths frequently hold a nil *Telemetry
// when no exporter is configured (the CLI --otel-endpoint flag default);
// a single missing nil check would turn every lifecycle run into a panic.
func TestTelemetry_NilSafe(t *testing.T) {
	var nilT *Telemetry
	ctx := context.Background()

	// Should not panic
	nilT.StartLifecycle(ctx, "main")
	nilT.EndLifecycle(ctx, "main", "completed")
	nilT.Record(ctx, Event{Kind: EventTaskCompleted, TaskID: "t1"}, "main")
}

// TestTelemetry_NoopProvidersDoNotPanic verifies that NoopTelemetry() produces
// a Telemetry that accepts every event kind without panicking. Catches drift
// where a new event kind is added to the enum but not handled by Record()'s
// switch — even unhandled kinds must be no-ops, not panics.
func TestTelemetry_NoopProvidersDoNotPanic(t *testing.T) {
	telem := NoopTelemetry()
	require.NotNil(t, telem)
	ctx := context.Background()

	telem.StartLifecycle(ctx, "main")
	for _, kind := range AllEventKinds {
		telem.Record(ctx, Event{Kind: kind, TaskID: "t-" + string(kind)}, "main")
	}
	telem.EndLifecycle(ctx, "main", "completed")

	// Second call must be a no-op — EndLifecycle is idempotent so retry
	// paths can invoke it without double-recording the histogram.
	telem.EndLifecycle(ctx, "main", "completed")
}

// TestTelemetry_RecordsCounters sets up a ManualReader, drives a realistic
// lifecycle through Record(), and asserts the expected counter deltas.
// Pins the metric-name contract that the Grafana dashboard panels depend on.
func TestTelemetry_RecordsCounters(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	spanRec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	telem, err := NewTelemetry(mp, tp)
	require.NoError(t, err)

	ctx := context.Background()
	telem.StartLifecycle(ctx, "branch-a")

	// Simulate one task's life: dispatch → complete.
	telem.Record(ctx, Event{Kind: EventWorkerSpawned, Worker: "w1"}, "branch-a")
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "t1", Role: "builder", Worker: "w1"}, "branch-a")
	telem.Record(ctx, Event{Kind: EventTaskCompleted, TaskID: "t1", Role: "builder", Worker: "w1"}, "branch-a")
	telem.Record(ctx, Event{Kind: EventWorkerReleased, Worker: "w1"}, "branch-a")

	// Retry path on a second task.
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "t2", Role: "verifier"}, "branch-a")
	telem.Record(ctx, Event{Kind: EventTaskRetried, TaskID: "t2", Role: "verifier"}, "branch-a")

	// Handoff limit hit — dedicated counter so handleTerminalFailure's
	// subsequent EventEscalationCreated doesn't double-count.
	telem.Record(ctx, Event{Kind: EventHandoffLimitReached, TaskID: "t-limit", Role: "verifier"}, "branch-a")

	// Graph validation on planner completion.
	telem.Record(ctx, Event{Kind: EventGraphValidated, TaskID: "plan-1"}, "branch-a")

	telem.EndLifecycle(ctx, "branch-a", "completed")

	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(ctx, &rm))

	metrics := collectMetrics(t, &rm)

	// Counters: one task completed, one retry, one watchdog event, one graph validated.
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_task_dispatch_total"),
		"one task_completed emission should increment dispatch counter once")
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_retry_total"),
		"one task_retried emission should increment retry counter once")
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_handoff_limit_total"))
	assert.Equal(t, int64(0), sumCounter(metrics, "wonka_escalations_total"),
		"handoff limit must go to handoff_limit_total, not escalations_total")
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_graph_validation_total"))

	// Histograms must have recorded at least one sample (the completed task).
	assert.GreaterOrEqual(t, histogramCount(metrics, "wonka_task_duration_seconds"), uint64(1))
	assert.GreaterOrEqual(t, histogramCount(metrics, "wonka_lifecycle_duration_seconds"), uint64(1))

	// Spans: one lifecycle + one task span that completed + one task span that retried.
	// (workers_active tracking and other gauges aren't asserted here because
	// UpDownCounters in OTel sum-over-window reporting; the noop-safe test
	// above already proves the emission path.)
	spans := spanRec.Ended()
	assert.GreaterOrEqual(t, len(spans), 3, "expected lifecycle + 2 task spans")
}

// TestTelemetry_RecordThroughEventLog verifies the EventLog side-channel
// actually reaches the telemetry. Catches regressions in the EventLog.Emit
// wiring (the one line that makes Phase 10 functional).
func TestTelemetry_RecordThroughEventLog(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	telem, err := NewTelemetry(mp, nil)
	require.NoError(t, err)

	dir := t.TempDir()
	log, err := NewEventLog(dir + "/events.jsonl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	log.WithTelemetry(telem, "branch-x")

	// Event.Role populated by emission sites (dispatch.go, watchdog.go) —
	// telemetry uses the structured field, never scraping Summary.
	require.NoError(t, log.Emit(Event{
		Kind:   EventTaskDispatched,
		TaskID: "t1",
		Role:   "builder",
	}))
	require.NoError(t, log.Emit(Event{
		Kind:   EventTaskCompleted,
		TaskID: "t1",
		Role:   "builder",
	}))

	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &rm))

	assert.Equal(t, int64(1), sumCounter(collectMetrics(t, &rm), "wonka_task_dispatch_total"),
		"EventLog.Emit must drive telemetry when WithTelemetry was called")
}

// --- helpers ---

// collectMetrics flattens the scope->metric hierarchy into a single slice
// keyed by metric name, which is easier to assert against than the nested
// OTel SDK shape.
func collectMetrics(t *testing.T, rm *metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	t.Helper()
	out := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// sumCounter returns the total increment across all attribute sets for a
// counter metric. Returns 0 when absent so assertions can run against
// metrics that happened to not fire in a given test scenario.
func sumCounter(metrics map[string]metricdata.Metrics, name string) int64 {
	m, ok := metrics[name]
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

// histogramCount returns the total observation count across all attribute
// sets for a histogram metric. Returns 0 when absent.
func histogramCount(metrics map[string]metricdata.Metrics, name string) uint64 {
	m, ok := metrics[name]
	if !ok {
		return 0
	}
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		return 0
	}
	var total uint64
	for _, dp := range h.DataPoints {
		total += dp.Count
	}
	return total
}
