package orch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/endgame/wonka-factory/orch"

// Telemetry bundles OTel instruments for orchestrator observability (OBS-04).
// A nil *Telemetry is valid and makes every method a no-op, matching the
// nil-safe convention used elsewhere (e.g. ProgressReporter in emitAndNotify).
type Telemetry struct {
	tracer trace.Tracer

	taskTerminal    metric.Int64Counter
	retries         metric.Int64Counter
	handoffs        metric.Int64Counter
	handoffLimit    metric.Int64Counter
	graphValidation metric.Int64Counter
	escalations     metric.Int64Counter
	gatesCreated    metric.Int64Counter
	gatesPassed     metric.Int64Counter
	gatesFailed     metric.Int64Counter

	taskDuration      metric.Float64Histogram
	lifecycleDuration metric.Float64Histogram

	workersActive   metric.Int64UpDownCounter
	tasksInProgress metric.Int64UpDownCounter
	gapCount        metric.Int64UpDownCounter
	lockHeld        metric.Int64UpDownCounter

	taskSpans sync.Map // taskID → *taskSpanRecord

	// lifecycleMu guards lifecycleSpan and lifecycleStarted. StartLifecycle
	// and EndLifecycle mutate them; Record reads lifecycleSpan in
	// onTaskDispatched to parent the task span. Current call sites
	// serialize access (dispatch goroutine writes, Drain reads post-Wait),
	// but the race detector flagged a narrow window during test-mode
	// shutdown where a trailing outcome goroutine could Emit after the
	// main goroutine entered EndLifecycle. Mutex-guarded access removes
	// the hazard at negligible cost.
	lifecycleMu      sync.Mutex
	lifecycleSpan    trace.Span
	lifecycleStarted time.Time
}

// NewTelemetry builds a Telemetry bound to the given providers. Either
// argument may be nil, in which case the OTel no-op provider is used.
func NewTelemetry(meterProvider metric.MeterProvider, tracerProvider trace.TracerProvider) (*Telemetry, error) {
	if meterProvider == nil {
		meterProvider = metricnoop.NewMeterProvider()
	}
	if tracerProvider == nil {
		tracerProvider = tracenoop.NewTracerProvider()
	}

	m := meterProvider.Meter(instrumentationName)
	t := &Telemetry{tracer: tracerProvider.Tracer(instrumentationName)}

	var err error
	if t.taskTerminal, err = m.Int64Counter("wonka_task_terminal_total",
		metric.WithDescription("Tasks that reached a terminal state, counted by outcome (completed/failed/blocked)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: task_terminal counter: %w", err)
	}
	if t.retries, err = m.Int64Counter("wonka_retry_total",
		metric.WithDescription("Exit-1 retries (BVV-ERR-01)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: retry counter: %w", err)
	}
	if t.handoffs, err = m.Int64Counter("wonka_handoff_total",
		metric.WithDescription("Exit-3 handoffs (BVV-DSP-14)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: handoff counter: %w", err)
	}
	if t.handoffLimit, err = m.Int64Counter("wonka_handoff_limit_total",
		metric.WithDescription("Tasks that exceeded the handoff budget (BVV-L-04)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: handoff_limit counter: %w", err)
	}
	if t.graphValidation, err = m.Int64Counter("wonka_graph_validation_total",
		metric.WithDescription("Post-planner graph validation results (BVV-TG-07..10)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: graph validation counter: %w", err)
	}
	if t.escalations, err = m.Int64Counter("wonka_escalations_total",
		metric.WithDescription("Escalation tasks created"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: escalation counter: %w", err)
	}
	if t.gatesCreated, err = m.Int64Counter("wonka_gates_created_total",
		metric.WithDescription("PR gate creations"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: gate_created counter: %w", err)
	}
	if t.gatesPassed, err = m.Int64Counter("wonka_gates_passed_total",
		metric.WithDescription("PR gate passes (BVV-GT-02)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: gate_passed counter: %w", err)
	}
	if t.gatesFailed, err = m.Int64Counter("wonka_gates_failed_total",
		metric.WithDescription("PR gate failures"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: gate_failed counter: %w", err)
	}
	if t.taskDuration, err = m.Float64Histogram("wonka_task_duration_seconds",
		metric.WithDescription("Session duration per task by role and outcome"),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: task duration histogram: %w", err)
	}
	if t.lifecycleDuration, err = m.Float64Histogram("wonka_lifecycle_duration_seconds",
		metric.WithDescription("End-to-end lifecycle duration by branch and outcome"),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: lifecycle duration histogram: %w", err)
	}
	if t.workersActive, err = m.Int64UpDownCounter("wonka_workers_active",
		metric.WithDescription("Active worker sessions (BVV-S-04)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: workers_active gauge: %w", err)
	}
	if t.tasksInProgress, err = m.Int64UpDownCounter("wonka_tasks_in_progress",
		metric.WithDescription("Tasks currently in_progress by role"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: tasks_in_progress gauge: %w", err)
	}
	if t.gapCount, err = m.Int64UpDownCounter("wonka_gap_count",
		metric.WithDescription("Current gap counter per branch (BVV-ERR-04)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: gap_count gauge: %w", err)
	}
	if t.lockHeld, err = m.Int64UpDownCounter("wonka_lock_held",
		metric.WithDescription("Lifecycle lock held (0/1) per branch (BVV-S-01)"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: lock_held gauge: %w", err)
	}
	return t, nil
}

// NoopTelemetry returns a Telemetry bound to OTel no-op providers. Prefer a
// nil *Telemetry when a code path can skip the call entirely; use this when
// the type requires a non-nil value.
func NoopTelemetry() *Telemetry {
	t, _ := NewTelemetry(nil, nil) // cannot fail with no-op providers
	return t
}

// StartLifecycle opens the lifecycle-scope span. Called once per engine run
// from emitLifecycleStarted. Safe on a nil receiver. Concurrent-safe.
func (t *Telemetry) StartLifecycle(ctx context.Context, branch string) context.Context {
	if t == nil {
		return ctx
	}
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	t.lifecycleStarted = time.Now()
	ctx, t.lifecycleSpan = t.tracer.Start(ctx, "wonka.lifecycle",
		trace.WithAttributes(attribute.String("branch", branch)),
	)
	return ctx
}

// EndLifecycle closes the lifecycle span and records duration. Idempotent —
// nils lifecycleSpan after End so a second call is a no-op. Safe on a nil
// receiver or if StartLifecycle was never called. Concurrent-safe.
func (t *Telemetry) EndLifecycle(ctx context.Context, branch, outcome string) {
	if t == nil {
		return
	}
	t.lifecycleMu.Lock()
	span := t.lifecycleSpan
	started := t.lifecycleStarted
	t.lifecycleSpan = nil
	t.lifecycleMu.Unlock()
	if span == nil {
		return
	}
	duration := time.Since(started).Seconds()
	t.lifecycleDuration.Record(ctx, duration,
		metric.WithAttributes(
			attribute.String("branch", branch),
			attribute.String("outcome", outcome),
		),
	)
	span.SetAttributes(attribute.String("outcome", outcome))
	span.End()
}

// Record dispatches an event to the right instruments. Attributes stay low-
// cardinality (branch, role, outcome) because high-cardinality labels like
// task_id on counters blow up Prometheus storage — per-task detail lives in
// spans. Safe on a nil receiver.
func (t *Telemetry) Record(ctx context.Context, ev Event, branch string) {
	if t == nil {
		return
	}
	branchAttr := attribute.String("branch", branch)

	switch ev.Kind {
	case EventTaskDispatched:
		t.onTaskDispatched(ctx, ev, branchAttr)

	case EventTaskCompleted:
		t.onTaskTerminal(ctx, ev, "completed")
		t.taskTerminal.Add(ctx, 1, metric.WithAttributes(
			branchAttr, attribute.String("outcome", "completed"),
		))
		t.tasksInProgress.Add(ctx, -1, metric.WithAttributes(branchAttr))

	case EventTaskFailed:
		t.onTaskTerminal(ctx, ev, "failed")
		t.taskTerminal.Add(ctx, 1, metric.WithAttributes(
			branchAttr, attribute.String("outcome", "failed"),
		))
		t.tasksInProgress.Add(ctx, -1, metric.WithAttributes(branchAttr))

	case EventTaskBlocked:
		t.onTaskTerminal(ctx, ev, "blocked")
		t.taskTerminal.Add(ctx, 1, metric.WithAttributes(
			branchAttr, attribute.String("outcome", "blocked"),
		))
		t.tasksInProgress.Add(ctx, -1, metric.WithAttributes(branchAttr))

	case EventTaskRetried:
		t.onTaskTerminal(ctx, ev, "retried")
		t.retries.Add(ctx, 1, metric.WithAttributes(branchAttr))
		t.tasksInProgress.Add(ctx, -1, metric.WithAttributes(branchAttr))

	case EventTaskHandoff:
		// BVV-DSP-14: the task remains in_progress across a handoff;
		// only the tmux session restarts. We keep the dispatch span
		// open, annotate it with a handoff marker, and leave
		// tasksInProgress alone. Decrementing here (and ending the
		// span) would drift the gauge permanently low per handoff and
		// drop the post-handoff segment from wonka_task_duration_seconds.
		if raw, ok := t.taskSpans.Load(ev.TaskID); ok {
			rec := raw.(*taskSpanRecord)
			rec.span.AddEvent("handoff", trace.WithAttributes(
				attribute.String("worker", ev.Worker),
			))
		}
		t.handoffs.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventWorkerSpawned:
		t.workersActive.Add(ctx, 1)

	case EventWorkerReleased:
		t.workersActive.Add(ctx, -1)

	case EventGapRecorded:
		t.gapCount.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventEscalationCreated:
		reason := ev.Detail
		if reason == "" {
			reason = "unknown"
		}
		t.escalations.Add(ctx, 1, metric.WithAttributes(
			branchAttr, attribute.String("reason", reason),
		))

	case EventEscalationResolved:
		t.gapCount.Add(ctx, -1, metric.WithAttributes(branchAttr))

	case EventLifecycleStarted:
		t.lockHeld.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventLifecycleCompleted:
		t.lockHeld.Add(ctx, -1, metric.WithAttributes(branchAttr))

	case EventGateCreated:
		t.gatesCreated.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventGatePassed:
		t.gatesPassed.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventGateFailed:
		t.gatesFailed.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventHandoffLimitReached:
		// Dedicated counter: handleTerminalFailure may also emit
		// EventEscalationCreated for the same task (critical bit / gap
		// overshoot), so routing to wonka_escalations_total here would
		// double-count the handoff-limit case alone.
		t.handoffLimit.Add(ctx, 1, metric.WithAttributes(branchAttr))

	case EventGraphValidated:
		t.graphValidation.Add(ctx, 1, metric.WithAttributes(
			branchAttr, attribute.String("result", "valid"),
		))

	case EventGraphInvalid:
		t.graphValidation.Add(ctx, 1, metric.WithAttributes(
			branchAttr, attribute.String("result", "invalid"),
		))
	}
}

func (t *Telemetry) onTaskDispatched(ctx context.Context, ev Event, branchAttr attribute.KeyValue) {
	var parent context.Context = ctx
	t.lifecycleMu.Lock()
	lifecycleSpan := t.lifecycleSpan
	t.lifecycleMu.Unlock()
	if lifecycleSpan != nil {
		parent = trace.ContextWithSpan(ctx, lifecycleSpan)
	}
	_, span := t.tracer.Start(parent, "wonka.task",
		trace.WithAttributes(
			attribute.String("task.id", ev.TaskID),
			attribute.String("role", ev.Role),
			branchAttr,
			attribute.String("worker", ev.Worker),
		),
	)
	newRec := &taskSpanRecord{
		span:    span,
		started: time.Now(),
		role:    ev.Role,
	}
	// LoadOrStore protects against duplicate dispatch events (crash
	// recovery / resume replay can re-emit task_dispatched for a task
	// whose first span is still in taskSpans). A plain Store would
	// orphan the prior span — never End()-ed, never exported. Instead
	// we end the prior span with outcome="superseded" and take over.
	if prior, loaded := t.taskSpans.LoadOrStore(ev.TaskID, newRec); loaded {
		priorRec := prior.(*taskSpanRecord)
		priorRec.span.SetAttributes(attribute.String("outcome", "superseded"))
		priorRec.span.End()
		t.taskSpans.Store(ev.TaskID, newRec)
	}
	t.tasksInProgress.Add(ctx, 1, metric.WithAttributes(branchAttr))
}

func (t *Telemetry) onTaskTerminal(ctx context.Context, ev Event, outcome string) {
	raw, ok := t.taskSpans.LoadAndDelete(ev.TaskID)
	if !ok {
		return
	}
	rec := raw.(*taskSpanRecord)
	// Use the role captured at dispatch time. Watchdog-initiated events
	// (EventTaskHandoff) carry Role too, but the dispatch record is the
	// single source of truth for the attempt.
	role := rec.role
	if role == "" {
		role = "unknown"
	}
	duration := time.Since(rec.started).Seconds()

	rec.span.SetAttributes(
		attribute.String("outcome", outcome),
		attribute.Float64("duration_seconds", duration),
	)
	rec.span.End()

	t.taskDuration.Record(ctx, duration,
		metric.WithAttributes(
			attribute.String("role", role),
			attribute.String("outcome", outcome),
		),
	)
}

type taskSpanRecord struct {
	span    trace.Span
	started time.Time
	role    string
}
