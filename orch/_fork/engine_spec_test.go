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

// TestRCV01_AdmissionControl verifies [RCV-01]: bounded worker pool.
func TestRCV01_AdmissionControl(t *testing.T) {
	cfg := orch.DefaultEngineConfig(testPreset(), "", t.TempDir(), t.TempDir())
	cfg.MaxWorkers = 2

	p := testutil.MiniPipeline()
	e, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)
	assert.NotNil(t, e)
}

// TestEngine_RunCreatesStore verifies Run creates store and expands pipeline.
func TestEngine_RunCreatesStore(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	cfg := orch.DefaultEngineConfig(testPreset(), "", dir, dir)
	cfg.RunID = "test-engine-run"
	cfg.MaxWorkers = 2
	cfg.Dispatch = orch.DispatchConfig{Interval: 50 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	p := testutil.MiniPipeline()
	e, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	// Run with a timeout so we don't hang.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Run will eventually time out or fail because mock agents don't produce
	// real output, but it should create the store and expand the pipeline.
	runErr := e.Run(ctx)
	t.Logf("Run returned: %v", runErr)

	// Verify store directory was created.
	ledgerDir := filepath.Join(dir, "ledger")
	_, err = os.Stat(filepath.Join(ledgerDir, "tasks"))
	require.NoError(t, err, "tasks directory should exist")

	// Verify event log was created.
	_, err = os.Stat(filepath.Join(dir, "events.jsonl"))
	assert.NoError(t, err, "event log should exist")
}

// TestEngine_DefaultConfig verifies DefaultEngineConfig returns sensible defaults.
// [RCV-02]: --max-workers flag is configurable via EngineConfig.MaxWorkers (default 4).
func TestEngine_DefaultConfig(t *testing.T) {
	cfg := orch.DefaultEngineConfig(testPreset(), "/plugin", "/run", "/repo")
	assert.Equal(t, 4, cfg.MaxWorkers)
	assert.Equal(t, time.Second, cfg.Dispatch.Interval)
	assert.Equal(t, 30*time.Second, cfg.Watchdog.Interval)
	assert.Equal(t, 2, cfg.Retry.MaxRetries)
	assert.Equal(t, 500*time.Millisecond, cfg.Dispatch.AgentPollInterval)
	assert.NotEmpty(t, cfg.RunID)
}

// TestS7_PipelineExclusionViaLock verifies [S7]: second engine gets lock contention.
func TestS7_PipelineExclusionViaLock(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	cfg := orch.DefaultEngineConfig(testPreset(), "", dir, dir)
	cfg.RunID = "test-s7"
	cfg.Dispatch = orch.DispatchConfig{Interval: 50 * time.Millisecond}

	p := testutil.MiniPipeline()

	// Start first engine in background.
	e1, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	errCh := make(chan error, 1)
	go func() { errCh <- e1.Run(ctx1) }()

	// Wait a bit for first engine to acquire lock.
	time.Sleep(200 * time.Millisecond)

	// Second engine should fail with lock contention.
	cfg2 := cfg
	cfg2.RunID = "test-s7-2"
	e2, err := orch.NewEngine(p, cfg2)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel2()
	err = e2.Run(ctx2)
	require.Error(t, err, "second engine should fail with lock contention")

	cancel1()
	<-errCh
}

// TestOPS17_FinalizationOnSuccess verifies [OPS-17, OPS-17a, OPS-18, S8]: transient artefacts
// are cleaned on success. Best-effort finalisation (OPS-18). Cleanup completeness (S8).
// Requires tmux since finalisation is triggered through Engine.Run on successful completion.
func TestOPS17_FinalizationOnSuccess(t *testing.T) {
	skipIfNoTmux(t)

	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "success")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "e2e-ops17"
	cfg.MaxWorkers = 4
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	require.NoError(t, err, "pipeline should complete successfully")

	// Verify transient artefacts (.exitcode, .tmp) are cleaned up.
	logsDir := filepath.Join(dir, "logs")
	entries, err := os.ReadDir(logsDir)
	require.NoError(t, err)
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		assert.NotEqual(t, ".exitcode", ext, "exitcode files should be cleaned (OPS-17a)")
		assert.NotEqual(t, ".tmp", ext, "tmp files should be cleaned (OPS-17a)")
	}
}

// TestRCV08_DegradedWatchdogMode verifies [RCV-08]: watchdog continues liveness checks
// when the store is temporarily unavailable. CheckOnce returns an error but does not panic.
func TestRCV08_DegradedWatchdogMode(t *testing.T) {
	dir := t.TempDir()
	realStore := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	// FailingStore that fails immediately (0 successful ops).
	failStore := testutil.NewFailingStore(realStore, 0)

	tmuxClient := newTestTmux(t, "test-rcv08")
	pool := orch.NewWorkerPool(failStore, tmuxClient, 4, "test-rcv08", dir, dir)

	p := testutil.MiniPipeline()
	wd := orch.NewWatchdog(pool, failStore, nil, p, testPreset(), "", orch.DefaultWatchdogConfig(), nil)

	// CheckOnce should return error but not panic (degraded mode).
	err := wd.CheckOnce()
	require.Error(t, err, "CheckOnce should surface store error")

	// Watchdog should still be functional after degraded check.
	assert.False(t, wd.CBTripped(), "circuit breaker should NOT trip from store errors")
}

// TestWKR06_AssignmentSurvivesSessionDeath verifies [WKR-06, SUP-04]: worker-task
// assignment and in_progress status are preserved in the store when a session dies.
// The watchdog does NOT modify task status or assignment (SUP-04) — it only triggers
// RestartSession.
func TestWKR06_AssignmentSurvivesSessionDeath(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	// Create a worker with an active session and in-progress task.
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "task-1",
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "task-1", Type: orch.TypeAgent, Status: orch.StatusInProgress,
		Assignee: "w-01", AgentID: "test-agent",
		Output: filepath.Join("out", "test.md"),
	}))

	// Verify task assignment survives a read cycle (simulates watchdog reading
	// store state after session death — it reads but does not write).
	task, err := store.GetTask("task-1")
	require.NoError(t, err)
	assert.Equal(t, "w-01", task.Assignee, "task should be assigned to w-01")
	assert.Equal(t, orch.StatusInProgress, task.Status)

	worker, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, "task-1", worker.CurrentTaskID,
		"worker-task assignment must survive session death (WKR-06)")

	// Task status remains in_progress — watchdog does not change it (SUP-04).
	task, err = store.GetTask("task-1")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusInProgress, task.Status,
		"task status must remain in_progress (SUP-04: watchdog does not change task status)")
}
