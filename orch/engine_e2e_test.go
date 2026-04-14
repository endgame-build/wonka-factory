//go:build verify && integration

package orch_test

import (
	"bufio"
	"context"
	"encoding/json"
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

// TestE2E_HappyPath verifies the golden path: plan→build→vv→gate, all exit 0.
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
	defer store2.Close()
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
// resumed: state is reconciled and remaining tasks are dispatched.
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
	// First spawn completes, context cancels before second task dispatches.
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

	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)

	// Run in a goroutine so we can simulate the crash.
	done := make(chan error, 1)
	go func() { done <- e1.Run(ctx1) }()

	// Complete the first task.
	ch <- 0
	// Then cancel to simulate crash before second task completes.
	time.Sleep(100 * time.Millisecond)
	cancel1()

	// First run exits via context cancellation (simulated crash).
	<-done

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
	store2, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	defer store2.Close()
	tasks, err := store2.ListTasks("branch:feat/resume")
	require.NoError(t, err)
	for _, task := range tasks {
		assert.True(t, task.Status.Terminal(),
			"task %s should be terminal after resume, got %s", task.ID, task.Status)
	}
}

// --- Helpers ---

// validateEventSequence checks that the event log contains the specified
// event kinds in order (not necessarily contiguous — other events may appear
// between them).
func validateEventSequence(t *testing.T, logPath string, expected []orch.EventKind) {
	t.Helper()
	f, err := os.Open(logPath)
	require.NoError(t, err)
	defer f.Close()

	var events []orch.Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e orch.Event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			events = append(events, e)
		}
	}
	require.NoError(t, scanner.Err())

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
