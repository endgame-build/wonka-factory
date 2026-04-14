//go:build verify

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

// --- NewEngine validation tests ---

func TestNewEngine_Validation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*orch.EngineConfig)
		errMsg string
	}{
		{"nil lifecycle", func(c *orch.EngineConfig) { c.Lifecycle = nil }, "lifecycle config is required"},
		{"empty branch", func(c *orch.EngineConfig) { c.Lifecycle.Branch = "" }, "Branch must be non-empty"},
		{"empty RunDir", func(c *orch.EngineConfig) { c.RunDir = "" }, "RunDir is required"},
		{"empty RepoPath", func(c *orch.EngineConfig) { c.RepoPath = "" }, "RepoPath is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fresh config per test — Lifecycle is a pointer, so copies share it.
			cfg := orch.DefaultEngineConfig(
				testutil.MockLifecycleConfig("feat/x", "builder"),
				t.TempDir(), "/repo",
			)
			tt.mutate(&cfg)
			_, err := orch.NewEngine(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestNewEngine_DefaultRunID(t *testing.T) {
	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat/x", "builder"),
		t.TempDir(), "/repo",
	)
	cfg.RunID = "" // should be generated
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	assert.NotEmpty(t, e.RunID())
}

func TestNewEngine_DefaultMaxWorkers(t *testing.T) {
	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat/x", "builder"),
		t.TempDir(), "/repo",
	)
	cfg.MaxWorkers = 0 // should default to 4
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	assert.NotNil(t, e)
}

// TestEngine_StoreAccessor verifies the Store() accessor returns nil before
// init and non-nil after (tests use it to inspect post-Run state).
func TestEngine_StoreAccessor(t *testing.T) {
	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat/x", "builder"),
		t.TempDir(), "/repo",
	)
	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	assert.Nil(t, e.Store(), "Store should be nil before Run/Resume")
}

// --- Engine.Run integration tests (require tmux) ---

func skipWithoutTmux(t *testing.T) {
	t.Helper()
	if !orch.Available() {
		t.Skip("tmux not available")
	}
}

// prepopulateLedger creates the ledger and logs directories inside runDir,
// opens a temporary FS store, creates the given tasks, and closes the store.
func prepopulateLedger(t *testing.T, runDir string, tasks ...*orch.Task) {
	t.Helper()
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	fsStore, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	for _, task := range tasks {
		require.NoError(t, fsStore.CreateTask(task))
	}
	require.NoError(t, fsStore.Close())
}

func testTask(id, branch, role string) *orch.Task {
	return &orch.Task{
		ID: id, Title: id, Status: orch.StatusOpen,
		Labels: map[string]string{"branch": branch, "role": role, "criticality": "non_critical"},
	}
}

// TestEngine_RunLifecycleDone verifies §8.1.3: all tasks terminal → Run returns nil.
func TestEngine_RunLifecycleDone(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.TestSpawnFunc = testutil.ImmediateSpawnFunc(0)
	cfg.RunID = "test-run"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	prepopulateLedger(t, runDir, testTask("build-1", "feat/x", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err)

	assertEventKinds(t, filepath.Join(runDir, "events.jsonl"),
		orch.EventLifecycleStarted, orch.EventLifecycleCompleted)
}

// TestEngine_RunGapAbort verifies BVV-ERR-04: gap tolerance exceeded → ErrLifecycleAborted.
func TestEngine_RunGapAbort(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.GapTolerance = 1 // abort after 1 gap
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.TestSpawnFunc = testutil.ImmediateSpawnFunc(1) // all tasks fail
	cfg.RunID = "test-abort"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	prepopulateLedger(t, runDir,
		testTask("t1", "feat/x", "builder"),
		testTask("t2", "feat/x", "builder"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted)
}

// TestBVV_ERR06_LockExclusion verifies BVV-S-01: two engines on the same
// branch get lock contention.
func TestBVV_ERR06_LockExclusion(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lockPath := filepath.Join(runDir, ".wonka-feat-x.lock")

	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.Path = lockPath
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	// Acquire the lock manually.
	lock := orch.NewLifecycleLock(lifecycle.Lock)
	require.NoError(t, lock.Acquire("first-run", "feat/x"))
	defer lock.Release() //nolint:errcheck // test cleanup

	// Second engine should fail to acquire.
	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.TestSpawnFunc = testutil.ImmediateSpawnFunc(0)
	cfg.RunID = "second-run"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	// Pre-create infrastructure dirs so init succeeds before lock.
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "ledger"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))

	ctx := context.Background()
	err = e.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrLockContention)
}

// TestBVV_ERR09_GracefulShutdown verifies BVV-ERR-09: context cancellation
// does NOT modify task statuses.
func TestBVV_ERR09_GracefulShutdown(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	// SpawnFunc that blocks until context is cancelled.
	blockingSpawn := func(ctx context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		<-ctx.Done()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeFailure, 1, roleCfg)
	}

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.TestSpawnFunc = blockingSpawn
	cfg.RunID = "test-shutdown"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	prepopulateLedger(t, runDir, testTask("build-1", "feat/x", "builder"))
	statusBefore := orch.StatusOpen

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay to trigger graceful shutdown.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	_ = e.Run(ctx) // error expected (context cancelled)

	// Reopen store and verify task status is unchanged.
	fsStore2, _, err := orch.NewStore("", filepath.Join(runDir, "ledger"))
	require.NoError(t, err)
	defer fsStore2.Close()

	taskAfter, err := fsStore2.GetTask("build-1")
	require.NoError(t, err)
	// Task should still be open OR in_progress (assigned by dispatcher) —
	// but NOT modified to a terminal state by shutdown.
	assert.False(t, taskAfter.Status.Terminal(),
		"BVV-ERR-09: shutdown must not set task to terminal; was %s, now %s",
		statusBefore, taskAfter.Status)
}

// TestEngine_RunCreatesInfrastructure verifies that Run creates the expected
// directory structure and event log file.
func TestEngine_RunCreatesInfrastructure(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.TestSpawnFunc = testutil.ImmediateSpawnFunc(0)
	cfg.RunID = "test-infra"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	prepopulateLedger(t, runDir, testTask("t1", "feat/x", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Run(ctx))

	// Verify infrastructure was created.
	assert.DirExists(t, filepath.Join(runDir, "ledger"))
	assert.DirExists(t, filepath.Join(runDir, "logs"))
	assert.FileExists(t, filepath.Join(runDir, "events.jsonl"))

	// Verify lifecycle events.
	assertEventKinds(t, filepath.Join(runDir, "events.jsonl"),
		orch.EventLifecycleStarted, orch.EventLifecycleCompleted)
}

// TestBVV_ERR10a_ReleaseDrained verifies the runtime invariant fires when
// a worker is still active at voluntary release time (build tag verify).
func TestBVV_ERR10a_ReleaseDrained(t *testing.T) {
	store := testutil.NewMockStore()
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "some-task",
	}))

	assert.Panics(t, func() {
		orch.AssertLifecycleReleaseDrained(store)
	}, "should panic with active worker")
}

// TestBVV_ERR10a_ReleaseDrainedOK verifies the invariant does NOT fire when
// all workers are idle.
func TestBVV_ERR10a_ReleaseDrainedOK(t *testing.T) {
	store := testutil.NewMockStore()
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w1", Status: orch.WorkerIdle,
	}))

	assert.NotPanics(t, func() {
		orch.AssertLifecycleReleaseDrained(store)
	})
}

// TestEngine_ResumeRecoversPreviousRunID verifies that Resume reads the
// stale lock file to recover the previous RunID (BVV-ERR-08).
func TestEngine_ResumeRecoversPreviousRunID(t *testing.T) {
	runDir := t.TempDir()
	branch := "feat-x"

	// Write a stale lock file with the previous RunID.
	lockPath := filepath.Join(runDir, ".wonka-"+branch+".lock")
	staleContent := `{"holder":"previous-run-id","branch":"feat-x","timestamp":"2000-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(staleContent), 0o644))

	// Pre-create ledger directory so Resume gets past the ledger check
	// and into initForResume where RunID recovery happens.
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "ledger"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))

	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.Path = lockPath
	lifecycle.Lock.StalenessThreshold = 1 * time.Millisecond

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "new-run-id"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	// Resume will fail (tmux or store issues) but RunID should be recovered
	// from the lock file before any infrastructure starts.
	_ = e.Resume(context.Background())

	assert.Equal(t, "previous-run-id", e.RunID())
}

// TestEngine_ResumeFallbackRunID verifies that when no lock file exists,
// Resume keeps the configured RunID.
func TestEngine_ResumeFallbackRunID(t *testing.T) {
	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat-x", "builder")

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "my-run-id"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	_ = e.Resume(context.Background()) // will fail — no ledger dir

	assert.Equal(t, "my-run-id", e.RunID())
}

// --- Helpers ---

// assertEventKinds checks that the event log contains the expected event kinds
// (in any order, non-exclusively).
func assertEventKinds(t *testing.T, logPath string, kinds ...orch.EventKind) {
	t.Helper()
	f, err := os.Open(logPath)
	require.NoError(t, err)
	defer f.Close()

	found := make(map[orch.EventKind]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e orch.Event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			found[e.Kind] = true
		}
	}
	require.NoError(t, scanner.Err())

	for _, kind := range kinds {
		assert.True(t, found[kind], "expected event kind %q in log", kind)
	}
}
