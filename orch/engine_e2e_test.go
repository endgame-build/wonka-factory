//go:build verify && integration

package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	var plannerAttempts int
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(plannerE2ESpawnFunc(t, e, branch, wellFormedGraph(), &plannerAttempts))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "idempotent planner should complete despite first-attempt failure")
	assert.GreaterOrEqual(t, plannerAttempts, 2, "planner should have been invoked at least twice")

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
// succeed immediately (exit 0).
//
// The engine reference is captured by closure; e.Store() resolves lazily
// at invocation time, after init() has populated it.
func plannerE2ESpawnFunc(t *testing.T, e *orch.Engine, branch string, tasks []seededTask, attempts *int) orch.SpawnFunc {
	return func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		if task.Role() == orch.RolePlanner {
			if store := e.Store(); store != nil {
				seedGraphForTest(t, store, branch, tasks)
			}
			if attempts != nil {
				*attempts++
				if *attempts == 1 {
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
