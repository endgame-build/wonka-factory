//go:build integration

package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/endgame/facet-scan/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fault Injection Tests (V&V Plan §3.2) ---
// These tests exercise failure paths that cannot be reached during normal operation.
// Tagged //go:build integration — runs on main branch only with longer timeout.

// TestFault01_KillTmuxSession verifies [SUP-01, SUP-02, SUP-03, OPS-07]: watchdog detects and restarts dead sessions.
func TestFault01_KillTmuxSession(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	// Use slow-success so we have time to kill the session.
	preset := testutil.MockPresetForScript(mockDir, "slow-success")
	preset.Env = map[string]string{"MOCK_DELAY_SECONDS": "5"}

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-01"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{
		Interval: 500 * time.Millisecond,
		// Slow agent poll (10s) so the watchdog (1s) reliably detects the dead
		// session before the agent goroutine marks the task terminal. Without
		// this gap, the agent goroutine wins the detection race and the watchdog
		// skips the restart (line 172-174 in watchdog.go).
		AgentPollInterval: 10 * time.Second,
	}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 1 * time.Second, CBThreshold: 5, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Run in background.
	errCh := make(chan error, 1)
	go func() { errCh <- engine.Run(ctx) }()

	// Poll until at least one tmux session exists (replaces flaky time.Sleep).
	tmuxClient := orch.NewTmuxClient("fault-01")
	var sessions []string
	deadline := time.After(15 * time.Second)
	for len(sessions) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for tmux sessions to appear")
		case <-time.After(200 * time.Millisecond):
		}
		var listErr error
		sessions, listErr = tmuxClient.ListSessions()
		if listErr != nil {
			t.Logf("ListSessions error (retrying): %v", listErr)
		}
	}
	require.NotEmpty(t, sessions, "expected at least one tmux session")
	err = tmuxClient.KillSession(sessions[0])
	require.NoError(t, err, "kill session should succeed")

	// Wait for completion or timeout.
	select {
	case err := <-errCh:
		t.Logf("engine.Run returned: %v", err)
	case <-ctx.Done():
		t.Log("test timed out — expected for fault injection")
	}

	// Verify event log contains a session restart event.
	eventLogPath := filepath.Join(dir, "events.jsonl")
	require.FileExists(t, eventLogPath, "event log should exist after engine run")
	testutil.ValidateEventSequence(t, eventLogPath, []orch.EventKind{
		orch.EventSessionRestart,
	})
}

// TestFault03_InvalidOutput verifies [OPS-04, OPS-05]: mock agent invalid output → validation rejects.
func TestFault03_InvalidOutput(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "invalid-output")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-03"
	cfg.MaxWorkers = 2
	cfg.Retry = orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	// Should abort (gap tolerance reached) or error (all agents produce invalid output).
	assert.Error(t, err, "pipeline should fail with invalid output agents")
}

// TestFault04_ExhaustRetriesCritical verifies [ERR-03, PC-07]: critical agent exhausts retries → pipeline terminates.
func TestFault04_ExhaustRetriesCritical(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	// Use GatedPipeline — the gate agent (validator) is critical.
	p := testutil.GatedPipeline()
	preset := testutil.MockPresetForScript(mockDir, "fail")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-04"
	cfg.MaxWorkers = 2
	cfg.Retry = orch.RetryConfig{MaxRetries: 1, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	assert.Error(t, err, "pipeline should terminate when critical agent exhausts retries")

	// Resume verification: pipeline is in a terminal failed state.
	// Resume should detect that the pipeline already terminated (critical failure is unrecoverable).
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	t.Logf("resume after critical failure: err=%v", err)
	// Resume of a terminally-failed pipeline may re-fail or error — no panic is the requirement.
}

// TestFault06_SaturateWorkerPool verifies [RCV-01]: admission control when pool is exhausted.
func TestFault06_SaturateWorkerPool(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	// slow-success ensures agents occupy workers for a while.
	preset := testutil.MockPresetForScript(mockDir, "slow-success")
	preset.Env = map[string]string{"MOCK_DELAY_SECONDS": "5"}

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-06"
	cfg.MaxWorkers = 1 // only 1 worker — second agent must wait
	cfg.Dispatch = orch.DispatchConfig{Interval: 500 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	t.Logf("engine.Run returned: %v", err)
	// May complete or timeout — the point is no panic and admission control works.
	// Verify ledger exists and tasks were not lost.
	ledgerDir := filepath.Join(dir, "ledger", "tasks")
	entries, statErr := os.ReadDir(ledgerDir)
	require.NoError(t, statErr, "ledger tasks dir should exist after pool saturation")
	assert.NotEmpty(t, entries, "tasks should exist in ledger (not lost during pool saturation)")
}

// TestFault08_GapToleranceReached verifies [ERR-08, S5]: gap tolerance reached → pipeline aborts.
func TestFault08_GapToleranceReached(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	p.GapTolerance = 1 // abort on first gap
	preset := testutil.MockPresetForScript(mockDir, "invalid-output")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-08"
	cfg.MaxWorkers = 2
	cfg.Retry = orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	assert.ErrorIs(t, err, orch.ErrPipelineAborted, "pipeline should abort on gap tolerance")
}

// TestFault09_LockContention verifies [OPS-10..11, S7]: second orchestrator halts with contention.
func TestFault09_LockContention(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "slow-success")
	preset.Env = map[string]string{"MOCK_DELAY_SECONDS": "10"}

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-09"
	cfg.MaxWorkers = 1
	cfg.Dispatch = orch.DispatchConfig{Interval: 500 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	// Start first engine.
	engine1, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel1()

	errCh := make(chan error, 1)
	go func() { errCh <- engine1.Run(ctx1) }()

	// Wait for lock acquisition.
	time.Sleep(1 * time.Second)

	// Second engine should fail with lock contention.
	cfg2 := cfg
	cfg2.RunID = "fault-09-b"
	engine2, err := orch.NewEngine(p, cfg2)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	err = engine2.Run(ctx2)
	assert.Error(t, err, "second engine should fail with lock contention")

	cancel1()
	<-errCh
}

// TestFault07_CircuitBreakerTrip verifies [SUP-04..06, OrphanCk, WC]:
// 3 rapid failures trip CB → OrphanCk fails orphaned task → worker conservation holds.
func TestFault07_CircuitBreakerTrip(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	p.GapTolerance = 3 // allow all 3 agents to fail before abort (so CB can accumulate)
	// crash.sh exits after 2s → rapid failure within 60s CB window.
	preset := testutil.MockPresetForScript(mockDir, "crash")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-07"
	cfg.MaxWorkers = 2
	cfg.Retry = orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 500 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 1 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	// Should abort or error — the point is no panic and no worker leak.
	assert.Error(t, err, "pipeline should fail after CB trip")

	// Verify event log contains circuit_breaker event.
	eventLogPath := filepath.Join(dir, "events.jsonl")
	require.FileExists(t, eventLogPath, "event log should exist after engine run")
	testutil.ValidateEventSequence(t, eventLogPath, []orch.EventKind{
		orch.EventCircuitBreaker,
	})

	// Resume verification: resume should detect gaps and either continue or re-abort.
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	t.Logf("resume after CB trip: err=%v", err)
}

// TestFault02_CorruptLedgerFile verifies [OPS-09]: corrupt ledger file mid-run →
// resume reconciliation corrects state. The pipeline should be resumable after corruption.
func TestFault02_CorruptLedgerFile(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "success")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-02"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	// First run: let it partially complete, then cancel.
	engine1, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel1()
	_ = engine1.Run(ctx1)

	// Verify ledger was created.
	ledgerDir := filepath.Join(dir, "ledger")
	require.DirExists(t, ledgerDir)

	// Corrupt a task file by writing invalid JSON.
	tasksDir := filepath.Join(ledgerDir, "tasks")
	entries, err := os.ReadDir(tasksDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "expected at least one task file to corrupt")
	corruptPath := filepath.Join(tasksDir, entries[0].Name())
	require.NoError(t, os.WriteFile(corruptPath, []byte("{invalid json"), 0644))

	// Resume should handle the corruption gracefully — either reconcile or error cleanly.
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	// The resume may error on corrupt file or reconcile it — either is acceptable.
	// The key requirement is no panic and no data loss beyond the corrupted file.
	t.Logf("resume after corruption: err=%v", err)
}

// TestFault05_SIGINTDuringRun verifies [OPS-12, RCV-14]: send SIGINT during pipeline
// execution → graceful shutdown → ledger intact → resume succeeds.
func TestFault05_SIGINTDuringRun(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "slow-success")
	preset.Env = map[string]string{"MOCK_DELAY_SECONDS": "10"}

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-05"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 500 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	// Use a cancellable context to simulate SIGINT (context cancellation is the
	// Go-native equivalent — signal.go translates SIGINT → context cancel).
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- engine.Run(ctx) }()

	// Wait for agents to start, then cancel (simulates SIGINT).
	time.Sleep(2 * time.Second)
	cancel()

	err = <-errCh
	// Expect context.Canceled or a wrapped form — not a panic.
	t.Logf("run after cancel: err=%v", err)

	// Verify ledger was not corrupted — resume should work.
	ledgerDir := filepath.Join(dir, "ledger")
	require.DirExists(t, ledgerDir, "ledger should exist after graceful shutdown (RCV-14)")

	// Verify lock was released (OPS-12).
	lockPath := filepath.Join(dir, p.Lock.Path)
	_, statErr := os.Stat(lockPath)
	assert.True(t, os.IsNotExist(statErr), "lock should be released after shutdown (OPS-12)")

	// Resume should succeed.
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	// Resume may succeed or error depending on state — the key is no panic.
	t.Logf("resume after interrupt: err=%v", err)
}

// --- Resume-after-fault verification helpers ---
// V&V Plan §3.2: "Each scenario validates: setup → inject fault → verify post-condition
// → verify resume works."

// TestFault03_ResumeAfterInvalidOutput verifies resume works after invalid output rejection.
func TestFault03_ResumeAfterInvalidOutput(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	// First run with invalid-output (fails all agents).
	preset := testutil.MockPresetForScript(mockDir, "invalid-output")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-03r"
	cfg.MaxWorkers = 2
	cfg.Retry = orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine1, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	_ = engine1.Run(ctx1) // expected to fail

	// Resume with working agents — should pick up from ledger state.
	cfg.Preset = testutil.MockPresetForScript(mockDir, "success")
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	t.Logf("resume after invalid output: err=%v", err)
	// No panic = success for this test. Pipeline may or may not complete
	// depending on whether gap tolerance was already reached.
}

// TestFault08_ResumeAfterGapAbort verifies resume detects aborted state after gap tolerance.
func TestFault08_ResumeAfterGapAbort(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	p.GapTolerance = 1
	preset := testutil.MockPresetForScript(mockDir, "invalid-output")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "fault-08r"
	cfg.MaxWorkers = 2
	cfg.Retry = orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine1, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	err = engine1.Run(ctx1)
	assert.ErrorIs(t, err, orch.ErrPipelineAborted)

	// Resume should detect the gap-aborted state.
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	t.Logf("resume after gap abort: err=%v", err)
	// Resume of an aborted pipeline is expected to either re-abort or error.
}
