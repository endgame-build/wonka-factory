//go:build verify

package orch_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDispatcher creates a Dispatcher with MockStore and configurable SpawnFunc.
// Returns the dispatcher, store, and lifecycle config for test inspection.
func newTestDispatcher(t *testing.T, branch string, maxWorkers int, roles ...string) (*orch.Dispatcher, *testutil.MockStore, *orch.LifecycleConfig) {
	t.Helper()
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig(branch, roles...)

	pool := orch.NewWorkerPool(store, nil, maxWorkers, "test-run", "/tmp/repo", t.TempDir())

	retries := orch.NewRetryState()
	gaps := orch.NewGapTracker(lifecycle.GapTolerance)
	handoffs := orch.NewHandoffState(lifecycle.MaxHandoffs)

	d := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		gaps, retries, handoffs,
		orch.RetryConfig{MaxRetries: lifecycle.MaxRetries, BaseTimeout: 30 * time.Minute},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil, // no progress reporter
	)
	return d, store, lifecycle
}

// TestBVV_DSP01_DispatchAllReady verifies that the dispatcher dispatches all
// ready tasks up to available workers (BVV-DSP-01).
func TestBVV_DSP01_DispatchAllReady(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	result := d.Tick(context.Background())
	assert.Equal(t, 3, result.Dispatched, "all 3 ready tasks should be dispatched")
}

// TestBVV_DSP02_NoHoldingReady verifies that ready tasks are dispatched
// immediately, not held waiting for other tasks (BVV-DSP-02).
func TestBVV_DSP02_NoHoldingReady(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// First tick dispatches all 3.
	r1 := d.Tick(context.Background())
	assert.Equal(t, 3, r1.Dispatched)

	// Wait for goroutines to send outcomes.
	d.Wait()

	// Second tick processes completions and terminates.
	r2 := d.Tick(context.Background())
	assert.True(t, r2.LifecycleDone, "all tasks completed → lifecycle done")
}

// TestBVV_DSP03_RoleBasedRouting verifies that tasks are routed to the
// correct RoleConfig based on their role label (BVV-DSP-03).
func TestBVV_DSP03_RoleBasedRouting(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder", "verifier")

	// Create tasks with different roles.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "build-1", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/x", "role": "builder", "criticality": "non_critical"},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "verify-1", Status: orch.StatusOpen, Priority: 1,
		Labels: map[string]string{"branch": "feat/x", "role": "verifier", "criticality": "non_critical"},
	}))

	var mu sync.Mutex
	var dispatched []string
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		dispatched = append(dispatched, task.ID+":"+task.Role())
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	d.Tick(context.Background())
	d.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, dispatched, 2)
	assert.Contains(t, dispatched, "build-1:builder")
	assert.Contains(t, dispatched, "verify-1:verifier")
}

// TestBVV_DSP03a_UnknownRoleEscalation verifies that a task with an unknown
// role creates an escalation task and blocks the original (BVV-DSP-03a).
func TestBVV_DSP03a_UnknownRoleEscalation(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")

	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "mystery", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/x", "role": "unknown_role", "criticality": "non_critical"},
	}))
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	d.Tick(context.Background())

	// Original task should be blocked.
	task, err := store.GetTask("mystery")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusBlocked, task.Status, "unknown role → task blocked")

	// Escalation task should exist.
	esc, err := store.GetTask("escalation-mystery")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, esc.Status)
	assert.Equal(t, "escalation", esc.Labels["role"])
	assert.Contains(t, esc.Title, "unknown_role")
}

// TestBVV_DSP05_OneTaskPerSession verifies that each task gets exactly one
// SpawnSession call (BVV-DSP-05).
func TestBVV_DSP05_OneTaskPerSession(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)

	var spawnCount atomic.Int32
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		spawnCount.Add(1)
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	d.Tick(context.Background())
	d.Wait()
	assert.Equal(t, int32(3), spawnCount.Load(), "each task should be spawned exactly once")
}

// TestBVV_DSP08_LifecycleScoping verifies that the dispatcher only dispatches
// tasks matching the lifecycle's branch label (BVV-DSP-08).
func TestBVV_DSP08_LifecycleScoping(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")

	// Create tasks on two different branches.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "on-branch", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/x", "role": "builder", "criticality": "non_critical"},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "off-branch", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/y", "role": "builder", "criticality": "non_critical"},
	}))

	var mu sync.Mutex
	var dispatched []string
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		dispatched = append(dispatched, task.ID)
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	d.Tick(context.Background())
	d.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"on-branch"}, dispatched, "only branch:feat/x tasks dispatched")
}

// TestBVV_DSP09_OrchestratorAuthority verifies that only the dispatcher
// changes task status, not the SpawnFunc (BVV-DSP-09).
func TestBVV_DSP09_OrchestratorAuthority(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 1)

	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		// Agent does NOT modify task status — it just exits.
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	// Tick 1: dispatch.
	d.Tick(context.Background())
	task, _ := store.GetTask("p-0")
	assert.Equal(t, orch.StatusInProgress, task.Status, "after dispatch: in_progress (set by test mode)")

	// Wait for goroutine, then tick 2: process outcome → completed.
	d.Wait()
	d.Tick(context.Background())
	task, _ = store.GetTask("p-0")
	assert.Equal(t, orch.StatusCompleted, task.Status, "after outcome: completed (set by dispatcher)")
}

// TestBVV_DSP14_HandoffNoStatusChange verifies that exit code 3 (handoff)
// does not change the task's ledger status — it stays in_progress and the
// session is restarted (BVV-DSP-14). In test mode, RestartSession is skipped
// and the SpawnFunc is re-launched directly.
func TestBVV_DSP14_HandoffNoStatusChange(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 1)

	var callCount atomic.Int32
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		n := callCount.Add(1)
		if n == 1 {
			outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeHandoff, 3, roleCfg)
		} else {
			outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
		}
	})

	// Tick 1: dispatch → agent exits 3 (handoff).
	d.Tick(context.Background())
	d.Wait()
	// Tick 2: process handoff → re-launch SpawnFunc → agent exits 0.
	d.Tick(context.Background())
	d.Wait()
	// Tick 3: process success outcome.
	d.Tick(context.Background())

	assert.GreaterOrEqual(t, callCount.Load(), int32(2), "handoff should trigger re-launch")

	task, _ := store.GetTask("p-0")
	assert.Equal(t, orch.StatusCompleted, task.Status,
		"after handoff + success, task should be completed")
}

// TestBVV_DSP15_OrchestratorOwnsAssignment verifies that the orchestrator
// owns task selection and assignment via Store.Assign (BVV-DSP-15).
func TestBVV_DSP15_OrchestratorOwnsAssignment(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 1)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	d.Tick(context.Background())

	// Task should have been assigned by the dispatcher.
	task, _ := store.GetTask("p-0")
	assert.NotEmpty(t, task.Assignee, "dispatcher must set assignee via Assign")
}

// TestBVV_ERR03_CriticalTaskAbort verifies that a critical task failure
// causes immediate lifecycle abort (BVV-ERR-03).
func TestBVV_ERR03_CriticalTaskAbort(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	pool := orch.NewWorkerPool(store, nil, 3, "test-run", "/repo", t.TempDir())

	// No retries — failures are immediately terminal.
	d := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)

	// Create one critical task that will fail.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "critical-1", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/x", "role": "builder", "criticality": "critical"},
	}))

	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1)) // task fails

	// Tick 1: dispatch critical task.
	d.Tick(context.Background())
	d.Wait()
	// Tick 2: process outcome. Critical task fails → abort.
	r := d.Tick(context.Background())

	// The lifecycle should be aborted.
	assert.True(t, r.GapAbort, "critical task failure should trigger abort")

	// Critical task should be failed.
	crit, _ := store.GetTask("critical-1")
	assert.Equal(t, orch.StatusFailed, crit.Status, "critical task should be failed")
}

// TestBVV_DSP12_ConcurrentLifecycles verifies that two dispatchers on
// different branches don't interfere with each other (BVV-DSP-12).
func TestBVV_DSP12_ConcurrentLifecycles(t *testing.T) {
	store := testutil.NewMockStore()
	cfg := orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond}

	// Create tasks on two branches.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "a1", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "branch-a", "role": "builder", "criticality": "non_critical"},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "b1", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "branch-b", "role": "builder", "criticality": "non_critical"},
	}))

	lcA := testutil.MockLifecycleConfig("branch-a", "builder")
	lcB := testutil.MockLifecycleConfig("branch-b", "builder")

	poolA := orch.NewWorkerPool(store, nil, 2, "run-a", "/repo", t.TempDir())
	poolB := orch.NewWorkerPool(store, nil, 2, "run-b", "/repo", t.TempDir())

	dA := orch.NewDispatcher(store, poolA, nil, nil, nil,
		orch.NewGapTracker(3), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.DefaultRetryConfig(), lcA, cfg, nil)
	dB := orch.NewDispatcher(store, poolB, nil, nil, nil,
		orch.NewGapTracker(3), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.DefaultRetryConfig(), lcB, cfg, nil)

	var mu sync.Mutex
	var aDispatched, bDispatched []string
	dA.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		aDispatched = append(aDispatched, task.ID)
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})
	dB.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		bDispatched = append(bDispatched, task.ID)
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	dA.Tick(context.Background())
	dA.Wait()
	dB.Tick(context.Background())
	dB.Wait()

	assert.Equal(t, []string{"a1"}, aDispatched, "dispatcher A should only see branch-a tasks")
	assert.Equal(t, []string{"b1"}, bDispatched, "dispatcher B should only see branch-b tasks")
}

// TestBVV_DSP04_ExitCodeOutcome verifies that each exit code maps to the
// correct task status after processing (BVV-DSP-04).
func TestBVV_DSP04_ExitCodeOutcome(t *testing.T) {
	cases := []struct {
		name       string
		exitCode   int
		wantStatus orch.TaskStatus
	}{
		{"exit0_completed", 0, orch.StatusCompleted},
		{"exit1_failed", 1, orch.StatusFailed},
		{"exit2_blocked", 2, orch.StatusBlocked},
		// exit3 (handoff) is tested separately — needs RestartSession
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := testutil.NewMockStore()
			lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
			pool := orch.NewWorkerPool(store, nil, 1, "test-run", "/repo", t.TempDir())
			// No retries so failures are immediately terminal.
			d := orch.NewDispatcher(
				store, pool, nil, nil, nil,
				orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
				orch.RetryConfig{MaxRetries: 0},
				lifecycle,
				orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
				nil,
			)
			require.NoError(t, store.CreateTask(&orch.Task{
				ID: "task-1", Status: orch.StatusOpen, Priority: 0,
				Labels: map[string]string{"branch": "feat/x", "role": "builder", "criticality": "non_critical"},
			}))
			d.SetSpawnFunc(testutil.ImmediateSpawnFunc(c.exitCode))

			// Tick 1: dispatch. Wait for goroutines. Tick 2: process outcome.
			d.Tick(context.Background())
			d.Wait()
			d.Tick(context.Background())

			task, err := store.GetTask("task-1")
			require.NoError(t, err)
			assert.Equal(t, c.wantStatus, task.Status, "exit code %d should produce status %s", c.exitCode, c.wantStatus)
		})
	}
}

// TestBVV_ERR02_EscalatingTimeout verifies that session timeout escalates
// with retry attempts (BVV-ERR-02): timeout(n) = base * (1.0 + 0.5 * n).
func TestBVV_ERR02_EscalatingTimeout(t *testing.T) {
	base := 100 * time.Millisecond

	// Attempt 0: base * 1.0 = 100ms
	t0 := orch.ScaledTimeout(base, 0)
	assert.Equal(t, 100*time.Millisecond, t0, "attempt 0: 1.0x base")

	// Attempt 1: base * 1.5 = 150ms
	t1 := orch.ScaledTimeout(base, 1)
	assert.Equal(t, 150*time.Millisecond, t1, "attempt 1: 1.5x base")

	// Attempt 2: base * 2.0 = 200ms
	t2 := orch.ScaledTimeout(base, 2)
	assert.Equal(t, 200*time.Millisecond, t2, "attempt 2: 2.0x base")

	// Attempt 3: base * 2.5 = 250ms
	t3 := orch.ScaledTimeout(base, 3)
	assert.Equal(t, 250*time.Millisecond, t3, "attempt 3: 2.5x base")

	// Monotonically increasing.
	assert.True(t, t1 > t0, "timeout must increase with attempts")
	assert.True(t, t2 > t1, "timeout must increase with attempts")
	assert.True(t, t3 > t2, "timeout must increase with attempts")
}

// TestBVV_ERR02a_BaseSessionTimeout verifies that a session exceeding the
// base timeout is treated as exit code 1 (retryable failure, BVV-ERR-02a).
// This tests the contract: runAgent detects timeout → outcome = OutcomeFailure.
// Since runAgent needs tmux, we verify the contract at the DetermineOutcome
// level and the timeout formula.
func TestBVV_ERR02a_BaseSessionTimeout(t *testing.T) {
	// Contract: when runAgent times out, it forces exitCode=1.
	// exitCode 1 → OutcomeFailure.
	assert.Equal(t, orch.OutcomeFailure, orch.DetermineOutcome(1),
		"timeout exit code 1 → OutcomeFailure")

	// Jitter is bounded: [0, d/4].
	base := 100 * time.Millisecond
	for i := 0; i < 100; i++ {
		jittered := orch.RetryJitter(base)
		assert.GreaterOrEqual(t, jittered, base, "jitter must not reduce timeout")
		assert.LessOrEqual(t, jittered, base+base/4, "jitter must be <= d + d/4")
	}
}

// TestBVV_DSP01_LinearDAGOrdering verifies that tasks in a linear chain are
// dispatched in dependency order: t-0 first, then t-1, etc.
func TestBVV_DSP01_LinearDAGOrdering(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.LinearGraph(t, store, "feat/x", "builder", 3)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// Run until lifecycle done, waiting for goroutines between ticks.
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		d.Tick(ctx)
		d.Wait()
		r := d.Tick(ctx)
		if r.LifecycleDone {
			break
		}
		d.Wait()
	}

	// All tasks should be completed.
	for i := 0; i < 3; i++ {
		task, _ := store.GetTask(fmt.Sprintf("t-%d", i))
		assert.Equal(t, orch.StatusCompleted, task.Status, "task t-%d should be completed", i)
	}
}

// TestBVV_ERR04_DispatchGapAbort verifies that reaching gap tolerance
// aborts the lifecycle (BVV-ERR-04).
func TestBVV_ERR04_DispatchGapAbort(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.GapTolerance = 2 // low tolerance for testing

	pool := orch.NewWorkerPool(store, nil, 3, "test-run", "/repo", t.TempDir())
	d := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(2), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0}, // no retries — failures go straight to gap
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)

	// Create 3 non-critical tasks that all fail.
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1))

	// Tick 1: dispatch all 3.
	d.Tick(context.Background())
	d.Wait()
	// Tick 2: process failures → gap count reaches tolerance → abort.
	r := d.Tick(context.Background())

	assert.True(t, r.GapAbort, "gap tolerance reached → lifecycle aborted")
}
