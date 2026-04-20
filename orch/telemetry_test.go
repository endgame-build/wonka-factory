//go:build verify

package orch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestOBS04_NilSafe verifies every Telemetry method is safe to invoke on
// a nil receiver. Production code paths frequently hold a nil *Telemetry
// when no exporter is configured (the CLI --otel-endpoint flag default);
// a single missing nil check would turn every lifecycle run into a panic.
func TestOBS04_NilSafe(t *testing.T) {
	var nilT *Telemetry
	ctx := context.Background()

	// Should not panic
	nilT.StartLifecycle(ctx, "main")
	nilT.EndLifecycle(ctx, "main", "completed")
	nilT.Record(ctx, Event{Kind: EventTaskCompleted, TaskID: "t1"}, "main")
}

// TestOBS04_NoopProvidersDoNotPanic verifies that NoopTelemetry() produces
// a Telemetry that accepts every event kind without panicking. Catches drift
// where a new event kind is added to the enum but not handled by Record()'s
// switch — even unhandled kinds must be no-ops, not panics.
func TestOBS04_NoopProvidersDoNotPanic(t *testing.T) {
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

// TestOBS04_RecordsCounters sets up a ManualReader, drives a realistic
// lifecycle through Record(), and asserts the expected counter deltas.
// Pins the metric-name contract that the Grafana dashboard panels depend on.
func TestOBS04_RecordsCounters(t *testing.T) {
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
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_task_terminal_total"),
		"one task_completed emission should increment terminal counter once")
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_retry_total"),
		"one task_retried emission should increment retry counter once")
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_handoff_limit_total"))
	assert.Equal(t, int64(0), sumCounter(metrics, "wonka_escalations_total"),
		"handoff limit must go to handoff_limit_total, not escalations_total")
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_graph_validation_total"))

	// Histograms — exact counts: task_duration has 2 samples (t1 completed
	// + t2 retried; retry is a terminal-attempt marker and records a sample
	// the same way completion does), lifecycle_duration has 1 (EndLifecycle).
	// Using Equal (not GreaterOrEqual) so a future regression that
	// double-records either histogram fails this test.
	assert.Equal(t, uint64(2), histogramCount(metrics, "wonka_task_duration_seconds"),
		"one completion + one retry = 2 duration samples")
	assert.Equal(t, uint64(1), histogramCount(metrics, "wonka_lifecycle_duration_seconds"),
		"one EndLifecycle = one lifecycle duration sample")

	// Spans — exact count: 1 lifecycle + 2 task spans (t1 + t2).
	// Equal not GreaterOrEqual so a regression that leaks an extra span
	// (e.g. double-starting on duplicate dispatch) fails this test.
	spans := spanRec.Ended()
	assert.Equal(t, 3, len(spans), "expected exactly lifecycle + 2 task spans")
}

// TestOBS04_RecordsAllSwitchArms walks every arm of Record()'s switch and
// asserts the expected metric deltas. The existing TestOBS04_RecordsCounters
// covers the happy path; this one plugs the coverage gaps the code review
// flagged (EventTaskFailed/Blocked, EventTaskHandoff, gauge-pair symmetry,
// gate events, escalation reason fallback, EventGraphInvalid).
func TestOBS04_RecordsAllSwitchArms(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	spanRec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	telem, err := NewTelemetry(mp, tp)
	require.NoError(t, err)
	ctx := context.Background()
	branch := "branch-cov"

	// Build a scenario that hits every switch arm at least once.
	// Start lifecycle → lockHeld=+1.
	telem.StartLifecycle(ctx, branch)

	// Task A: dispatch → fail (outcome=failed).
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "A", Role: "builder"}, branch)
	telem.Record(ctx, Event{Kind: EventTaskFailed, TaskID: "A", Role: "builder"}, branch)

	// Task B: dispatch → blocked (outcome=blocked).
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "B", Role: "builder"}, branch)
	telem.Record(ctx, Event{Kind: EventTaskBlocked, TaskID: "B", Role: "builder"}, branch)

	// Task C: dispatch → handoff → complete. BVV-DSP-14: handoff is
	// non-terminal; the dispatch span stays open; tasksInProgress is NOT
	// decremented by handoff.
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "C", Role: "builder"}, branch)
	telem.Record(ctx, Event{Kind: EventTaskHandoff, TaskID: "C", Role: "builder", Worker: "w1"}, branch)
	telem.Record(ctx, Event{Kind: EventTaskCompleted, TaskID: "C", Role: "builder"}, branch)

	// Gap pair: gap_recorded then escalation_resolved — gauge symmetric.
	telem.Record(ctx, Event{Kind: EventGapRecorded, TaskID: "D"}, branch)
	telem.Record(ctx, Event{Kind: EventEscalationResolved, TaskID: "D"}, branch)

	// Gate flow: created → passed, then another created → failed.
	telem.Record(ctx, Event{Kind: EventGateCreated, TaskID: "g1"}, branch)
	telem.Record(ctx, Event{Kind: EventGatePassed, TaskID: "g1"}, branch)
	telem.Record(ctx, Event{Kind: EventGateCreated, TaskID: "g2"}, branch)
	telem.Record(ctx, Event{Kind: EventGateFailed, TaskID: "g2"}, branch)

	// Escalation with no Detail → reason should fall back to "unknown".
	telem.Record(ctx, Event{Kind: EventEscalationCreated, TaskID: "e1"}, branch)
	// Escalation with Detail → reason attribute carries it.
	telem.Record(ctx, Event{Kind: EventEscalationCreated, TaskID: "e2", Detail: "critical_task_failure"}, branch)

	// Graph invalid branch (sibling of graph_validated).
	telem.Record(ctx, Event{Kind: EventGraphInvalid, TaskID: "plan-bad"}, branch)

	// End lifecycle → lockHeld=-1.
	telem.EndLifecycle(ctx, branch, "completed")

	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(ctx, &rm))
	metrics := collectMetrics(t, &rm)

	// Terminal outcomes: 1 failed + 1 blocked + 1 completed = 3.
	assert.Equal(t, int64(3), sumCounter(metrics, "wonka_task_terminal_total"),
		"task_terminal must count each terminal outcome once regardless of outcome label")

	// Outcome label breakdown — catches label-swap regressions that a
	// total-only assertion would hide.
	assert.Equal(t, int64(1), counterWithAttr(metrics, "wonka_task_terminal_total", "outcome", "completed"))
	assert.Equal(t, int64(1), counterWithAttr(metrics, "wonka_task_terminal_total", "outcome", "failed"))
	assert.Equal(t, int64(1), counterWithAttr(metrics, "wonka_task_terminal_total", "outcome", "blocked"))

	// Handoff counter fires on EventTaskHandoff (BVV-DSP-14).
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_handoff_total"))

	// Gauge symmetry — balanced inc/dec pairs must sum to zero.
	// StartLifecycle doesn't drive lockHeld; drive it via Record to
	// exercise the lockHeld inc/dec pair.
	telem.Record(ctx, Event{Kind: EventLifecycleStarted}, branch)
	telem.Record(ctx, Event{Kind: EventLifecycleCompleted}, branch)
	require.NoError(t, reader.Collect(ctx, &rm))
	metrics = collectMetrics(t, &rm)

	assert.Equal(t, int64(0), gaugeSum(metrics, "wonka_tasks_in_progress"),
		"handoff is non-terminal per BVV-DSP-14 — dispatches and terminals must balance")
	assert.Equal(t, int64(0), gaugeSum(metrics, "wonka_gap_count"),
		"gap_recorded and escalation_resolved must balance")
	assert.Equal(t, int64(0), gaugeSum(metrics, "wonka_lock_held"),
		"lifecycle_started and lifecycle_completed must balance")

	// Gate counters — one each.
	assert.Equal(t, int64(2), sumCounter(metrics, "wonka_gates_created_total"))
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_gates_passed_total"))
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_gates_failed_total"))

	// Escalation counter: 2 emissions, one with Detail="" → reason="unknown".
	assert.Equal(t, int64(2), sumCounter(metrics, "wonka_escalations_total"))
	assert.Equal(t, int64(1), counterWithAttr(metrics, "wonka_escalations_total", "reason", "unknown"),
		"empty Detail must fall back to reason=unknown")
	assert.Equal(t, int64(1), counterWithAttr(metrics, "wonka_escalations_total", "reason", "critical_task_failure"))

	// Graph validation by result.
	assert.Equal(t, int64(1), counterWithAttr(metrics, "wonka_graph_validation_total", "result", "invalid"),
		"EventGraphInvalid must route to graph_validation with result=invalid")

	// Spans — 3 task dispatches + 1 lifecycle + handoff-annotated task C
	// ended exactly once. Equal asserts the handoff bug (previously
	// LoadAndDelete'd the span, producing double-end panics) stays fixed.
	ended := spanRec.Ended()
	assert.Equal(t, 4, len(ended),
		"expected 3 task spans (A, B, C) + 1 lifecycle span — handoff must not end the task span early")
}

// TestOBS04_DuplicateDispatchSupersedes verifies the LoadOrStore fix at
// onTaskDispatched: a second EventTaskDispatched for the same task ID
// ends the first span with outcome=superseded and installs a new span.
// Regression shape: plain Store would silently orphan the first span
// (never End()-ed, leaks via the batch exporter).
func TestOBS04_DuplicateDispatchSupersedes(t *testing.T) {
	spanRec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	telem, err := NewTelemetry(nil, tp)
	require.NoError(t, err)
	ctx := context.Background()

	telem.StartLifecycle(ctx, "branch-dup")
	// First dispatch for T.
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "T", Role: "builder"}, "branch-dup")
	// Duplicate dispatch (replay / resume scenario) — must end the first
	// span rather than orphan it.
	telem.Record(ctx, Event{Kind: EventTaskDispatched, TaskID: "T", Role: "builder"}, "branch-dup")
	// Terminal — should end the second span.
	telem.Record(ctx, Event{Kind: EventTaskCompleted, TaskID: "T", Role: "builder"}, "branch-dup")
	telem.EndLifecycle(ctx, "branch-dup", "completed")

	ended := spanRec.Ended()
	// 1 lifecycle + 2 task spans = 3. Both task spans must have End()-ed.
	assert.Equal(t, 3, len(ended), "superseded + final + lifecycle = 3 ended spans")

	// Find the superseded task span — it must carry outcome=superseded.
	var supersededFound, completedFound bool
	for _, s := range ended {
		if s.Name() != "wonka.task" {
			continue
		}
		for _, attr := range s.Attributes() {
			if attr.Key == "outcome" {
				switch attr.Value.AsString() {
				case "superseded":
					supersededFound = true
				case "completed":
					completedFound = true
				}
			}
		}
	}
	assert.True(t, supersededFound, "first dispatch's span must end with outcome=superseded")
	assert.True(t, completedFound, "second dispatch's span must end with outcome=completed")
}

// TestOBS04_EndLifecycleIdempotent verifies EndLifecycle records the
// histogram exactly once even when called twice. The nils-lifecycleSpan
// guard ensures a retry path that double-calls doesn't double-record.
// Catches regressions where the guard is removed.
func TestOBS04_EndLifecycleIdempotent(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	telem, err := NewTelemetry(mp, nil)
	require.NoError(t, err)
	ctx := context.Background()

	telem.StartLifecycle(ctx, "branch-idem")
	telem.EndLifecycle(ctx, "branch-idem", "completed")
	telem.EndLifecycle(ctx, "branch-idem", "completed") // second call

	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(ctx, &rm))
	metrics := collectMetrics(t, &rm)

	assert.Equal(t, uint64(1), histogramCount(metrics, "wonka_lifecycle_duration_seconds"),
		"EndLifecycle must be idempotent — only the first call records a sample")
}

// TestOBS04_UnknownTaskTerminalIsNoOp verifies onTaskTerminal returns
// silently when LoadAndDelete misses. This is the resume/replay path: a
// terminal event can arrive for a task whose dispatch span was opened in
// a prior process. A regression (panic, dereference-before-ok) would
// crash the orchestrator on the first reconciled terminal event.
func TestOBS04_UnknownTaskTerminalIsNoOp(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	telem, err := NewTelemetry(mp, nil)
	require.NoError(t, err)
	ctx := context.Background()

	// No StartLifecycle, no Dispatched — go straight to Completed.
	assert.NotPanics(t, func() {
		telem.Record(ctx, Event{Kind: EventTaskCompleted, TaskID: "orphan", Role: "builder"}, "branch-z")
	})

	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(ctx, &rm))
	metrics := collectMetrics(t, &rm)

	// Terminal counter still increments (outcome was recorded).
	assert.Equal(t, int64(1), sumCounter(metrics, "wonka_task_terminal_total"))
	// But no duration sample — the dispatch span record was absent.
	assert.Equal(t, uint64(0), histogramCount(metrics, "wonka_task_duration_seconds"),
		"terminal without prior dispatch must not record a duration sample")
}

// TestOBS04_RecordThroughEventLog verifies the EventLog side-channel
// actually reaches the telemetry. Catches regressions in the EventLog.Emit
// wiring (the one line that makes Phase 10 functional).
func TestOBS04_RecordThroughEventLog(t *testing.T) {
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

	assert.Equal(t, int64(1), sumCounter(collectMetrics(t, &rm), "wonka_task_terminal_total"),
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

// counterWithAttr returns the sum of data-point values on a counter
// metric restricted to points whose attribute set contains key=value.
// Used to assert per-label totals without iterating OTel's nested shape
// in every test.
func counterWithAttr(metrics map[string]metricdata.Metrics, name, key, value string) int64 {
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
		if v, present := dp.Attributes.Value(attribute.Key(key)); present && v.AsString() == value {
			total += dp.Value
		}
	}
	return total
}

// gaugeSum returns the net value of an UpDownCounter across all attribute
// sets. For symmetric inc/dec pairs (gap_recorded + escalation_resolved,
// lifecycle_started + lifecycle_completed), the expected value is zero —
// any non-zero reading flags a drift bug.
func gaugeSum(metrics map[string]metricdata.Metrics, name string) int64 {
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
