//go:build verify && integration

package orch_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fault injection tests (Tier 3) ---

// TestFault_SessionTimeout verifies the engine completes without hanging
// or panicking when a spawn function blocks indefinitely and the context
// times out.
func TestFault_SessionTimeout(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/timeout", "builder")
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "fault-timeout"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{
		Interval:    50 * time.Millisecond,
		CBThreshold: 10,
		CBWindow:    time.Minute,
	}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	var spawnsStarted, spawnsDone atomic.Int64
	e.SetTestSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		spawnsStarted.Add(1)
		defer spawnsDone.Add(1)
		<-ctx.Done()
	})

	prepopulateLedger(t, runDir, testTask("hang-t", "feat/timeout", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = e.Run(ctx)
	if err != nil {
		assert.ErrorIs(t, err, context.DeadlineExceeded,
			"expected nil or deadline exceeded, got %v", err)
	}

	assert.GreaterOrEqual(t, spawnsStarted.Load(), int64(1),
		"at least one spawn must have been invoked to exercise the timeout path")
	assert.Equal(t, spawnsStarted.Load(), spawnsDone.Load(),
		"every spawn goroutine must exit before Engine.Run returns")
}

// TestFault_CircuitBreakerTrip exercises rapid consecutive failure handling
// and verifies gap recording occurs under rapid consecutive failures.
func TestFault_CircuitBreakerTrip(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/cb", "builder")
	lifecycle.GapTolerance = 10 // high so abort doesn't fire first
	lifecycle.MaxRetries = 0
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "fault-cb"
	cfg.MaxWorkers = 1
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	e.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(1)) // all fail immediately

	// Multiple tasks that all fail rapidly.
	prepopulateLedger(t, runDir,
		testTask("cb-1", "feat/cb", "builder"),
		testTask("cb-2", "feat/cb", "builder"),
		testTask("cb-3", "feat/cb", "builder"),
		testTask("cb-4", "feat/cb", "builder"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// May abort via gap tolerance or complete — either is acceptable.
	err = e.Run(ctx)
	if err != nil {
		assert.True(t, errors.Is(err, orch.ErrLifecycleAborted) || errors.Is(err, context.DeadlineExceeded),
			"expected nil, lifecycle aborted, or deadline exceeded, got %v", err)
	}

	// Require task_failed so the accept-either error policy above can't pass
	// vacuously when no spawns ran.
	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventGapRecorded, orch.EventTaskFailed)
}

// TestFault_ConcurrentLockContention verifies BVV-S-01 + BVV-ERR-06:
// two engines racing for the same branch lock — exactly one succeeds.
func TestFault_ConcurrentLockContention(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lockPath := filepath.Join(runDir, ".wonka-feat-lock.lock")

	mkLifecycle := func() *orch.LifecycleConfig {
		lc := testutil.MockLifecycleConfig("feat/lock", "builder")
		lc.Lock.Path = lockPath
		lc.Lock.StalenessThreshold = 1 * time.Hour
		lc.Lock.RetryCount = 0
		return lc
	}

	// Engine 1.
	cfg1 := orch.DefaultEngineConfig(mkLifecycle(), runDir, t.TempDir())
	cfg1.RunID = "lock-1"
	cfg1.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e1, err := orch.NewEngine(cfg1)
	require.NoError(t, err)
	e1.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	prepopulateLedger(t, runDir, testTask("lock-t", "feat/lock", "builder"))

	// Engine 2 with same lock path but different RunDir (to avoid shared ledger).
	runDir2 := t.TempDir()
	cfg2 := orch.DefaultEngineConfig(mkLifecycle(), runDir2, t.TempDir())
	cfg2.RunID = "lock-2"
	cfg2.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e2, err := orch.NewEngine(cfg2)
	require.NoError(t, err)
	e2.SetTestSpawnFunc(testutil.ImmediateSpawnFunc(0))

	prepopulateLedger(t, runDir2, testTask("lock-t", "feat/lock", "builder"))

	// Race both engines. Receiving both values from the buffered channel is
	// itself the completion barrier — no separate counter needed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := make(chan error, 2)
	go func() { results <- e1.Run(ctx) }()
	go func() { results <- e2.Run(ctx) }()

	err1 := <-results
	err2 := <-results

	// Exactly one should succeed, one should get ErrLockContention.
	isLock1 := err1 != nil && errors.Is(err1, orch.ErrLockContention)
	isLock2 := err2 != nil && errors.Is(err2, orch.ErrLockContention)
	ok1 := err1 == nil
	ok2 := err2 == nil

	assert.True(t, (ok1 && isLock2) || (isLock1 && ok2),
		"one engine should succeed and one should get lock contention (err1=%v, err2=%v)", err1, err2)
}

// TestFault_KillTmuxSession verifies the engine does not hang or panic when
// a spawn function blocks indefinitely on the first invocation and subsequent
// invocations succeed.
func TestFault_KillTmuxSession(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/kill", "builder")
	lifecycle.MaxHandoffs = 3
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "fault-kill"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{
		Interval:    50 * time.Millisecond,
		CBThreshold: 10,
		CBWindow:    time.Minute,
	}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	// First spawn hangs; subsequent spawns exit 0 (recovery succeeds).
	var spawnCount atomic.Int64
	e.SetTestSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		if spawnCount.Add(1) == 1 {
			<-ctx.Done()
			return
		}
		outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(0), 0, roleCfg)
	})

	prepopulateLedger(t, runDir, testTask("kill-t", "feat/kill", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// May timeout — the test verifies the engine doesn't hang or panic.
	err = e.Run(ctx)
	if err != nil {
		assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, orch.ErrLifecycleAborted),
			"expected nil, deadline exceeded, or lifecycle aborted, got %v", err)
	}

	// Require at least one spawn so the accept-either error policy can't pass
	// vacuously without exercising the hang-recovery path.
	assert.GreaterOrEqual(t, spawnCount.Load(), int64(1),
		"at least one spawn must have been invoked to exercise the hang path")
}

// TestFault_StoreFailureDuringDispatch verifies graceful degradation when
// the store fails mid-dispatch. Uses FailingStore at the Dispatcher level
// since Engine constructs its own store internally.
func TestFault_StoreFailureDuringDispatch(t *testing.T) {
	// No tmux needed — this operates at the Dispatcher level.
	inner := testutil.NewMockStore()
	branch := "feat/storefail"

	// Create tasks before wrapping with FailingStore.
	for i := 0; i < 3; i++ {
		require.NoError(t, inner.CreateTask(&orch.Task{
			ID:       fmt.Sprintf("sf-%d", i),
			Status:   orch.StatusOpen,
			Priority: 0,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        "builder",
				orch.LabelCriticality: string(orch.NonCritical),
			},
		}))
	}

	// Budget chosen empirically — the load-bearing invariant is that the
	// store failure surfaces (sawError below), not the exact tick it fires.
	const storeSuccessBudget = 5
	failing := testutil.NewFailingStore(inner, storeSuccessBudget)

	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	pool := orch.NewWorkerPool(failing, nil, 2, "fault-run", "/repo", t.TempDir())
	d, err := orch.NewDispatcher(
		failing, pool, nil, nil, nil,
		orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// Drain any in-flight spawns on failure paths — idempotent with the
	// per-tick d.Wait() inside the loop below.
	t.Cleanup(func() { d.Wait() })

	ctx := context.Background()
	// Run ticks until the store failure surfaces. The dispatcher must not panic
	// and must eventually report an error.
	var sawError bool
	for tick := 0; tick < 20; tick++ {
		r := d.Tick(ctx)
		d.Wait()
		if r.Error != nil {
			sawError = true
			assert.ErrorIs(t, r.Error, testutil.ErrInjectedFailure,
				"store failure should surface as injected failure")
			break
		}
		if r.LifecycleDone || r.GapAbort {
			break
		}
	}
	assert.True(t, sawError, "dispatcher should have encountered store failure within 20 ticks")
}

// TestFault_FailureThenRetry exercises BVV-ERR-01 at the Engine level: a task
// that exits 1 is retried and the retry succeeds. This is the end-to-end
// companion to the unit-level TestBVV_ERR01_RetryBehavior.
//
// Note: BVV-DSP-13 (worktree merge-back failure semantics) is NOT exercised
// here — the test uses a generic spawn sequence rather than driving the
// actual worktree/rebase code path. BVV-DSP-13 is tracked as an open
// coverage gap until worktree merge-back is implemented in the dispatcher.
func TestFault_FailureThenRetry(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	lifecycle := testutil.MockLifecycleConfig("feat/retry-e2e", "builder")
	lifecycle.MaxRetries = 1
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "fault-retry"
	cfg.Dispatch = orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 10 * time.Millisecond}

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)
	// First call exits 1 (retryable failure); second call exits 0 (retry succeeds).
	e.SetTestSpawnFunc(testutil.SequenceSpawnFunc([]int{1, 0}))

	prepopulateLedger(t, runDir, testTask("retry-t", "feat/retry-e2e", "builder"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = e.Run(ctx)
	assert.NoError(t, err, "retry after failure should succeed")

	logPath := filepath.Join(runDir, "events.jsonl")
	assertEventKinds(t, logPath, orch.EventTaskRetried, orch.EventTaskCompleted)
}
