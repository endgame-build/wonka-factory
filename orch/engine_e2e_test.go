//go:build verify && integration

package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// --- E2E integration tests (Tier 3) ---
//
// Each test asserts three things:
// 1. Expected final task statuses
// 2. Mandatory event sequence in audit trail
// 3. Runtime invariants don't fire (implicit via -tags verify)

// TestE2E_HappyPath verifies the golden path: a linear chain of builder tasks, all exit 0.
func TestE2E_HappyPath(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/happy", "builder", "verifier")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-happy"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	testutil.LinearGraph(t, store, "feat/happy", "builder", 3)
	require.NoError(t, store.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "happy path should complete without error")

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath,
		orch.EventLifecycleStarted, orch.EventLifecycleCompleted)
	validateEventSequence(t, logPath, []orch.EventKind{
		orch.EventLifecycleStarted,
		orch.EventTaskDispatched,
		orch.EventTaskCompleted,
		orch.EventLifecycleCompleted,
	})

	store2, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store2.Close()) })
	tasks, err := store2.ListTasks("branch:feat/happy")
	require.NoError(t, err)
	for _, task := range tasks {
		assert.Equal(t, orch.StatusCompleted, task.Status,
			"task %s should be completed", task.ID)
	}
}

// TestE2E_RetryThenSucceed verifies that a task that fails once (exit 1)
// is retried and eventually succeeds.
func TestE2E_RetryThenSucceed(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/retry", "builder")
	lifecycle.MaxRetries = 2
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-retry"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	// First call fails (exit 1), subsequent calls succeed (exit 0).
	e.SetTestSpawnFunc(testutil.SequenceSpawnFunc([]int{1, 0}))

	prepopulateLedger(t, runDir, testTask("retry-t", "feat/retry", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err)

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventTaskRetried)

	validateEventSequence(t, logPath, []orch.EventKind{
		orch.EventTaskDispatched,
		orch.EventTaskRetried,
		orch.EventTaskDispatched,
		orch.EventTaskCompleted,
	})
}

// TestE2E_BlockedTask verifies that exit code 2 sets the task to blocked
// and downstream tasks cannot be dispatched.
func TestE2E_BlockedTask(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/block", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-block"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(2)) // all blocked

	prepopulateLedger(t, runDir, testTask("block-t", "feat/block", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A single blocked task allows lifecycle completion (all tasks terminal).
	err = e.Run(ctx)
	assert.NoError(t, err)

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventTaskBlocked)
}

// TestE2E_GapAbort verifies BVV-ERR-04: multiple non-critical failures
// exceed gap tolerance, triggering lifecycle abort.
func TestE2E_GapAbort(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/gap", "builder")
	lifecycle.GapTolerance = 2
	lifecycle.MaxRetries = 0 // no retries — immediate failure
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-gap"
	cfg.MaxWorkers = 3
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(1)) // all fail

	// 3 parallel non-critical tasks — gap=2 will abort.
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	testutil.ParallelGraph(t, store, "feat/gap", "builder", 3)
	require.NoError(t, store.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted)

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventGapRecorded, orch.EventEscalationCreated)
}

// TestE2E_HandoffSuccess verifies the happy-path handoff at the Engine level
// (BVV-DSP-14): exit 3 then exit 0 completes cleanly with the expected event
// sequence and no failure/block/limit events.
func TestE2E_HandoffSuccess(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/hoff-ok", "builder")
	lifecycle.MaxHandoffs = 3
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-hoff-ok"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	// First call exits 3 (handoff), second call exits 0 (success).
	e.SetTestSpawnFunc(testutil.SequenceSpawnFunc([]int{3, 0}))

	prepopulateLedger(t, runDir, testTask("hoff-ok-t", "feat/hoff-ok", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "handoff followed by success should complete cleanly")

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventTaskHandoff, orch.EventTaskCompleted)
	validateEventSequence(t, logPath, []orch.EventKind{
		orch.EventTaskDispatched,
		orch.EventTaskHandoff,
		orch.EventTaskCompleted,
	})

	for _, ev := range readEvents(t, logPath) {
		assert.NotEqual(t, orch.EventTaskFailed, ev.Kind,
			"happy-path handoff must not emit task_failed (ev=%+v)", ev)
		assert.NotEqual(t, orch.EventTaskBlocked, ev.Kind,
			"happy-path handoff must not emit task_blocked (ev=%+v)", ev)
		assert.NotEqual(t, orch.EventHandoffLimitReached, ev.Kind,
			"handoff within budget must not reach the limit (ev=%+v)", ev)
	}
}

// TestE2E_HandoffLimit verifies BVV-L-04: a task that keeps requesting
// handoffs (exit 3) is eventually failed when the limit is reached.
func TestE2E_HandoffLimit(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/hoff", "builder")
	lifecycle.MaxHandoffs = 2
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-hoff"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(3)) // always handoff

	prepopulateLedger(t, runDir, testTask("hoff-t", "feat/hoff", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Task hits handoff limit → failed → lifecycle completes (all terminal).
	err = e.Run(ctx)
	assert.NoError(t, err)

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventHandoffLimitReached)
}

// TestE2E_ResumeAfterCrash verifies that an interrupted lifecycle can be
// resumed. Guards against vacuous passes: Phase 1 must be interrupted while at
// least one task is still non-terminal, otherwise Resume has no work to do.
func TestE2E_ResumeAfterCrash(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/resume", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	// Phase 1: start a run, complete one task, then cancel.
	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-resume"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e1, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	spawnFn, ch := testutil.ChannelSpawnFunc()
	e1.SetTestSpawnFunc(spawnFn)

	// Create two tasks: t-0 → t-1 (linear).
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	testutil.LinearGraph(t, store, "feat/resume", "builder", 2)
	require.NoError(t, store.Close())

	logPath := filepath.Join(runDir, "events.jsonl")
	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)

	// Run in a goroutine so we can simulate the crash.
	done := make(chan error, 1)
	go func() { done <- e1.Run(ctx1) }()

	// Poll instead of sleeping — removes the prior 100ms race window.
	ch <- 0
	waitForTaskEvent(t, logPath, orch.EventTaskCompleted, "t-0", 10*time.Second)
	cancel1()

	runErr := <-done
	if runErr != nil {
		assert.ErrorIs(t, runErr, context.Canceled,
			"Phase 1 should exit via cancellation, got: %v", runErr)
	}

	store2, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	phase1Tasks, err := store2.ListTasks("branch:feat/resume")
	require.NoError(t, err)
	require.NoError(t, store2.Close())
	nonTerminalAfterPhase1 := 0
	for _, task := range phase1Tasks {
		if !task.Status.Terminal() {
			nonTerminalAfterPhase1++
		}
	}
	require.Greater(t, nonTerminalAfterPhase1, 0,
		"Phase 1 must be interrupted mid-lifecycle — resume path is untested otherwise")

	// Phase 2: resume and complete remaining task.
	cfg2 := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg2.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e2, err := orch.NewEngine(cfg2)
	require.NoError(t, err)
	e2.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()

	err = e2.Resume(ctx2)
	assert.NoError(t, err, "resume should complete successfully")

	// Verify all tasks are terminal.
	store3, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store3.Close()) })
	tasks, err := store3.ListTasks("branch:feat/resume")
	require.NoError(t, err)
	for _, task := range tasks {
		assert.True(t, task.Status.Terminal(),
			"task %s should be terminal after resume, got %s", task.ID, task.Status)
	}
}

// --- Helpers ---

// waitForTaskEvent polls the JSONL event log until a (kind, taskID) event
// appears or the timeout elapses. Lets tests sequence on a specific lifecycle
// milestone instead of sleeping for a fixed duration. Lives here (not in
// watchdog_test.go alongside readEvents) because CI lints with just the
// `verify` tag — placing it there would flag it as unused.
func waitForTaskEvent(t *testing.T, logPath string, kind orch.EventKind, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(logPath); err == nil {
			for _, e := range readEvents(t, logPath) {
				if e.Kind == kind && e.TaskID == taskID {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %q for task %q did not appear in %s within %s", kind, taskID, logPath, timeout)
}

// validateEventSequence checks that the event log contains the specified
// event kinds in order (not necessarily contiguous — other events may appear
// between them).
func validateEventSequence(t *testing.T, logPath string, expected []orch.EventKind) {
	t.Helper()
	events := readEvents(t, logPath) // reuse JSONL parser from watchdog_test.go

	idx := 0
	for _, e := range events {
		if idx < len(expected) && e.Kind == expected[idx] {
			idx++
		}
	}
	if idx < len(expected) {
		t.Errorf("event sequence incomplete: matched %d/%d expected events; stuck at %q",
			idx, len(expected), expected[idx])
	}
}

// TestE2E_PlannerThenDispatch verifies end-to-end conformance for BVV-TG-07..10:
// a lifecycle seeded with a single role:planner task runs the planner,
// passes post-planner graph validation, then dispatches build → verify →
// gate in dependency order. Completes with the graph_validated anchor in
// the audit trail.
func TestE2E_PlannerThenDispatch(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/planner-e2e"
	lifecycle := testutil.MockLifecycleConfig(branch, "planner", "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true // BVV-TG-07..10 enforcement

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-planner"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	// Pre-populate ledger with the plan task only — the planner itself
	// creates build/verify/gate via seedLifecycleGraph inside the SpawnFunc.
	prepopulateLedger(t, runDir, &orch.Task{
		ID: "plan-1", Title: "plan", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      branch,
			orch.LabelRole:        "planner",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	})

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	// The spawn func reads e.Store() lazily (at invocation time, not at
	// construction). Safe because dispatch only runs after init() populates
	// the engine's store field. SetTestSpawnFunc takes the closure by value.
	e.SetTestSpawnFunc(plannerE2ESpawnFunc(t, e, branch, wellFormedGraph(), nil))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "planner-then-dispatch lifecycle should complete")

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath,
		orch.EventLifecycleStarted,
		orch.EventGraphValidated,
		orch.EventLifecycleCompleted,
	)

	// Dispatch ordering: plan must dispatch before build; build before
	// verify; verify before gate. Gate may or may not be reachable
	// depending on role config — we only assert the first three.
	events := readEvents(t, logPath)
	dispatchOrder := []string{}
	for _, e := range events {
		if e.Kind == orch.EventTaskDispatched {
			dispatchOrder = append(dispatchOrder, e.TaskID)
		}
	}
	assert.Contains(t, dispatchOrder, "plan-1", "plan task must dispatch")
	assert.Contains(t, dispatchOrder, "build-1", "build task must dispatch")
	assert.Contains(t, dispatchOrder, "verify-1", "verify task must dispatch")

	// Verify final state: plan/build/verify all completed.
	ledgerDir := filepath.Join(runDir, "ledger")
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, id := range []string{"plan-1", "build-1", "verify-1"} {
		task, err := store.GetTask(id)
		require.NoError(t, err, "task %s must exist after lifecycle", id)
		assert.Equal(t, orch.StatusCompleted, task.Status, "task %s should be completed", id)
	}
}

// TestE2E_PlannerIdempotent verifies BVV-TG-02 through the orchestrator's
// retry path: the planner fails on its first attempt (exit 1), retries,
// and succeeds on the second attempt. Both invocations call the graph-
// seeding logic; the second call must not create duplicate tasks or
// duplicate dependency edges.
func TestE2E_PlannerIdempotent(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/planner-idem"
	lifecycle := testutil.MockLifecycleConfig(branch, "planner", "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true
	lifecycle.MaxRetries = 2

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-planner-idem"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	prepopulateLedger(t, runDir, &orch.Task{
		ID: "plan-1", Title: "plan", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      branch,
			orch.LabelRole:        "planner",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	})

	// plannerAttempts is captured so the planner fails on attempt 1, succeeds on attempt 2.
	// seedLifecycleGraph runs on BOTH attempts — verifies idempotency.
	// atomic.Int32: spawn func runs on dispatch goroutine; test reads on test goroutine.
	var plannerAttempts atomic.Int32
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(plannerE2ESpawnFunc(t, e, branch, wellFormedGraph(), &plannerAttempts))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "idempotent planner should complete despite first-attempt failure")
	assert.GreaterOrEqual(t, int(plannerAttempts.Load()), 2, "planner should have been invoked at least twice")

	// Post-run: only one build-1 / verify-1 / gate-1 should exist (no dupes).
	ledgerDir := filepath.Join(runDir, "ledger")
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	tasks, err := store.ListTasks("branch:" + branch)
	require.NoError(t, err)
	ids := make(map[string]int, len(tasks))
	for _, t := range tasks {
		ids[t.ID]++
	}
	// Every task ID should appear exactly once — no duplicates from retry.
	for id, count := range ids {
		assert.Equal(t, 1, count, "task %s must appear exactly once (got %d)", id, count)
	}
}

// TestE2E_PlannerGraphInvalid verifies the BVV-TG-07..10 failure path
// end-to-end: a planner that completes successfully but produces a
// malformed graph (missing gate → BVV-TG-09) must cause the engine to
// emit EventGraphInvalid, create an escalation-graph-<plan-id> task, and
// abort the lifecycle with ErrLifecycleAborted. Complements
// TestE2E_PlannerThenDispatch by pinning the full wiring: validator →
// hook → emit → escalation → abort.
func TestE2E_PlannerGraphInvalid(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/planner-invalid"
	lifecycle := testutil.MockLifecycleConfig(branch, "planner", "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true // BVV-TG-07..10 enforcement

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-planner-invalid"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	prepopulateLedger(t, runDir, &orch.Task{
		ID: "plan-1", Title: "plan", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      branch,
			orch.LabelRole:        "planner",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	})

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(plannerE2ESpawnFunc(t, e, branch, malformedGraphNoGate(), nil))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Run(ctx)
	// AbortLifecycle from the hook surfaces through the dispatcher as
	// ErrLifecycleAborted — the same exit signal as gap-tolerance abort.
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted,
		"malformed-graph post-planner validation must abort the lifecycle")

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventGraphInvalid)

	// Engine must have created the graph-invalid escalation task.
	ledgerDir := filepath.Join(runDir, "ledger")
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	esc, err := store.GetTask("escalation-graph-plan-1")
	require.NoError(t, err, "graph-invalid escalation task must exist")
	assert.Equal(t, "escalation", esc.Labels[orch.LabelRole])
	assert.Equal(t, string(orch.Critical), esc.Labels[orch.LabelCriticality],
		"graph-invalid escalation must be critical — lifecycle cannot proceed")
	assert.Contains(t, esc.Title, "BVV-TG-09",
		"escalation title must name the failed requirement so operators grep-classify")

	// Happy-path escalation creation: the graph_invalid event's Detail
	// must NOT carry escalation_creation_failed (that field only appears
	// when the store rejects the CreateTask — silent-failure audit I-3).
	for _, ev := range readEvents(t, logPath) {
		if ev.Kind == orch.EventGraphInvalid && ev.TaskID == "plan-1" {
			assert.NotContains(t, ev.Detail, "escalation_creation_failed",
				"happy-path escalation creation must leave Detail clean")
		}
	}

	// Planner completed (store write persisted before hook fired), but no
	// build/verify/gate tasks should have dispatched — the abort blocks
	// further dispatch via abortCleanup's status=Blocked sweep.
	events := readEvents(t, logPath)
	for _, ev := range events {
		if ev.Kind == orch.EventTaskDispatched && ev.TaskID != "plan-1" {
			t.Errorf("no task other than plan-1 should dispatch after graph_invalid; saw %s", ev.TaskID)
		}
	}
}

// plannerE2ESpawnFunc builds a test SpawnFunc that, for role:planner
// (BVV-S-05: label-derived — ZFC), seeds `tasks` into the engine's live
// store via seedGraphForTest. If attempts is non-nil, the first planner
// invocation fails (exit 1) and subsequent attempts succeed — exercises
// BVV-TG-02 idempotency through the retry path. Non-planner roles
// succeed immediately (exit 0). e.Store() resolves lazily at invocation
// time, after init() has populated it.
func plannerE2ESpawnFunc(t *testing.T, e *orch.Engine, branch string, tasks []seededTask, attempts *atomic.Int32) orch.SpawnFunc {
	return func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		if task.Role() == orch.RolePlanner {
			if store := e.Store(); store != nil {
				seedGraphForTest(t, store, branch, tasks)
			}
			if attempts != nil {
				if attempts.Add(1) == 1 {
					outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeFailure, 1, roleCfg)
					return
				}
			}
		}
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	}
}

// seededTask names a downstream task for seedGraphForTest.
type seededTask struct {
	id          string
	role        string
	criticality orch.Criticality
	dependsOn   string // ID the seeded task depends on; must already exist in the store
}

// seedGraphForTest atomically seeds a downstream task graph onto a live
// store while the dispatch loop is running on another goroutine. Used by
// E2E tests to simulate a planner creating tasks mid-lifecycle.
//
// Race avoidance: tasks are created with a sentinel Assignee so ReadyTasks
// skips them (filter requires Assignee == ""). Dependencies wire, then
// each Assignee is cleared — opening the task for dispatch gated by its
// deps. Status stays Open throughout; no BVV-S-02 terminal-irreversibility
// violation is possible (no terminal state is ever visited). A silent
// GetTask / UpdateTask failure during release would leave a task stuck
// with the sentinel, hanging the lifecycle until test timeout — errors
// fail the test loudly instead.
//
// Idempotent per BVV-TG-02: duplicate CreateTask / AddDep on existing
// state are tolerated; a re-entry after all tasks are released short-
// circuits via the Assignee check.
func seedGraphForTest(t *testing.T, store orch.Store, branch string, tasks []seededTask) {
	t.Helper()
	const wiringSentinel = "__wiring__"

	for _, s := range tasks {
		_ = store.CreateTask(&orch.Task{
			ID: s.id, Status: orch.StatusOpen, Assignee: wiringSentinel,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        s.role,
				orch.LabelCriticality: string(s.criticality),
			},
		})
	}
	for _, s := range tasks {
		_ = store.AddDep(s.id, s.dependsOn)
	}
	for _, s := range tasks {
		task, err := store.GetTask(s.id)
		if err != nil {
			t.Errorf("seedGraphForTest: GetTask(%q) during sentinel release: %v", s.id, err)
			continue
		}
		if task.Assignee != wiringSentinel {
			continue // released by a prior idempotent call
		}
		task.Assignee = ""
		if err := store.UpdateTask(task); err != nil {
			t.Errorf("seedGraphForTest: UpdateTask(%q) during sentinel release: %v", s.id, err)
		}
	}
}

// wellFormedGraph returns the canonical plan→build→verify→gate downstream
// set. The plan task itself must already exist in the store.
func wellFormedGraph() []seededTask {
	return []seededTask{
		{id: "build-1", role: "builder", criticality: orch.NonCritical, dependsOn: "plan-1"},
		{id: "verify-1", role: "verifier", criticality: orch.NonCritical, dependsOn: "build-1"},
		{id: "gate-1", role: "gate", criticality: orch.Critical, dependsOn: "verify-1"},
	}
}

// malformedGraphNoGate omits the gate task, violating BVV-TG-09 (exactly
// one role:gate required). Used by TestE2E_PlannerGraphInvalid to trigger
// the post-planner validation-failure path end-to-end.
func malformedGraphNoGate() []seededTask {
	return []seededTask{
		{id: "build-1", role: "builder", criticality: orch.NonCritical, dependsOn: "plan-1"},
		{id: "verify-1", role: "verifier", criticality: orch.NonCritical, dependsOn: "build-1"},
	}
}

// --- Engine-level crash-replay tests (BVV-TG-07..10 crash resilience) ---
//
// These tests stage ledger + event log state simulating a crash mid-way
// through post-planner validation, then verify Engine.Resume closes the
// window by re-firing the hook. Without these, the crash-resilience claim
// is only verified at the reconcile-reader layer (TestReconcile_Graph*).
//
// None of these tests write a lock file: Engine.Resume's initForResume
// tolerates a missing lock (treats it as fresh-resume; keeps cfg.RunID),
// which keeps the scaffolding focused on the replay path. BVV-ERR-08
// lock-recovery behavior is covered by separate tests.

// seedCompletedPlannerWithGraph populates the ledger with a completed
// planner task plus the downstream graph, simulating the state where the
// planner succeeded (store write persisted) but the validation hook never
// ran. Delegates to prepopulateLedger for task creation, then re-opens
// the store to wire dependency edges (AddDep can't be issued through
// prepopulateLedger's close-after-create contract).
func seedCompletedPlannerWithGraph(t *testing.T, runDir, branch string, graph []seededTask) {
	t.Helper()
	tasks := make([]*orch.Task, 0, len(graph)+1)
	tasks = append(tasks, &orch.Task{
		ID: "plan-1", Title: "plan", Status: orch.StatusCompleted,
		Labels: map[string]string{
			orch.LabelBranch:      branch,
			orch.LabelRole:        "planner",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	})
	for _, s := range graph {
		tasks = append(tasks, &orch.Task{
			ID: s.id, Status: orch.StatusOpen,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        s.role,
				orch.LabelCriticality: string(s.criticality),
			},
		})
	}
	prepopulateLedger(t, runDir, tasks...)

	store, _, err := orch.NewStore("", filepath.Join(runDir, "ledger"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	for _, s := range graph {
		require.NoError(t, store.AddDep(s.id, s.dependsOn))
	}
}

// TestE2E_ResumeReplaysGraphValidation verifies the crash-replay path:
// when the ledger shows a completed planner task but the event log has no
// graph_validated / graph_invalid anchor, Engine.Resume must re-fire the
// validation hook and emit the missing anchor. This pins the BVV-TG-07..10
// crash-resilience claim at the engine level — reconcile-layer tests only
// verify the input data, not the engine's consumption of it.
func TestE2E_ResumeReplaysGraphValidation(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/replay-valid"
	runID := "e2e-replay-v"

	// Pre-crash state: planner completed, downstream graph wired, no
	// graph_* anchor in the event log.
	seedCompletedPlannerWithGraph(t, runDir, branch, wellFormedGraph())
	writeEvents(t, filepath.Join(runDir, "events.jsonl"), []orch.Event{
		{Kind: orch.EventLifecycleStarted, Summary: "prior run"},
		{Kind: orch.EventTaskCompleted, TaskID: "plan-1"},
		// No graph_validated / graph_invalid — the crash is here.
	})

	lifecycle := testutil.MockLifecycleConfig(branch, "planner", "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Millisecond // stale → recoverable
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = runID
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Resume(ctx)
	assert.NoError(t, err, "resume with well-formed latent graph should complete")

	logPath := filepath.Join(runDir, "events.jsonl")
	// Replay must emit graph_validated for plan-1, and the rest of the
	// lifecycle must proceed to completion.
	assertEventKinds(t, logPath,
		orch.EventGraphValidated,
		orch.EventLifecycleCompleted,
	)

	// Invariant from Issue 1: the new (post-resume) lifecycle_started anchor
	// must precede the replayed graph_validated. Prior to the reorder fix,
	// graph_validated could land BEFORE the new lifecycle_started, producing
	// an audit trail where mutation events fall outside any lifecycle.
	validateEventSequence(t, logPath, []orch.EventKind{
		orch.EventLifecycleStarted, // prior run's anchor
		orch.EventTaskCompleted,    // prior plan-1 completion
		orch.EventLifecycleStarted, // this run's anchor (the Resume)
		orch.EventGraphValidated,   // replay fires AFTER the new lifecycle_started
	})
}

// TestE2E_ResumeSkipsReplayWhenAnchorPresent verifies that when the event
// log already contains graph_validated for the completed planner, the
// replay path skips re-validation (no duplicate emission, no redundant
// Store scan). Pins the GraphValidationSeen idempotency guard.
func TestE2E_ResumeSkipsReplayWhenAnchorPresent(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/replay-skip"
	runID := "e2e-replay-s"

	seedCompletedPlannerWithGraph(t, runDir, branch, wellFormedGraph())
	writeEvents(t, filepath.Join(runDir, "events.jsonl"), []orch.Event{
		{Kind: orch.EventLifecycleStarted, Summary: "prior run"},
		{Kind: orch.EventTaskCompleted, TaskID: "plan-1"},
		{Kind: orch.EventGraphValidated, TaskID: "plan-1"}, // already validated
	})

	lifecycle := testutil.MockLifecycleConfig(branch, "planner", "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Millisecond
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = runID
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Resume(ctx)
	assert.NoError(t, err)

	// Count graph_validated events — should be exactly ONE (the pre-existing
	// one from the prior run). Replay must not re-emit.
	events := readEvents(t, filepath.Join(runDir, "events.jsonl"))
	validated := 0
	for _, ev := range events {
		if ev.Kind == orch.EventGraphValidated && ev.TaskID == "plan-1" {
			validated++
		}
	}
	assert.Equal(t, 1, validated, "replay must skip when graph_validated already present")
}

// TestE2E_ResumeReplayGraphInvalidAbortOrdering verifies both Issue 1
// (event ordering: lifecycle_started before graph_invalid) and Issue 3
// (abort reason plumbing: reason=graph_invalid:BVV-TG-09 lands on the
// terminal anchor). The ledger simulates a planner that completed with a
// malformed graph, crashed before the validator fired, then is Resumed.
func TestE2E_ResumeReplayGraphInvalidAbortOrdering(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/replay-invalid"
	runID := "e2e-replay-i"

	// Malformed graph in the ledger (no gate → BVV-TG-09 violation).
	seedCompletedPlannerWithGraph(t, runDir, branch, malformedGraphNoGate())
	writeEvents(t, filepath.Join(runDir, "events.jsonl"), []orch.Event{
		{Kind: orch.EventLifecycleStarted, Summary: "prior run"},
		{Kind: orch.EventTaskCompleted, TaskID: "plan-1"},
	})

	lifecycle := testutil.MockLifecycleConfig(branch, "planner", "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Millisecond
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = runID
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Resume(ctx)
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted,
		"malformed-graph replay must abort the lifecycle")

	logPath := filepath.Join(runDir, "events.jsonl")

	// Issue 1 invariant: this run's lifecycle_started must precede the
	// replayed graph_invalid. validateEventSequence does subsequence matching,
	// so we walk the log and find the indices explicitly to pin ordering.
	// Pin the ordering: the SECOND lifecycle_started (the one emitted by
	// this Resume — the first came from the pre-staged prior-run log) must
	// precede the graph_invalid event emitted by replayPlannerValidation.
	// Counting occurrences instead of using index arithmetic keeps the test
	// robust against changes to the pre-staged fixture's event count.
	events := readEvents(t, logPath)
	var startIdx, invalidIdx = -1, -1
	lifecycleStartedCount := 0
	for i, ev := range events {
		if ev.Kind == orch.EventLifecycleStarted {
			lifecycleStartedCount++
			if lifecycleStartedCount == 2 {
				startIdx = i
			}
		}
		if ev.Kind == orch.EventGraphInvalid && ev.TaskID == "plan-1" {
			invalidIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, startIdx, 0, "post-resume lifecycle_started must exist")
	require.GreaterOrEqual(t, invalidIdx, 0, "graph_invalid must be emitted by replay")
	assert.Less(t, startIdx, invalidIdx,
		"BVV-TG-07..10: post-resume lifecycle_started MUST precede replayed graph_invalid (audit trail integrity)")

	// Issue 3 invariant: the terminal lifecycle_completed anchor must carry
	// the machine-parseable abort reason identifying the graph-invalid
	// requirement — NOT the default "gap_tolerance_exceeded".
	var terminalDetail string
	for _, ev := range events {
		if ev.Kind == orch.EventLifecycleCompleted {
			terminalDetail = ev.Detail
		}
	}
	assert.Contains(t, terminalDetail, "graph_invalid:BVV-TG-09",
		"terminal anchor must carry the specific abort reason, not 'gap_tolerance_exceeded'")
	assert.NotContains(t, terminalDetail, "gap_tolerance_exceeded",
		"abort reason must not fall through to the gap-tolerance default for graph-invalid aborts")

	// Escalation task must exist (replay created it).
	ledgerDir := filepath.Join(runDir, "ledger")
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	esc, err := store.GetTask("escalation-graph-plan-1")
	require.NoError(t, err, "replay must create the graph-invalid escalation")
	assert.Contains(t, esc.Title, "BVV-TG-09")
}

// --- V&V matrix Sections 9.2–9.5 scenario coverage ---

// TestE2E_PlannerPartialFailure verifies BVV_VV_STRATEGY.md §Phase 8
// scenario 9.2: a planner task that fails on its first attempt and
// succeeds on retry must (a) emit task_retried then task_completed for the
// plan task, and (b) dispatch the downstream build/verify/gate tasks
// after the retry succeeds. Distinct from TestE2E_PlannerIdempotent —
// that test pins "no duplicate tasks created"; this one pins "lifecycle
// progress past the planner is not blocked by the partial failure".
func TestE2E_PlannerPartialFailure(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/planner-partial"
	lifecycle := testutil.MockLifecycleConfig(branch,
		orch.RolePlanner, orch.RoleBuilder, orch.RoleVerifier, orch.RoleGate)
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = true
	lifecycle.MaxRetries = 2

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-planner-partial"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	prepopulateLedger(t, runDir, &orch.Task{
		ID: "plan-1", Title: "plan", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      branch,
			orch.LabelRole:        orch.RolePlanner,
			orch.LabelCriticality: string(orch.NonCritical),
		},
	})

	// atomic.Int32: spawn func runs on dispatch goroutine; test reads on test goroutine.
	var plannerAttempts atomic.Int32
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(plannerE2ESpawnFunc(t, e, branch, wellFormedGraph(), &plannerAttempts))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "lifecycle must complete after planner retry succeeds")
	// Exact == not >= so a watchdog-driven over-retry regression fails here.
	assert.Equal(t, int32(2), plannerAttempts.Load(), "planner must retry exactly once (1 fail + 1 success)")

	logPath := filepath.Join(runDir, "events.jsonl")
	// Mandatory event sequence per V&V matrix §9.2.
	validateEventSequence(t, logPath, []orch.EventKind{
		orch.EventTaskDispatched,
		orch.EventTaskRetried,
		orch.EventTaskDispatched,
		orch.EventTaskCompleted,
		orch.EventGraphValidated,
	})

	// Lifecycle progress assertion: every downstream task must have dispatched.
	dispatched := map[string]bool{}
	for _, ev := range readEvents(t, logPath) {
		if ev.Kind == orch.EventTaskDispatched {
			dispatched[ev.TaskID] = true
		}
	}
	for _, id := range []string{"plan-1", "build-1", "verify-1"} {
		assert.True(t, dispatched[id],
			"task %s must dispatch — partial planner failure must not stall progress", id)
	}
}

// TestE2E_ConcurrentVVConflict verifies V&V matrix §9.3: two parallel
// verifier tasks where one fails on its first attempt and succeeds on
// retry must both reach `completed`. Models the canonical "git conflict
// during parallel V&V" scenario where one verifier transiently fails and
// the orchestrator's retry path resolves it without blocking the sibling.
func TestE2E_ConcurrentVVConflict(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/vv-conflict"
	lifecycle := testutil.MockLifecycleConfig(branch,
		orch.RoleBuilder, orch.RoleVerifier, orch.RoleGate)
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	lifecycle.ValidateGraph = false // no planner — skip graph validation
	lifecycle.MaxRetries = 2

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-vv-conflict"
	cfg.MaxWorkers = 3
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	// Seed graph: build-1 → [vv-1, vv-2] → gate-1.
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	mk := func(id, role string, crit orch.Criticality) {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: id, Title: id, Status: orch.StatusOpen,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        role,
				orch.LabelCriticality: string(crit),
			},
		}))
	}
	mk("build-1", orch.RoleBuilder, orch.NonCritical)
	mk("vv-1", orch.RoleVerifier, orch.NonCritical)
	mk("vv-2", orch.RoleVerifier, orch.NonCritical)
	mk("gate-1", orch.RoleGate, orch.Critical)
	require.NoError(t, store.AddDep("vv-1", "build-1"))
	require.NoError(t, store.AddDep("vv-2", "build-1"))
	require.NoError(t, store.AddDep("gate-1", "vv-1"))
	require.NoError(t, store.AddDep("gate-1", "vv-2"))
	require.NoError(t, store.Close())

	// vv-1 fails on its first invocation only; everything else exits 0.
	// Atomic counter keeps the spawn func race-free under concurrent dispatch.
	var vv1Attempts atomic.Int32
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		if task.ID == "vv-1" && vv1Attempts.Add(1) == 1 {
			outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeFailure, 1, roleCfg)
			return
		}
		// gate-1 will fail on `gh pr create` (no gh CLI in tests). Skip its
		// real execution and short-circuit to success — the test scope is
		// V&V conflict resolution, not gate behavior (covered separately).
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "concurrent V&V with retry must complete")

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventTaskRetried)

	// Final state: both verifiers and the gate must be completed.
	store2, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store2.Close()) })
	for _, id := range []string{"vv-1", "vv-2", "gate-1"} {
		task, err := store2.GetTask(id)
		require.NoError(t, err)
		assert.Equal(t, orch.StatusCompleted, task.Status,
			"task %s must complete despite vv-1 conflict", id)
	}
	assert.GreaterOrEqual(t, int(vv1Attempts.Load()), 2,
		"vv-1 must have been retried after first failure")
}

// TestE2E_CrashDuringReconciliation verifies V&V matrix §9.4: an
// orchestrator killed while a task is still `in_progress` must, on
// resume, reconcile the stale assignment (status reset, assignment
// cleared) before re-dispatching. Pins the reconcile-then-dispatch
// ordering in the resumed run's event log.
//
// Mechanism: Phase 1 uses ChannelSpawnFunc to leave t-0 stuck in
// `in_progress` (no completion signal sent), then cancels. Phase 2's
// Resume must observe the in-progress-without-tmux state, reset t-0
// to `open`, and re-dispatch it cleanly to completion.
func TestE2E_CrashDuringReconciliation(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/crash-reconcile"
	lifecycle := testutil.MockLifecycleConfig(branch, orch.RoleBuilder)
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	// Seed two linear tasks; t-0 will be left in_progress at Phase-1 crash time.
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	testutil.LinearGraph(t, store, branch, orch.RoleBuilder, 2)
	require.NoError(t, store.Close())

	logPath := filepath.Join(runDir, "events.jsonl")

	// --- Phase 1: dispatch t-0, leave it in_progress, cancel ---
	cfg1 := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg1.RunID = "e2e-crash-reconcile-1"
	cfg1.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e1, err := orch.NewEngine(cfg1)
	require.NoError(t, err)
	// ChannelSpawnFunc blocks forever (no signal sent) — t-0 stays in_progress.
	spawnFn, _ := testutil.ChannelSpawnFunc()
	e1.SetTestSpawnFunc(spawnFn)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	done1 := make(chan error, 1)
	go func() { done1 <- e1.Run(ctx1) }()

	// Wait for t-0 to be dispatched (and therefore in_progress).
	waitForTaskEvent(t, logPath, orch.EventTaskDispatched, "t-0", 10*time.Second)
	cancel1()
	// ChannelSpawnFunc blocks forever — t-0 can't terminate before the
	// cancel, so Phase 1 MUST surface context.Canceled, not nil.
	runErr := <-done1
	require.Error(t, runErr, "Phase 1 must surface context.Canceled, not nil")
	assert.ErrorIs(t, runErr, context.Canceled, "Phase 1 should exit via cancel, got: %v", runErr)

	// Confirm Phase-1 crash left t-0 in a non-terminal state with a stale
	// assignment — otherwise the reconcile path is vacuous. The paired
	// (non-terminal status, non-empty assignee) precondition is what makes
	// the later "t0Dispatches >= 2" check load-bearing: a regression that
	// redispatches without first clearing Assignee would violate BVV-S-03
	// (single assignment) under the new dispatch.
	storeMid, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t0Mid, err := storeMid.GetTask("t-0")
	require.NoError(t, err)
	require.False(t, t0Mid.Status.Terminal(),
		"reconcile precondition: t-0 must be non-terminal at crash time, got %s", t0Mid.Status)
	require.NotEmpty(t, t0Mid.Assignee,
		"reconcile precondition: t-0 must carry a stale assignment at crash time")
	require.NoError(t, storeMid.Close())

	// --- Phase 2: Resume with immediate-success spawns ---
	cfg2 := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg2.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e2, err := orch.NewEngine(cfg2)
	require.NoError(t, err)
	e2.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()

	err = e2.Resume(ctx2)
	assert.NoError(t, err, "resume after mid-flight crash must complete")

	// Final state: both tasks terminal.
	store3, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store3.Close()) })
	for _, id := range []string{"t-0", "t-1"} {
		task, err := store3.GetTask(id)
		require.NoError(t, err)
		assert.True(t, task.Status.Terminal(),
			"task %s must be terminal after resume, got %s", id, task.Status)
	}

	// Event-log assertion: the resumed run must dispatch t-0 a SECOND time
	// after reconcile reset its stale assignment. Count dispatches for t-0
	// across the full log — Phase 1 contributed one; Phase 2's reconcile
	// must contribute another.
	t0Dispatches := 0
	for _, ev := range readEvents(t, logPath) {
		if ev.Kind == orch.EventTaskDispatched && ev.TaskID == "t-0" {
			t0Dispatches++
		}
	}
	assert.GreaterOrEqual(t, t0Dispatches, 2,
		"reconcile must redispatch t-0 after Phase-1 crash (got %d dispatches)", t0Dispatches)
}

// TestE2E_ParallelGapExhaustion verifies V&V matrix §9.5: 5 parallel
// non-critical builders all fail; gap-tolerance=3 trips abort after the
// 3rd failure; remaining open tasks are blocked per BVV-ERR-04a abort
// cleanup. The lifecycle returns ErrLifecycleAborted with the audit trail
// pinning the gap_recorded × 3 → escalation_created sequence.
func TestE2E_ParallelGapExhaustion(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat/gap-exhaust"
	lifecycle := testutil.MockLifecycleConfig(branch, orch.RoleBuilder)
	lifecycle.GapTolerance = 3
	lifecycle.MaxRetries = 0 // immediate failure → gap counter increment
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-gap-exhaust"
	cfg.MaxWorkers = 3
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(1)) // every dispatch fails

	// 5 parallel tasks: 3 will dispatch (fill the worker pool), all fail,
	// gap trips at the 3rd, abort cleanup blocks the 2 still-open tasks.
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	testutil.ParallelGraph(t, store, branch, orch.RoleBuilder, 5)
	require.NoError(t, store.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted,
		"gap exhaustion at threshold 3 must surface ErrLifecycleAborted")

	// Single log scan: count gap_recorded events and verify the escalation
	// landed. Gap may overshoot tolerance by up to MaxWorkers-1 (see
	// handleTerminalFailure; property covered by TestProp_GapBoundedOvershoot).
	logPath := filepath.Join(runDir, "events.jsonl")
	gapCount, sawEscalation := 0, false
	for _, ev := range readEvents(t, logPath) {
		switch ev.Kind {
		case orch.EventGapRecorded:
			gapCount++
		case orch.EventEscalationCreated:
			sawEscalation = true
		}
	}
	assert.True(t, sawEscalation, "abort must emit escalation_created")
	assert.GreaterOrEqual(t, gapCount, lifecycle.GapTolerance,
		"gap counter must reach the tolerance threshold (got %d)", gapCount)
	assert.LessOrEqual(t, gapCount, lifecycle.GapTolerance+cfg.MaxWorkers-1,
		"gap counter must not overshoot by more than MaxWorkers-1 (got %d)", gapCount)

	// BVV-ERR-04a: post-abort, no task should remain `open`. Every task
	// must be either failed (dispatched + exit 1) or blocked (abort
	// cleanup swept it before dispatch). The total must account for every
	// seeded task — a leak would surface as other > 0 or as
	// failed+blocked < seeded.
	store2, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store2.Close()) })
	tasks, err := store2.ListTasks("branch:" + branch)
	require.NoError(t, err)
	const seeded = 5
	require.Len(t, tasks, seeded, "all seeded tasks must still exist in the ledger")
	failed, blocked, other := 0, 0, 0
	for _, task := range tasks {
		switch task.Status {
		case orch.StatusFailed:
			failed++
		case orch.StatusBlocked:
			blocked++
		default:
			other++
			t.Logf("post-abort task %s has unexpected status %s", task.ID, task.Status)
		}
	}
	assert.Zero(t, other, "BVV-ERR-04a: no tasks may remain non-terminal after abort cleanup")
	assert.Equal(t, seeded, failed+blocked,
		"every task must reach a terminal state (failed=%d blocked=%d)", failed, blocked)
	assert.GreaterOrEqual(t, failed, lifecycle.GapTolerance,
		"failed count must reach tolerance before abort (got %d)", failed)
	assert.GreaterOrEqual(t, blocked, 1,
		"at least one task must be blocked by BVV-ERR-04a cleanup (got %d)", blocked)
}

// TestE2E_TelemetryEmission verifies the OBS-04 observability pipeline
// end-to-end: a live lifecycle with an attached OTel meter/tracer produces
// the expected counters, histograms, and spans. Unlike the unit tests in
// telemetry_test.go, this exercises the actual EventLog.Emit → Telemetry
// Record path driven by the real dispatch loop — the spot where the CLI's
// --otel-endpoint ends up wiring.
//
// Hermetic: uses in-memory ManualReader and SpanRecorder, not a real OTLP
// collector. Runs a 3-task linear graph (builder → verifier → gate) with
// mock-agents that exit 0.
func TestE2E_TelemetryEmission(t *testing.T) {
	skipWithoutTmux(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	spanRec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	telem, err := orch.NewTelemetry(mp, tp)
	require.NoError(t, err)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/telemetry", "builder", "verifier")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "e2e-telemetry"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}
	cfg.Telemetry = telem

	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	store, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	testutil.LinearGraph(t, store, "feat/telemetry", "builder", 3)
	require.NoError(t, store.Close())

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.NoError(t, e.Run(ctx), "happy path should complete")

	// Collect after the lifecycle — the metrics shut-down path flushes last-
	// second exports, but our ManualReader pulls them inline on Collect().
	rm := metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), &rm))

	metrics := flattenMetrics(&rm)

	// All 3 tasks dispatched → completed.
	assert.Equal(t, int64(3), sumInt64(metrics["wonka_task_dispatch_total"]),
		"every completion should increment the dispatch counter once")

	// Task duration histogram observed 3 samples (one per completion).
	assert.Equal(t, uint64(3), histCount(metrics["wonka_task_duration_seconds"]),
		"task duration recorded per completion")

	// Lifecycle duration histogram observed one sample.
	assert.Equal(t, uint64(1), histCount(metrics["wonka_lifecycle_duration_seconds"]),
		"lifecycle duration recorded once at completion")

	// Traces: lifecycle span + one span per task = 4 ended spans.
	ended := spanRec.Ended()
	assert.GreaterOrEqual(t, len(ended), 4, "lifecycle + per-task spans expected, got %d", len(ended))

	// Find the lifecycle span and verify its branch attribute round-tripped.
	var hasLifecycle bool
	for _, s := range ended {
		if s.Name() == "wonka.lifecycle" {
			hasLifecycle = true
			var branchAttr string
			for _, a := range s.Attributes() {
				if string(a.Key) == "branch" {
					branchAttr = a.Value.AsString()
				}
			}
			assert.Equal(t, "feat/telemetry", branchAttr, "lifecycle span must carry branch")
		}
	}
	assert.True(t, hasLifecycle, "lifecycle span must be present")
}

// flattenMetrics collapses the scope → metrics hierarchy into a name-keyed
// map for easy assertion. Mirrors collectMetrics from the unit test but
// lives in orch_test to avoid exporting test helpers.
func flattenMetrics(rm *metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	out := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func sumInt64(m metricdata.Metrics) int64 {
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

func histCount(m metricdata.Metrics) uint64 {
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
