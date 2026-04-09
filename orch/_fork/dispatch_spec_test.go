package orch_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/endgame/facet-scan/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Dispatch Loop Tests (OPS-01, OPS-02, OPS-06) ---

// TestOPS01_DispatchAssignsReadyTasks verifies [OPS-01]: dispatch assigns ready tasks to idle workers.
func TestOPS01_DispatchAssignsReadyTasks(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-ops01")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-ops01", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	// Inject a spawn func that immediately completes tasks.
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		// Simulate successful completion.
		task.Status = orch.StatusCompleted
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	result := d.Tick(context.Background())

	assert.Positive(t, result.Dispatched, "should have dispatched at least one task")
	assert.NoError(t, result.Error)
}

// TestOPS02_PhaseProgressionViaDeps verifies [OPS-02]: phase progression via dependency resolution.
func TestOPS02_PhaseProgressionViaDeps(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-ops02")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-ops02", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	// Spawn func: immediately complete with valid output.
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		// Write valid output.
		outPath := filepath.Join(dir, task.Output)
		writeFile(t, outPath, "# Valid output\n"+largeBody)
		task.Status = orch.StatusCompleted
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Run ticks until pipeline is done or we hit a limit.
	var lastResult orch.DispatchResult
	for i := range 50 {
		lastResult = d.Tick(context.Background())
		if lastResult.PipelineDone || lastResult.Error != nil {
			break
		}
		_ = i
	}

	assert.True(t, lastResult.PipelineDone, "pipeline should complete through phase progression")
}

// TestOPS06_PhaseAdvancesImmediately verifies [OPS-06]: boundary=none → next phase proceeds immediately.
func TestOPS06_PhaseAdvancesImmediately(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-ops06")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-ops06", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		outPath := filepath.Join(dir, task.Output)
		writeFile(t, outPath, "# Valid output\n"+largeBody)
		task.Status = orch.StatusCompleted
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Run ticks and track phase advances.
	advanceCount := 0
	for range 50 {
		result := d.Tick(context.Background())
		if result.PhaseAdvanced {
			advanceCount++
		}
		if result.PipelineDone {
			break
		}
	}

	assert.Positive(t, advanceCount, "should have at least one phase advance")
}

// TestOrphanCk_CBTrippedFailsTask verifies [SUP-08] OrphanCk: CB tripped → fail orphaned task + return worker.
func TestOrphanCk_CBTrippedFailsTask(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	p := testutil.MiniPipeline()

	tmuxClient := newTestTmux(t, "test-orphan")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-orphan", dir, dir)

	// Create a tripped watchdog manually.
	cfg := orch.WatchdogConfig{Interval: time.Second, CBThreshold: 1, CBWindow: 60 * time.Second}
	wd := orch.NewWatchdog(pool, store, el, p, testPreset(), "", cfg, nil)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, wd, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	// Create an orphaned worker + task.
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "orphan-task",
		SessionStartedAt: time.Now().Add(-5 * time.Second),
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "orphan-task", ParentID: p.ID + ":" + p.Phases[0].ID,
		Type: orch.TypeAgent, Status: orch.StatusInProgress, AgentID: "test-agent",
		Assignee: "w-01",
	}))

	// Trip the CB manually.
	cb := orch.NewCircuitBreaker(1, 60*time.Second)
	cb.RecordFailure("w-01", time.Now().Add(-5*time.Second))

	// Trigger trip through watchdog's RecordFailure path by calling CheckOnce
	// (worker is active, session dead, recent start).
	_ = wd.CheckOnce()
	require.True(t, wd.CBTripped(), "CB should be tripped")

	// Run OrphanCk via Tick.
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {})
	result := d.Tick(context.Background())

	assert.Equal(t, 1, result.OrphansFailed, "should fail one orphan")

	// Verify task is now failed.
	task, err := store.GetTask("orphan-task")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, task.Status)

	// Verify CB is reset.
	assert.False(t, wd.CBTripped(), "CB should be reset after OrphanCk")
}

// TestERR08_GapAbortViaDispatch verifies [ERR-08]: gap tolerance reached via dispatch check.
func TestERR08_GapAbortViaDispatch(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	p.GapTolerance = 1 // abort on first gap
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-gap")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-gap", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)
	defer d.Wait() // ensure agent goroutines complete before TempDir cleanup

	// Spawn func that fails all tasks.
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		task.Status = orch.StatusFailed
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Run ticks until gap abort or limit.
	var lastResult orch.DispatchResult
	for range 50 {
		lastResult = d.Tick(context.Background())
		if lastResult.GapAbort || lastResult.Error != nil {
			break
		}
	}

	// Should abort OR error (retries exhausted if critical).
	assert.True(t, lastResult.GapAbort || lastResult.Error != nil,
		"pipeline should abort on gap tolerance or critical retry exhaustion")
}

// TestS1_PhaseOrderingEnforced verifies [S1]: agents from phase N+1 are NOT dispatched
// while phase N agents are still running. The dispatch loop must only auto-complete the
// current phase task, not future phases.
func TestS1_PhaseOrderingEnforced(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-s1")
	pool := orch.NewWorkerPool(store, tmuxClient, 8, "test-s1", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	// Track which agents get dispatched (thread-safe).
	var mu sync.Mutex
	var dispatched []string
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		mu.Lock()
		dispatched = append(dispatched, task.AgentID)
		mu.Unlock()
		// Do NOT complete the task — leave it in_progress.
	})

	// Run one tick. Only phase 0 agents should be dispatched.
	d.Tick(context.Background())

	// Give goroutines a moment to register.
	time.Sleep(50 * time.Millisecond)

	// Phase 0 has agents a1 and a2. Phase 1 has agent a3.
	mu.Lock()
	snapshot := append([]string{}, dispatched...)
	mu.Unlock()
	for _, agentID := range snapshot {
		assert.NotEqual(t, "a3", agentID,
			"phase 1 agent a3 must NOT be dispatched while phase 0 is running")
	}
	assert.NotEmpty(t, snapshot, "should dispatch at least one agent from phase 0")
}

// failFirstAttemptSpawnFunc returns a SpawnFunc that fails "critical-agent" on
// its first dispatch attempt and succeeds on all subsequent attempts. The
// returned getter provides a thread-safe copy of the per-agent attempt counts.
func failFirstAttemptSpawnFunc(
	t *testing.T, store orch.Store, pool *orch.WorkerPool, dir string,
) (func(context.Context, *orch.Task, *orch.Worker, orch.AgentDef), func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	attempts := map[string]int{}
	fn := func(_ context.Context, task *orch.Task, worker *orch.Worker, _ orch.AgentDef) {
		mu.Lock()
		attempts[task.AgentID]++
		attempt := attempts[task.AgentID]
		mu.Unlock()

		if task.AgentID == "critical-agent" && attempt == 1 {
			task.Status = orch.StatusFailed
		} else {
			writeFile(t, filepath.Join(dir, task.Output), "# Valid output\n"+largeBody)
			task.Status = orch.StatusCompleted
		}
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	}
	getAttempts := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		cp := make(map[string]int, len(attempts))
		for k, v := range attempts {
			cp[k] = v
		}
		return cp
	}
	return fn, getAttempts
}

// tickUntilDone runs dispatch ticks until the pipeline completes or errors (max 50 ticks).
func tickUntilDone(d *orch.Dispatcher) orch.DispatchResult {
	var result orch.DispatchResult
	for range 50 {
		result = d.Tick(context.Background())
		if result.PipelineDone || result.Error != nil {
			break
		}
	}
	return result
}

// TestERR01_CriticalAgentRetryCreated verifies [ERR-01]: critical agent failure with MaxRetries > 0
// creates a retry task with correct ID format, copies deps, emits RetryScheduled event.
func TestERR01_CriticalAgentRetryCreated(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.RetryPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-retry")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-retry", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	spawnFn, getAttempts := failFirstAttemptSpawnFunc(t, store, pool, dir)
	d.SetSpawnFunc(spawnFn)

	lastResult := tickUntilDone(d)

	// Pipeline should complete (retry succeeded on attempt 2).
	require.NoError(t, lastResult.Error)
	assert.True(t, lastResult.PipelineDone, "pipeline should complete after retry succeeds")

	// Verify retry task was created with correct ID format.
	retryTask, err := store.GetTask(orch.RetryTaskID("critical-agent", 1))
	require.NoError(t, err, "retry task should exist in store")
	assert.Equal(t, orch.StatusCompleted, retryTask.Status)
	assert.Equal(t, "critical-agent", retryTask.AgentID)

	assert.Equal(t, 2, getAttempts()["critical-agent"],
		"critical agent should be dispatched twice (original + retry)")

	// Verify RetryScheduled event was emitted.
	testutil.ValidateEventSequence(t, filepath.Join(dir, "events.jsonl"), []orch.EventKind{
		orch.EventRetryScheduled,
		orch.EventPipelineComplete,
	})
}

// TestERR01_RetryTaskAlreadyExists verifies that createRetryTask handles the
// ErrTaskExists case (idempotent retry on resume). When a retry task from a
// prior run already exists in the store, createRetryTask resets it to open
// instead of failing with a duplicate error.
func TestERR01_RetryTaskAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.RetryPipeline()
	p.ID = "test-retry-exists"
	require.NoError(t, orch.Expand(p, store))

	// Pre-create a failed retry task in the store — simulates a prior run that
	// created a retry, which then also failed before the pipeline was interrupted.
	retryID := orch.RetryTaskID("critical-agent", 1)
	phase1ID := p.ID + ":" + p.Phases[0].ID
	require.NoError(t, store.CreateTask(&orch.Task{
		ID:       retryID,
		ParentID: phase1ID,
		Type:     orch.TypeAgent,
		Status:   orch.StatusFailed,
		AgentID:  "critical-agent",
		Output:   "c1.md",
	}))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-retry-exists")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-retry-exists", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	spawnFn, getAttempts := failFirstAttemptSpawnFunc(t, store, pool, dir)
	d.SetSpawnFunc(spawnFn)

	lastResult := tickUntilDone(d)

	require.NoError(t, lastResult.Error)
	assert.True(t, lastResult.PipelineDone, "pipeline should complete after retry reuses existing task")

	// Verify the pre-existing retry task was reset and completed (not a new duplicate).
	retryTask, err := store.GetTask(retryID)
	require.NoError(t, err, "retry task should exist in store")
	assert.Equal(t, orch.StatusCompleted, retryTask.Status,
		"existing retry task should be reset to open then completed on re-dispatch")
	assert.Equal(t, "critical-agent", retryTask.AgentID)

	assert.Equal(t, 2, getAttempts()["critical-agent"],
		"critical agent dispatched twice (original + reused retry)")
}

// TestERR01_RetryTaskAlreadyCompleted verifies that createRetryTask does NOT
// reset a completed retry task. When a prior retry already succeeded, the
// dispatcher skips re-dispatch and lets the phase advance.
func TestERR01_RetryTaskAlreadyCompleted(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.RetryPipeline()
	p.ID = "test-retry-completed"
	require.NoError(t, orch.Expand(p, store))

	// Pre-create a completed retry task with valid output — simulates a prior run
	// where the retry succeeded. createRetryTask must not reset it.
	retryID := orch.RetryTaskID("critical-agent", 1)
	phase1ID := p.ID + ":" + p.Phases[0].ID
	require.NoError(t, store.CreateTask(&orch.Task{
		ID:       retryID,
		ParentID: phase1ID,
		Type:     orch.TypeAgent,
		Status:   orch.StatusCompleted,
		AgentID:  "critical-agent",
		Output:   "c1.md",
	}))
	// Write valid output so the retry task's output validates.
	writeFile(t, filepath.Join(dir, "c1.md"), "# Valid output\n"+largeBody)

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-retry-completed")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-retry-completed", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	spawnFn, getAttempts := failFirstAttemptSpawnFunc(t, store, pool, dir)
	d.SetSpawnFunc(spawnFn)

	lastResult := tickUntilDone(d)

	require.NoError(t, lastResult.Error)
	assert.True(t, lastResult.PipelineDone, "pipeline should complete — retry already succeeded")

	// Verify the completed retry task was NOT reset to open.
	retryTask, err := store.GetTask(retryID)
	require.NoError(t, err)
	assert.Equal(t, orch.StatusCompleted, retryTask.Status,
		"completed retry task must not be reset to open")

	assert.Equal(t, 1, getAttempts()["critical-agent"],
		"critical agent dispatched only once — retry was already completed")

	// Verify the retry budget is NOT consumed when a completed retry is found.
	// RecordAttempt should only fire when a retry is actually dispatched.
	assert.Equal(t, 0, retries.AttemptCount("critical-agent"),
		"completed prior retry must not consume the retry budget")
}

// TestRCV07_DispatchWithFailingStore verifies [RCV-07]: store failure propagates
// through the dispatch loop rather than silently continuing.
func TestRCV07_DispatchWithFailingStore(t *testing.T) {
	dir := t.TempDir()
	realStore := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, realStore))

	// Wrap with FailingStore that fails after 5 operations (enough for setup).
	failStore := testutil.NewFailingStore(realStore, 5)

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-rcv07")
	pool := orch.NewWorkerPool(failStore, tmuxClient, 4, "test-rcv07", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		failStore, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		task.Status = orch.StatusCompleted
		task.UpdatedAt = time.Now()
		_ = failStore.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Run ticks — store should fail and dispatch should surface the error.
	var gotError bool
	for range 50 {
		result := d.Tick(context.Background())
		if result.Error != nil {
			gotError = true
			// Verify error wraps ErrLedgerUnavailable.
			require.ErrorIs(t, result.Error, orch.ErrLedgerUnavailable,
				"dispatch error should wrap ErrLedgerUnavailable")
			break
		}
		if result.PipelineDone {
			break
		}
	}

	assert.True(t, gotError, "dispatch should surface store failure as an error")
}

// TestLDG20_ChildFailureDoesNotAutoFailParent verifies [LDG-20]: when a child task fails,
// the parent is NOT automatically failed while siblings are still running.
func TestLDG20_ChildFailureDoesNotAutoFailParent(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.CreateTask(&orch.Task{ID: "parent", Type: orch.TypePhase, Status: orch.StatusInProgress}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "child-a", Type: orch.TypeAgent, Status: orch.StatusFailed, ParentID: "parent"}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "child-b", Type: orch.TypeAgent, Status: orch.StatusInProgress, ParentID: "parent"}))

	// child-a failed but child-b is still running. Parent must NOT be failed yet (LDG-19/LDG-20).
	status, err := orch.DeriveParentStatus(store, "parent")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusInProgress, status,
		"parent must not be auto-failed while sibling child-b is non-terminal")

	// Now complete child-b. Parent should derive to failed (LDG-18).
	child, err := store.GetTask("child-b")
	require.NoError(t, err)
	child.Status = orch.StatusCompleted
	require.NoError(t, store.UpdateTask(child))

	status, err = orch.DeriveParentStatus(store, "parent")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, status,
		"parent should be failed once all children are terminal and one failed")
}

// TestERR04_NonCriticalExhaustsRetriesToGap verifies [ERR-04]: non-critical agent exhausts
// retries → gap recorded (not pipeline termination).
func TestERR04_NonCriticalExhaustsRetriesToGap(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := &orch.Pipeline{
		ID:           "test-err04",
		OutputDir:    "out",
		GapTolerance: 2, // WFC-10: must be in [1, non_critical_count]
		Lock:         testutil.DefaultTestLock(),
		Phases: []orch.Phase{
			{
				ID: "p1", Topology: orch.Parallel,
				Agents: []orch.AgentDef{
					{ID: "non-crit", Model: orch.ModelSonnet, Output: "nc.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "filler", Model: orch.ModelSonnet, Output: "f.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				},
			},
		},
	}
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-err04")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-err04", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)
	defer d.Wait()

	// Fail non-crit, complete filler.
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		if task.AgentID == "non-crit" {
			task.Status = orch.StatusFailed
		} else {
			task.Status = orch.StatusCompleted
		}
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Run ticks until done.
	for range 50 {
		result := d.Tick(context.Background())
		if result.PipelineDone || result.GapAbort || result.Error != nil {
			break
		}
	}

	// Gap should have been recorded, but pipeline should NOT have aborted (tolerance=2, only 1 gap).
	assert.Equal(t, 1, gaps.Count(), "one gap should be recorded for non-critical agent")
}

// --- Progress Reporter Tests ---

// recordingReporter captures events for assertions.
type recordingReporter struct {
	mu     sync.Mutex
	events []orch.Event
}

func (r *recordingReporter) OnEvent(ev orch.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingReporter) kinds() []orch.EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]orch.EventKind, len(r.events))
	for i, ev := range r.events {
		kinds[i] = ev.Kind
	}
	return kinds
}

// TestProgressReporter_ReceivesEvents verifies that a non-nil ProgressReporter
// receives events from the dispatch loop.
func TestProgressReporter_ReceivesEvents(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-progress")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-progress", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}

	rec := &recordingReporter{}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		rec,
	)
	defer d.Wait()

	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		task.Status = orch.StatusCompleted
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Run ticks until done.
	for range 50 {
		result := d.Tick(context.Background())
		if result.PipelineDone || result.Error != nil {
			break
		}
	}

	// Reporter should receive events from the dispatch loop path.
	// Note: EventAgentComplete fires in runAgent (production SpawnFunc), not the mock.
	kinds := rec.kinds()
	assert.Contains(t, kinds, orch.EventAgentStart, "reporter should receive agent_start")
	assert.Contains(t, kinds, orch.EventPhaseComplete, "reporter should receive phase_complete")
	assert.Contains(t, kinds, orch.EventPipelineComplete, "reporter should receive pipeline_complete")
	assert.NotEmpty(t, kinds, "reporter should receive at least some events")
}

// TestProgressReporter_NilSafe verifies that a nil ProgressReporter does not panic.
func TestProgressReporter_NilSafe(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-nilprogress")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-nilprogress", dir, dir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()

	// nil progress — must not panic.
	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries,
		orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second},
		p, testPreset(), "", dir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)
	defer d.Wait()

	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, agentDef orch.AgentDef) {
		task.Status = orch.StatusCompleted
		task.UpdatedAt = time.Now()
		_ = store.UpdateTask(task)
		_ = pool.Release(worker.Name)
	})

	// Should not panic.
	result := d.Tick(context.Background())
	assert.NoError(t, result.Error)
}

// TestDispatchFailure_EmitsEventWithReason verifies that when an agent fails to
// spawn during dispatch (e.g., ValidateInputs fails, WriteCrashMarker fails,
// SpawnSession fails), the orchestrator emits an EventCrashDetected event with
// the failure reason in the summary. Without this, dispatch failures would be
// silently swallowed and the root cause lost — as happened with the quality
// agent failure on bynder-lucee.
func TestDispatchFailure_EmitsEventWithReason(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outputDir, 0o755))

	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	// Sequential phase with producer → consumer. WFC requires the consumer's
	// input to be produced by a prior agent, so we declare that dependency.
	// We'll mark the producer task as completed WITHOUT writing the file, so
	// when the consumer runs, ValidateInputs fails and we can verify the event.
	p := &orch.Pipeline{
		ID:           "test-dispatch-fail",
		OutputDir:    outputDir,
		GapTolerance: 1,
		Lock: orch.LockConfig{
			Path:               filepath.Join(outputDir, ".pipeline.lock"),
			StalenessThreshold: time.Hour,
			RetryCount:         1,
			RetryDelay:         10 * time.Millisecond,
		},
		Phases: []orch.Phase{
			{
				ID: "p1", Topology: orch.Sequential,
				Agents: []orch.AgentDef{
					{
						ID:          "producer",
						Model:       orch.ModelSonnet,
						Output:      "producer.md",
						Criticality: orch.NonCritical,
						Format:      orch.FormatMd,
					},
					{
						ID:          "consumer",
						Model:       orch.ModelSonnet,
						Inputs:      []string{"producer.md"},
						Output:      "consumer.md",
						Criticality: orch.NonCritical,
						Format:      orch.FormatMd,
					},
				},
			},
		},
	}
	require.NoError(t, orch.Expand(p, store))

	// Simulate the producer having been "completed" in the ledger but without
	// actually creating producer.md on disk. This is exactly the scenario
	// where ValidateInputs catches missing output files.
	producerTask, err := store.GetTask("producer")
	require.NoError(t, err)
	producerTask.Status = orch.StatusCompleted
	producerTask.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateTask(producerTask))

	eventLogPath := filepath.Join(dir, "events.jsonl")
	el, err := orch.NewEventLog(eventLogPath)
	require.NoError(t, err)
	defer el.Close()

	tmuxClient := newTestTmux(t, "test-dispatch-fail")
	pool := orch.NewWorkerPool(store, tmuxClient, 4, "test-dispatch-fail", dir, outputDir)

	gaps := orch.NewGapTracker(p.GapTolerance)
	retries := orch.NewRetryState()
	retryCfg := orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}

	d := orch.NewDispatcher(
		store, pool, nil, el, nil, gaps, retries, retryCfg,
		p, testPreset(), "", outputDir,
		orch.DefaultDispatchConfig(), 0, p.ID,
		nil,
	)

	// Tick until consumer is dispatched (producer is pre-completed).
	for range 5 {
		result := d.Tick(context.Background())
		require.NoError(t, result.Error)
		task, _ := store.GetTask("consumer")
		if task != nil && task.Status == orch.StatusFailed {
			break
		}
	}

	// Verify consumer task is failed (ValidateInputs caught missing producer.md).
	task, err := store.GetTask("consumer")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, task.Status, "consumer should be marked failed after ValidateInputs fails")

	// Read the event log and find the dispatch-failure event.
	require.NoError(t, el.Close())
	events := readEventLog(t, eventLogPath)

	var crashEvent *orch.Event
	for i := range events {
		if events[i].Kind == orch.EventCrashDetected && events[i].TaskID == "consumer" {
			crashEvent = &events[i]
			break
		}
	}
	require.NotNil(t, crashEvent, "expected EventCrashDetected for consumer; got events: %+v", events)
	assert.Contains(t, crashEvent.Summary, "dispatch failed:", "summary should start with 'dispatch failed:'")
	assert.Contains(t, crashEvent.Summary, "validate inputs:", "summary should identify the failing step")
	assert.Contains(t, crashEvent.Summary, "producer.md", "summary should include the missing input name")
	assert.Equal(t, "p1", crashEvent.Phase, "event should include phase ID")
	assert.Equal(t, "consumer", crashEvent.Agent, "event should include agent ID")
	assert.NotEmpty(t, crashEvent.Worker, "event should include worker name")
}

// readEventLog reads a JSONL event log file and returns all events.
func readEventLog(t *testing.T, path string) []orch.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var events []orch.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e orch.Event
		require.NoError(t, json.Unmarshal([]byte(line), &e), "failed to parse line: %s", line)
		events = append(events, e)
	}
	return events
}

// largeBody provides enough bytes for md validation (>100 bytes).
var largeBody = "This is a sufficiently long body to pass the 100-byte minimum size check for markdown format validation in the orchestrator output validator.\n"
