//go:build verify

package orch_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
// opens a temporary FS store, creates the given tasks, closes the store, and
// seeds a parseable lifecycle_started event so Resume tests pass the
// ErrResumeNoEventLog sentinel check.
func prepopulateLedger(t *testing.T, runDir string, tasks ...*orch.Task) {
	t.Helper()
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	seedFreshEventLog(t, runDir)
	fsStore, _, err := orch.NewStore("", ledgerDir)
	require.NoError(t, err)
	for _, task := range tasks {
		require.NoError(t, fsStore.CreateTask(task))
	}
	require.NoError(t, fsStore.Close())
}

// seedFreshEventLog drops a single parseable lifecycle_started record into
// runDir/events.jsonl. Used by Resume tests to satisfy the event-log
// sentinel without driving an engine. The file is opened with O_APPEND
// elsewhere, so this seed is preserved even if engine.init runs after.
func seedFreshEventLog(t *testing.T, runDir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "events.jsonl"),
		[]byte(`{"kind":"lifecycle_started","summary":"seeded","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644))
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
	cfg.RunID = "test-run"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

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
	cfg.RunID = "test-abort"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(1)) // all tasks fail

	prepopulateLedger(t, runDir,
		testTask("t1", "feat/x", "builder"),
		testTask("t2", "feat/x", "builder"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted)

	// Audit trail must distinguish abort from normal completion — the event
	// kind is shared but Detail carries the abort marker.
	assertEventDetailContains(t, filepath.Join(runDir, "events.jsonl"),
		orch.EventLifecycleCompleted, "outcome=aborted")
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
	cfg.RunID = "second-run"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

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
	cfg.RunID = "test-shutdown"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(blockingSpawn)

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
	cfg.RunID = "test-infra"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

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

	// Pre-create ledger directory + event log so Resume gets past the
	// sentinel check and into initForResume where RunID recovery happens.
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "ledger"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	seedFreshEventLog(t, runDir)

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

// TestEngine_ResumeFallbackRunID verifies that a Resume failing in
// initForResume preserves the configured RunID rather than zeroing or
// regenerating it. The lock-recovery success path (event log present,
// lock file present) is covered separately by
// TestEngine_ResumeStaleLockRecoversRunID; this test pins the
// failure-mode invariant.
//
// With no event log, initForResume short-circuits at ErrResumeNoEventLog
// before any RunID mutation, so the configured value must survive.
func TestEngine_ResumeFallbackRunID(t *testing.T) {
	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat-x", "builder")

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "my-run-id"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err) // ErrResumeNoEventLog — no prior wonka run on this branch
	assert.Equal(t, "my-run-id", e.RunID())
}

// TestEngine_ResumeResetsHandoffOnReopen verifies BVV-S-02a end-to-end
// through Engine.Resume — when an event-log terminal task is now open, the
// handoff counter for that task is reset and an escalation_resolved event
// is emitted. Without this coverage, a regression that drops the
// handoffs.Reset call inside Engine.Resume would slip past unit tests of
// HandoffState alone.
func TestEngine_ResumeResetsHandoffOnReopen(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat-x"
	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	// Pre-populate ledger with a task that is currently open.
	prepopulateLedger(t, runDir, testTask("build-1", branch, "builder"))

	// Pre-write event log saying the task was previously completed (terminal),
	// then a handoff record (so handoff count > 0 is recovered).
	logPath := filepath.Join(runDir, "events.jsonl")
	f, err := os.Create(logPath)
	require.NoError(t, err)
	enc := json.NewEncoder(f)
	require.NoError(t, enc.Encode(orch.Event{
		Kind: orch.EventTaskHandoff, TaskID: "build-1", Timestamp: time.Now(),
	}))
	require.NoError(t, enc.Encode(orch.Event{
		Kind: orch.EventTaskCompleted, TaskID: "build-1", Timestamp: time.Now(),
	}))
	require.NoError(t, f.Close())

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "test-reopen"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Resume(ctx))

	// Verify escalation_resolved event was emitted for build-1 — proves that
	// Engine.Resume's HumanReopens loop ran (which is where retries.ResetTask
	// AND handoffs.Reset are both called). Without this, BVV-S-02a coverage
	// for the handoff counter is structural-only.
	assertEventForTask(t, logPath, orch.EventEscalationResolved, "build-1")
}

// TestEngine_ResumeLockContention verifies BVV-S-01 / BVV-ERR-06 on the
// Resume path. Today only Run is covered; Resume is the more likely
// human-invoked path after a crash, so contention against a still-live
// holder must also fail-fast rather than silently overlap.
func TestEngine_ResumeLockContention(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat-x"
	lockPath := filepath.Join(runDir, ".wonka-"+branch+".lock")

	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.Path = lockPath
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	// First holder (still alive — recent timestamp).
	first := orch.NewLifecycleLock(lifecycle.Lock)
	require.NoError(t, first.Acquire("first-run", branch))
	defer first.Release() //nolint:errcheck // test cleanup

	// Pre-create ledger + event log so initForResume passes the sentinel.
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "ledger"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	seedFreshEventLog(t, runDir)

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "second-run"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrLockContention)
}

// TestEngine_ResumeReconcilesBeforeDispatch verifies BVV-ERR-07: Reconcile
// completes before dispatch resumes. Closes the structural-only coverage gap
// by setting up a state only Reconcile can unblock (in_progress task with
// dead session): if dispatch ran first, the task would stay in_progress and
// never terminate; only after Reconcile resets it to open can dispatch pick
// it up. Task reaching completed proves the ordering.
func TestEngine_ResumeReconcilesBeforeDispatch(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat-x"

	// Pre-populate ledger with a task in status=in_progress (pointing at a
	// worker and a dead session). Without Reconcile resetting it, dispatch
	// would never pick this task up.
	task := testTask("build-1", branch, "builder")
	task.Status = orch.StatusInProgress
	task.Assignee = "w-dead"
	prepopulateLedger(t, runDir, task)

	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "resume-order"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Resume(ctx))

	// Task must have terminated successfully — proves Reconcile ran before
	// dispatch (otherwise dispatch would never have queued it).
	fsStore, _, err := orch.NewStore("", filepath.Join(runDir, "ledger"))
	require.NoError(t, err)
	defer fsStore.Close()
	got, err := fsStore.GetTask("build-1")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusCompleted, got.Status)
}

// TestEngine_ResumeSurfacesEventLogCorruption verifies that corrupt JSONL
// lines in a pre-existing event log surface via lifecycle_started.Detail.
// Without this, C2's counter reaches ResumeResult but never the audit
// trail — the operator cannot see that recovery was partial.
func TestEngine_ResumeSurfacesEventLogCorruption(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat-x"
	prepopulateLedger(t, runDir, testTask("build-1", branch, "builder"))

	// Pre-write an event log with one valid + two corrupt lines.
	logPath := filepath.Join(runDir, "events.jsonl")
	f, err := os.Create(logPath)
	require.NoError(t, err)
	enc := json.NewEncoder(f)
	require.NoError(t, enc.Encode(orch.Event{
		Kind: orch.EventGapRecorded, TaskID: "some-task", Timestamp: time.Now(),
	}))
	_, _ = f.WriteString("not valid json\n")
	_, _ = f.WriteString("{truncated\n")
	require.NoError(t, f.Close())

	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "resume-corrupt"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Resume(ctx))

	assertEventDetailContains(t, logPath, orch.EventLifecycleStarted, "event_log_corrupt_lines=2")
}

// TestEngine_RunSurfacesStoreFallback verifies that when the requested store
// backend falls back to a different one (e.g. beads → fs when Dolt is
// unavailable), the audit trail carries that signal. Skipped when the
// environment can actually serve the requested backend (no fallback occurs).
func TestEngine_RunSurfacesStoreFallback(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "test-fallback"
	// Empty LedgerKind preserves the beads→fs fallback semantics (NewStore
	// only falls back when the caller did not explicitly request beads).
	// Explicit LedgerBeads is strict by design — the auto-init path requires
	// `bd` on PATH and would not fall back. The audit-trail signal we're
	// pinning here applies to the empty-kind path.
	cfg.LedgerKind = ""
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	// Skip if Beads is actually reachable — the fallback would not fire
	// and the assertion would be vacuously wrong.
	if _, kind, err := orch.NewStore("", filepath.Join(t.TempDir(), "probe")); err == nil && kind == orch.LedgerBeads {
		t.Skip("Beads backend reachable — fallback path cannot be exercised")
	}

	prepopulateLedger(t, runDir, testTask("t1", "feat/x", "builder"))

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Run(ctx))

	assertEventDetailContains(t, filepath.Join(runDir, "events.jsonl"),
		orch.EventLifecycleStarted, "store_fallback=beads->")
}

// TestBVV_ERR10a_CheckReleaseDrained verifies the production-observable
// check helper used by runLoop in non-verify builds. CheckReleaseDrained
// runs in any build (unlike AssertLifecycleReleaseDrained which panics only
// under -tags verify and is a no-op otherwise) so release builds can still
// emit an audit-trail warning when a BVV-ERR-10a violation occurs.
func TestBVV_ERR10a_CheckReleaseDrained(t *testing.T) {
	t.Run("empty store → no busy workers", func(t *testing.T) {
		store := testutil.NewMockStore()
		assert.Empty(t, orch.CheckReleaseDrained(store))
	})

	t.Run("idle workers → no busy workers", func(t *testing.T) {
		store := testutil.NewMockStore()
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		assert.Empty(t, orch.CheckReleaseDrained(store))
	})

	t.Run("active worker → reported busy", func(t *testing.T) {
		store := testutil.NewMockStore()
		require.NoError(t, store.CreateWorker(&orch.Worker{
			Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "t1",
		}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerIdle}))
		busy := orch.CheckReleaseDrained(store)
		assert.Equal(t, []string{"w1"}, busy)
	})
}

// TestEngine_DoubleRunRejected verifies single-use Engine semantics: a
// second Run/Resume call on the same instance returns
// ErrEngineAlreadyStarted rather than re-initialising and double-acquiring
// the lock.
func TestEngine_DoubleRunRejected(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "single-use"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	prepopulateLedger(t, runDir, testTask("t1", "feat/x", "builder"))

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// First call runs to completion so we know the once-guard was consumed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Run(ctx))

	// Second Run/Resume should be rejected by the guard, with no lock
	// re-acquire, no tmux re-init, no audit-trail duplicate.
	err = e.Run(context.Background())
	assert.ErrorIs(t, err, orch.ErrEngineAlreadyStarted)

	err = e.Resume(context.Background())
	assert.ErrorIs(t, err, orch.ErrEngineAlreadyStarted)
}

// TestEngine_RunInvokesSeed pins the EngineConfig.Seed contract: the callback
// MUST fire exactly once on a fresh Run, with the engine's open store and
// while the lifecycle lock is held. Without this test, a future refactor
// could silently drop the `if e.cfg.Seed != nil` block in Engine.Run and
// every CLI seeding test would still pass (the CLI tests assert SeedPlannerTask
// in isolation, not its invocation by the engine).
//
// The callback creates a single task; we then prove it was created (proves
// the store argument is real and writable) and that the lifecycle completed
// (proves dispatch ran AFTER the seed, not before or instead of).
func TestEngine_RunInvokesSeed(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "seed-invoked"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	var calls atomic.Int32
	cfg.Seed = func(store orch.Store) error {
		calls.Add(1)
		require.NotNil(t, store, "seed must receive the engine's open store")
		// Create a task here so the dispatch loop has work — proves the seed
		// runs *before* the loop starts polling, and proves the store is
		// writable from inside the callback.
		return store.CreateTask(testTask("seeded-task", "feat/x", "builder"))
	}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Run(ctx))

	assert.Equal(t, int32(1), calls.Load(), "seed must fire exactly once per Run")

	// Re-open the store to verify the seeded task survived (it should have
	// been picked up and completed by the dispatch loop).
	store, _, err := orch.NewStore("", filepath.Join(runDir, "ledger"))
	require.NoError(t, err)
	defer store.Close()
	got, err := store.GetTask("seeded-task")
	require.NoError(t, err, "seeded task must be persisted in the ledger")
	assert.Equal(t, orch.StatusCompleted, got.Status, "seeded task must have been dispatched and completed")
}

// TestEngine_RunSeedErrorAborts verifies the failure semantics of the Seed
// hook: a non-nil error returned from Seed unwinds the lifecycle before
// dispatch starts. The error must wrap, so callers can errors.Is/As against
// their own sentinels for triage.
func TestEngine_RunSeedErrorAborts(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "seed-error"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	sentinel := errors.New("synthetic seed failure")
	var dispatchEntered atomic.Bool
	cfg.Seed = func(_ orch.Store) error { return sentinel }

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	// If dispatch were entered, this spawn func would record it. We expect
	// it to never fire — the seed error must abort before runLoop polls.
	e.SetTestSpawnFunc(func(_ context.Context, _ *orch.Task, _ *orch.Worker, _ orch.RoleConfig, _ int, _ chan<- orch.TaskOutcome) {
		dispatchEntered.Store(true)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = e.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "seed errors must wrap so callers can match their sentinels")
	assert.False(t, dispatchEntered.Load(), "dispatch must not run when seed fails")

	// Seed failure must still emit the §10.3 terminal anchor.
	assertEventDetailContains(t, filepath.Join(runDir, "events.jsonl"),
		orch.EventLifecycleCompleted, "outcome=failed")
}

// newBeadsPathEngine builds an engine pinned to <repo>/.beads/ via the
// SetTestLedgerDir seam, with the supplied Seed callback. LedgerKind stays
// empty: combined with the override, the engine resolves to beadsDir
// (override wins) without triggering strict beads.Open (which would require
// a live Dolt server). Tests built on this fixture assert the *routing
// contract* — wonka and Charlie agree on the path — not the beads transport.
func newBeadsPathEngine(t *testing.T, runID string, seed func(orch.Store) error) (e *orch.Engine, runDir, beadsDir string) {
	t.Helper()
	runDir = t.TempDir()
	repo := t.TempDir()
	beadsDir = filepath.Join(repo, ".beads")

	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, repo)
	cfg.RunID = runID
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}
	cfg.Seed = seed

	var err error
	e, err = orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestLedgerDir(beadsDir)
	return e, runDir, beadsDir
}

// TestEngine_RunInvokesSeed_BeadsPath pins that the Seed callback receives
// the store opened at the beads-resolved location (<repo>/.beads/), not the
// FS path. Without this, a regression that resolved beads to <runDir>/ledger
// would silently work in tests (FS still seedable) but break end-to-end
// because Charlie writes to <repo>/.beads/ — the BVV-DSN-04 contract this
// PR exists to enforce.
func TestEngine_RunInvokesSeed_BeadsPath(t *testing.T) {
	skipWithoutTmux(t)

	var calls atomic.Int32
	e, runDir, beadsDir := newBeadsPathEngine(t, "seed-beads", func(store orch.Store) error {
		calls.Add(1)
		require.NotNil(t, store, "seed must receive the engine's open store")
		return store.CreateTask(testTask("beads-seed", "feat/x", "builder"))
	})
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Run(ctx))

	assert.Equal(t, int32(1), calls.Load(), "seed must fire exactly once per Run")

	store, _, err := orch.NewStore("", beadsDir)
	require.NoError(t, err)
	defer store.Close()
	got, err := store.GetTask("beads-seed")
	require.NoError(t, err, "seeded task must be persisted at <repo>/.beads/")
	assert.Equal(t, orch.StatusCompleted, got.Status, "seeded task must have been dispatched and completed")

	// Belt-and-braces: the legacy per-run-dir ledger directory must NOT
	// exist. If it did, a regression silently bypassed the new resolver.
	assert.NoDirExists(t, filepath.Join(runDir, "ledger"),
		"legacy <runDir>/ledger must not be created when LedgerBeads is resolved")
}

// TestEngine_RunSeedErrorAborts_BeadsPath mirrors TestEngine_RunSeedErrorAborts
// against the beads-resolved path so the failure semantics survive the
// new resolution logic too. A regression that swallowed seed errors only
// for one backend would otherwise slip past the FS-only test.
func TestEngine_RunSeedErrorAborts_BeadsPath(t *testing.T) {
	skipWithoutTmux(t)

	sentinel := errors.New("synthetic seed failure (beads)")
	e, _, _ := newBeadsPathEngine(t, "seed-error-beads", func(_ orch.Store) error { return sentinel })
	var dispatchEntered atomic.Bool
	e.SetTestSpawnFunc(func(_ context.Context, _ *orch.Task, _ *orch.Worker, _ orch.RoleConfig, _ int, _ chan<- orch.TaskOutcome) {
		dispatchEntered.Store(true)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := e.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "seed errors must wrap so callers can match their sentinels")
	assert.False(t, dispatchEntered.Load(), "dispatch must not run when seed fails")
}

// SetTestLedgerDir must bypass EnsureBeadsInitialised even when
// LedgerKind == LedgerBeads — otherwise tests pointing beads at a
// controlled t.TempDir still need `bd` on PATH.
func TestEngine_SetTestLedgerDirBypassesAutoInit(t *testing.T) {
	t.Setenv("PATH", "")

	runDir := t.TempDir()
	repo := t.TempDir()
	override := filepath.Join(t.TempDir(), ".beads")

	lifecycle := testutil.MockLifecycleConfig("feat-x", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, repo)
	cfg.RunID = "bypass-test"
	cfg.LedgerKind = orch.LedgerBeads // would normally trip auto-init
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestLedgerDir(override)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = e.Run(ctx)
	require.Error(t, err)
	assert.NotErrorIs(t, err, orch.ErrBeadsCLIMissing)
}

// TestEngine_FullLifecycle_BeadsBackend is the load-bearing assertion this
// PR exists for. With the dispatcher reading from <repo>/.beads/ (the same
// store the planner writes to), a fresh Run with a seed that creates a
// builder + verifier + gate task must reach lifecycle_completed instead
// of aborting at BVV-TG-09 ("0 gate tasks expected 1") — the failure
// observed in PR #20's Level 4 run that motivated this change.
//
// Drives a fake planner via SetTestSpawnFunc that creates the full task
// graph in the same store wonka opens. Uses SetTestLedgerDir to bypass
// the bd auto-init (covered by gated unit tests) and pin the beads
// directory to a controlled t.TempDir()/.beads.
func TestEngine_FullLifecycle_BeadsBackend(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	repo := t.TempDir()
	beadsDir := filepath.Join(repo, ".beads")
	branch := "feat/x"

	lifecycle := testutil.MockLifecycleConfig(branch, "builder", "verifier", "gate")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	// Disable graph validation so we don't have to spin up the gate role's
	// PR machinery; the assertion here is "lifecycle reaches completion,"
	// not "gate task PR-creates."
	lifecycle.ValidateGraph = false

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, repo)
	cfg.RunID = "full-beads"
	// LedgerKind stays empty: combined with SetTestLedgerDir below, the
	// engine resolves to the injected beadsDir (override wins) without
	// triggering strict beads.Open (which would require a live Dolt server).
	// The test asserts the *routing contract* — wonka and Charlie agree on
	// the path — not the beads transport.
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	// Seed simulates Charlie: write the planner task plus the build/verify/gate
	// graph into the same store the dispatcher will read from. Before this
	// PR, the planner wrote to <repo>/.beads/ while the dispatcher read from
	// <runDir>/ledger/, so this graph would have been invisible.
	cfg.Seed = func(store orch.Store) error {
		// Planner task — completed up front (the seed is taking the planner's
		// place; we're testing dispatch through builder/verifier/gate).
		planner := testTask("plan-x", branch, "planner")
		planner.Status = orch.StatusCompleted
		if err := store.CreateTask(planner); err != nil {
			return err
		}
		build := testTask("build-1", branch, "builder")
		if err := store.CreateTask(build); err != nil {
			return err
		}
		verify := testTask("verify-1", branch, "verifier")
		if err := store.CreateTask(verify); err != nil {
			return err
		}
		if err := store.AddDep(verify.ID, build.ID); err != nil {
			return err
		}
		gate := testTask("gate-1", branch, "gate")
		if err := store.CreateTask(gate); err != nil {
			return err
		}
		return store.AddDep(gate.ID, verify.ID)
	}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestLedgerDir(beadsDir)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, e.Run(ctx),
		"full lifecycle must complete cleanly — pre-fix this aborted at BVV-TG-09 because the planner wrote to <repo>/.beads/ while dispatch read <runDir>/ledger/")

	// Confirm every non-planner task terminated successfully against the
	// shared store.
	store, _, err := orch.NewStore("", beadsDir)
	require.NoError(t, err)
	defer store.Close()
	for _, id := range []string{"build-1", "verify-1", "gate-1"} {
		got, err := store.GetTask(id)
		require.NoError(t, err, "task %s must be readable from the shared ledger", id)
		assert.Equal(t, orch.StatusCompleted, got.Status,
			"task %s must reach completed (single-ledger contract)", id)
	}

	// Lifecycle-completed event with no abort marker — pre-fix this would
	// have carried "outcome=aborted".
	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventLifecycleStarted, orch.EventLifecycleCompleted)
}

// --- Helpers ---

// assertEventDetailContains asserts the log contains an event of the given
// kind whose Detail field contains the given substring.
func assertEventDetailContains(t *testing.T, logPath string, kind orch.EventKind, needle string) {
	t.Helper()
	f, err := os.Open(logPath)
	require.NoError(t, err)
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e orch.Event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			if e.Kind == kind && strings.Contains(e.Detail, needle) {
				return
			}
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("expected event kind %q with Detail containing %q in %s", kind, needle, logPath)
}

// assertEventForTask asserts the log contains an event of the given kind
// for the given task ID.
func assertEventForTask(t *testing.T, logPath string, kind orch.EventKind, taskID string) {
	t.Helper()
	f, err := os.Open(logPath)
	require.NoError(t, err)
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e orch.Event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			if e.Kind == kind && e.TaskID == taskID {
				return
			}
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("expected event kind %q for task %q in %s", kind, taskID, logPath)
}

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
