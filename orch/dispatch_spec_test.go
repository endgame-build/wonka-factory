//go:build verify

package orch_test

import (
	"context"
	"fmt"
	"path/filepath"
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

	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		gaps, retries, handoffs,
		orch.RetryConfig{MaxRetries: lifecycle.MaxRetries, BaseTimeout: 30 * time.Minute},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil, // no progress reporter
	)
	require.NoError(t, err)
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
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "verify-1", Status: orch.StatusOpen, Priority: 1,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "verifier", orch.LabelCriticality: string(orch.NonCritical)},
	}))

	var mu sync.Mutex
	var dispatched []string
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
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

// TestBVV_DSP03a_NoEscalationOfEscalation verifies that the dispatcher does
// not recursively escalate its own escalation tasks (BVV-DSP-03a regression).
//
// Without the role=="escalation" skip in dispatch(), an escalation task created
// on tick N becomes "ready" on tick N+1, gets routed through the role map,
// misses (no "escalation" role is ever configured), and produces a second-
// generation escalation-escalation-<orig> task — blocking the previous one.
// Repeats every tick, flooding the ledger. Escalation tasks are human-facing
// and must remain `open` until manually resolved.
func TestBVV_DSP03a_NoEscalationOfEscalation(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")

	// Seed an escalation task directly — simulates state after a prior
	// unknown-role escalation, but without running the first tick.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "escalation-orig", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/x", "role": "escalation", "criticality": "critical"},
	}))
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// Run several ticks — pre-fix, each would spawn escalation-escalation-*.
	for i := 0; i < 3; i++ {
		d.Tick(context.Background())
	}

	// The seeded escalation must remain untouched: open, un-assigned.
	esc, err := store.GetTask("escalation-orig")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, esc.Status, "escalation task must not be mutated by dispatch")
	assert.Empty(t, esc.Assignee, "escalation task must not be assigned")

	// No recursive escalation should have been created.
	_, err = store.GetTask("escalation-escalation-orig")
	assert.Error(t, err, "dispatcher must not escalate its own escalation tasks")
}

// TestBVV_DSP05_OneTaskPerSession verifies that each task gets exactly one
// SpawnSession call (BVV-DSP-05).
func TestBVV_DSP05_OneTaskPerSession(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)

	var spawnCount atomic.Int32
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
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
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "off-branch", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{"branch": "feat/y", "role": "builder", "criticality": "non_critical"},
	}))

	var mu sync.Mutex
	var dispatched []string
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
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

	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
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

// TestBVV_DSP14_HandoffNoStatusChange verifies BVV-DSP-14: exit code 3
// (handoff) leaves the task's ledger status as in_progress. Asserts status
// between ticks so a transient flip isn't hidden by a final-status check.
func TestBVV_DSP14_HandoffNoStatusChange(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 1)

	var callCount atomic.Int32
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		n := callCount.Add(1)
		if n == 1 {
			outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeHandoff, 3, roleCfg)
		} else {
			outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
		}
	})

	ctx := context.Background()

	d.Tick(ctx)
	d.Wait()

	task, err := store.GetTask("p-0")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusInProgress, task.Status,
		"BVV-DSP-14: task status must remain in_progress across a handoff")
	assert.Equal(t, int32(1), callCount.Load(),
		"exactly one spawn observed before handoff re-launch")

	d.Tick(ctx)
	d.Wait()
	assert.GreaterOrEqual(t, callCount.Load(), int32(2),
		"handoff should trigger re-launch of the SpawnFunc")

	d.Tick(ctx)
	task, err = store.GetTask("p-0")
	require.NoError(t, err)
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
	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	// Create one critical task that will fail.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "critical-1", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.Critical)},
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

	dA, err := orch.NewDispatcher(store, poolA, nil, nil, nil,
		orch.NewGapTracker(3), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.DefaultRetryConfig(), lcA, cfg, nil)
	require.NoError(t, err)
	dB, err := orch.NewDispatcher(store, poolB, nil, nil, nil,
		orch.NewGapTracker(3), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.DefaultRetryConfig(), lcB, cfg, nil)
	require.NoError(t, err)

	var mu sync.Mutex
	var aDispatched, bDispatched []string
	dA.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		aDispatched = append(aDispatched, task.ID)
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})
	dB.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
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
			d, err := orch.NewDispatcher(
				store, pool, nil, nil, nil,
				orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
				orch.RetryConfig{MaxRetries: 0},
				lifecycle,
				orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
				nil,
			)
			require.NoError(t, err)
			require.NoError(t, store.CreateTask(&orch.Task{
				ID: "task-1", Status: orch.StatusOpen, Priority: 0,
				Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
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
	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(2), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0}, // no retries — failures go straight to gap
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

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

// TestBVV_ERR01_RetryBehavior verifies the retry protocol (BVV-ERR-01):
// exit code 1 with retries remaining → task is re-dispatched (same ID, no
// clones). After exhausting retries, the task transitions to StatusFailed.
func TestBVV_ERR01_RetryBehavior(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	pool := orch.NewWorkerPool(store, nil, 1, "test-run", "/repo", t.TempDir())

	gaps := orch.NewGapTracker(10)
	retries := orch.NewRetryState()
	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		gaps, retries, orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 2, BaseTimeout: 30 * time.Minute},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "retry-me", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
	}))

	// Count how many times the SpawnFunc is invoked (= dispatch count).
	var spawnCount atomic.Int32
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		spawnCount.Add(1)
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeFailure, 1, roleCfg)
	})

	ctx := context.Background()

	// Run ticks until the task reaches a terminal state.
	// MaxRetries=2 means 3 total attempts (1 initial + 2 retries).
	for i := 0; i < 20; i++ {
		d.Tick(ctx)
		d.Wait()
		task, _ := store.GetTask("retry-me")
		if task.Status.Terminal() {
			break
		}
	}

	task, _ := store.GetTask("retry-me")
	assert.Equal(t, orch.StatusFailed, task.Status,
		"after exhausting retries: task should be failed")
	assert.Equal(t, int32(3), spawnCount.Load(),
		"task should be dispatched 3 times (1 initial + 2 retries)")

	// Verify no retry-task clones were created (BVV retries reset the same task).
	tasks, _ := store.ListTasks("branch:feat/x")
	assert.Len(t, tasks, 1, "only the original task should exist — no clones")
}

// TestBVV_L04_HandoffBudgetExhaustion verifies that exceeding the handoff
// limit transitions the task to failed (BVV-L-04).
func TestBVV_L04_HandoffBudgetExhaustion(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	pool := orch.NewWorkerPool(store, nil, 1, "test-run", "/repo", t.TempDir())

	// MaxHandoffs=1 means the first handoff succeeds, the second exhausts budget.
	handoffs := orch.NewHandoffState(1)
	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(10), orch.NewRetryState(), handoffs,
		orch.RetryConfig{MaxRetries: 0, BaseTimeout: 30 * time.Minute},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "handoff-task", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
	}))

	// Every invocation exits 3 (handoff).
	var callCount atomic.Int32
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		callCount.Add(1)
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeHandoff, 3, roleCfg)
	})

	ctx := context.Background()

	// Tick 1: dispatch → agent exits 3 (handoff #1, within budget).
	d.Tick(ctx)
	d.Wait()
	// Tick 2: process handoff #1 → re-launch → agent exits 3 again (handoff #2).
	d.Tick(ctx)
	d.Wait()
	// Tick 3: process handoff #2 → budget exhausted → treat as failure → task fails.
	d.Tick(ctx)

	task, err := store.GetTask("handoff-task")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, task.Status,
		"handoff budget exhausted → task should be failed")
	assert.GreaterOrEqual(t, callCount.Load(), int32(2),
		"SpawnFunc should have been called at least twice (initial + 1 handoff)")
}

// TestBVV_Run_LifecycleDone verifies that Run() returns nil when all tasks
// complete normally.
func TestBVV_Run_LifecycleDone(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	err := d.Run(context.Background())
	assert.NoError(t, err, "Run should return nil on lifecycle done")

	// Verify all tasks completed.
	for i := 0; i < 3; i++ {
		task, _ := store.GetTask(fmt.Sprintf("p-%d", i))
		assert.Equal(t, orch.StatusCompleted, task.Status)
	}
}

// TestBVV_Run_GapAbort verifies that Run() returns ErrLifecycleAborted when
// gap tolerance is reached.
func TestBVV_Run_GapAbort(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	lifecycle.GapTolerance = 2
	pool := orch.NewWorkerPool(store, nil, 3, "test-run", "/repo", t.TempDir())

	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(2), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1)) // all fail

	err = d.Run(context.Background())
	assert.ErrorIs(t, err, orch.ErrLifecycleAborted,
		"Run should return ErrLifecycleAborted on gap abort")
}

// TestBVV_Run_ContextCancellation verifies that Run() returns ctx.Err() when
// the context is cancelled.
func TestBVV_Run_ContextCancellation(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.LinearGraph(t, store, "feat/x", "builder", 100) // many tasks — won't finish

	// SpawnFunc blocks until context done, then drops the outcome.
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		<-ctx.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	// Let the first tick dispatch, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-done
	assert.ErrorIs(t, err, context.Canceled,
		"Run should return context.Canceled on cancel")
}

// TestBVV_ERR11_OrphanCBTripped verifies that when the circuit breaker trips,
// the dispatcher fails exactly one in-progress task per tick and resets the CB
// (BVV-ERR-11, SUP-05/06).
func TestBVV_ERR11_OrphanCBTripped(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	pool := orch.NewWorkerPool(store, nil, 3, "test-run", "/repo", t.TempDir())

	// Create a real watchdog so we can trip its CB.
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	eventLog, err := orch.NewEventLog(logPath)
	require.NoError(t, err)

	handoffs := orch.NewHandoffState(3)
	watchdog, err := orch.NewWatchdog(
		pool, store, eventLog,
		map[string]orch.RoleConfig{"builder": testutil.MockRoleConfig()},
		handoffs, "feat/x",
		orch.WatchdogConfig{Interval: time.Second, CBThreshold: 3, CBWindow: time.Minute},
		nil,
	)
	require.NoError(t, err)

	gaps := orch.NewGapTracker(10)
	d, err := orch.NewDispatcher(
		store, pool, nil, eventLog, watchdog,
		gaps, orch.NewRetryState(), handoffs,
		orch.RetryConfig{MaxRetries: 0},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	// Create 2 tasks and manually put them in_progress with active workers
	// to simulate the state when the CB trips.
	for i := 0; i < 2; i++ {
		taskID := fmt.Sprintf("orphan-%d", i)
		workerName := fmt.Sprintf("w-%02d", i+1)
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: taskID, Status: orch.StatusInProgress, Priority: 0,
			Assignee: workerName,
			Labels: map[string]string{
				orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical),
			},
		}))
		require.NoError(t, store.CreateWorker(&orch.Worker{
			Name: workerName, Status: orch.WorkerActive, CurrentTaskID: taskID,
		}))
	}

	// Trip the circuit breaker by recording 3 rapid failures.
	recentStart := time.Now()
	for i := 0; i < 3; i++ {
		watchdog.RecordAgentFailure("w-01", recentStart)
	}
	require.True(t, watchdog.CBTripped(), "CB should be tripped after 3 rapid failures")

	// Use a blocking SpawnFunc — we don't want dispatch to actually run.
	d.SetSpawnFunc(func(ctx context.Context, _ *orch.Task, _ *orch.Worker, _ orch.RoleConfig, _ int, _ chan<- orch.TaskOutcome) {
		<-ctx.Done()
	})

	// Tick 1: orphanCk should fail exactly 1 task and reset CB.
	r1 := d.Tick(context.Background())
	assert.Equal(t, 1, r1.OrphansFailed, "CB tripped: exactly 1 orphan failed per tick")
	assert.False(t, watchdog.CBTripped(), "CB should be reset after processing orphan")

	// Verify one task is failed, one is still in_progress.
	t0, _ := store.GetTask("orphan-0")
	t1, _ := store.GetTask("orphan-1")
	failedCount := 0
	if t0.Status == orch.StatusFailed {
		failedCount++
	}
	if t1.Status == orch.StatusFailed {
		failedCount++
	}
	assert.Equal(t, 1, failedCount, "exactly 1 of 2 in-progress tasks should be failed")
}

// TestBVV_DSP01_DiamondDAG verifies that a diamond DAG (A → {B,C} → D)
// dispatches correctly: A first, then B and C in parallel, then D after
// both B and C complete.
func TestBVV_DSP01_DiamondDAG(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 3, "builder")
	testutil.DiamondGraph(t, store, "feat/x", "builder")
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))
	ctx := context.Background()

	// Tick 1: only A is ready (no deps).
	r1 := d.Tick(ctx)
	assert.Equal(t, 1, r1.Dispatched, "tick 1: only A should be dispatched")
	d.Wait()

	// Tick 2: process A's completion → B and C become ready.
	r2 := d.Tick(ctx)
	assert.Equal(t, 2, r2.Dispatched, "tick 2: B and C should be dispatched")
	d.Wait()

	// Tick 3: process B and C → D becomes ready.
	r3 := d.Tick(ctx)
	assert.Equal(t, 1, r3.Dispatched, "tick 3: D should be dispatched")
	d.Wait()

	// Tick 4: process D → lifecycle done.
	r4 := d.Tick(ctx)
	assert.True(t, r4.LifecycleDone, "tick 4: all tasks done → lifecycle done")

	for _, id := range []string{"A", "B", "C", "D"} {
		task, _ := store.GetTask(id)
		assert.Equal(t, orch.StatusCompleted, task.Status, "task %s should be completed", id)
	}
}

// TestBVV_ERR04a_AbortBlocksOpenTasks verifies that when a terminal failure
// triggers abort, all remaining open tasks are blocked (BVV-ERR-04a).
func TestBVV_ERR04a_AbortBlocksOpenTasks(t *testing.T) {
	store := testutil.NewMockStore()
	lifecycle := testutil.MockLifecycleConfig("feat/x", "builder")
	pool := orch.NewWorkerPool(store, nil, 1, "test-run", "/repo", t.TempDir())

	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
		orch.RetryConfig{MaxRetries: 0},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	// Create 1 critical task and 2 open non-critical tasks.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "crit", Status: orch.StatusOpen, Priority: 0,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.Critical)},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "nc-1", Status: orch.StatusOpen, Priority: 1,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "nc-2", Status: orch.StatusOpen, Priority: 2,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder", orch.LabelCriticality: string(orch.NonCritical)},
	}))
	// nc-1 and nc-2 depend on crit, so only crit dispatches first.
	require.NoError(t, store.AddDep("nc-1", "crit"))
	require.NoError(t, store.AddDep("nc-2", "crit"))

	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1)) // fail

	ctx := context.Background()

	// Tick 1: dispatch crit (only ready task).
	d.Tick(ctx)
	d.Wait()
	// Tick 2: crit fails → abort → open tasks blocked.
	r := d.Tick(ctx)
	assert.True(t, r.GapAbort, "critical failure should trigger abort")

	nc1, _ := store.GetTask("nc-1")
	nc2, _ := store.GetTask("nc-2")
	assert.Equal(t, orch.StatusBlocked, nc1.Status, "open task nc-1 should be blocked after abort")
	assert.Equal(t, orch.StatusBlocked, nc2.Status, "open task nc-2 should be blocked after abort")
}

// TestBVV_L01_EmptyLedgerNotDone verifies that an empty ledger does not
// trigger lifecycle completion.
func TestBVV_L01_EmptyLedgerNotDone(t *testing.T) {
	d, _, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	r := d.Tick(context.Background())
	assert.False(t, r.LifecycleDone, "empty ledger should not be 'done'")
}

// TestNewDispatcher_NilValidation verifies that NewDispatcher rejects nil
// required dependencies with clear error messages.
func TestNewDispatcher_NilValidation(t *testing.T) {
	store := testutil.NewMockStore()
	pool := orch.NewWorkerPool(store, nil, 1, "run", "/repo", t.TempDir())
	lc := testutil.MockLifecycleConfig("b", "builder")
	gaps := orch.NewGapTracker(3)
	retries := orch.NewRetryState()
	handoffs := orch.NewHandoffState(3)
	cfg := orch.DispatchConfig{Interval: time.Second, AgentPollInterval: 500 * time.Millisecond}
	rc := orch.DefaultRetryConfig()

	cases := []struct {
		name    string
		store   orch.Store
		pool    *orch.WorkerPool
		lc      *orch.LifecycleConfig
		gaps    *orch.GapTracker
		retries *orch.RetryState
		hoffs   *orch.HandoffState
		wantMsg string
	}{
		{"nil store", nil, pool, lc, gaps, retries, handoffs, "store is required"},
		{"nil pool", store, nil, lc, gaps, retries, handoffs, "pool is required"},
		{"nil lifecycle", store, pool, nil, gaps, retries, handoffs, "lifecycle is required"},
		{"nil gaps", store, pool, lc, nil, retries, handoffs, "gaps is required"},
		{"nil retries", store, pool, lc, gaps, nil, handoffs, "retries is required"},
		{"nil handoffs", store, pool, lc, gaps, retries, nil, "handoffs is required"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := orch.NewDispatcher(c.store, c.pool, nil, nil, nil,
				c.gaps, c.retries, c.hoffs, rc, c.lc, cfg, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantMsg)
		})
	}

	// Positive case: all non-nil → success.
	d, err := orch.NewDispatcher(store, pool, nil, nil, nil,
		gaps, retries, handoffs, rc, lc, cfg, nil)
	require.NoError(t, err)
	assert.NotNil(t, d)
}

// TestBVV_ERR02_TerminateAndRelease_StoreFailure verifies that when
// terminateAndRelease's store write fails, the worker is NOT released
// (preserving task/worker pairing for watchdog recovery) and no event is
// emitted.
func TestBVV_ERR02_TerminateAndRelease_StoreFailure(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	testutil.SingleTask(t, store, "t1", "feat/x", "builder")

	// Dispatch and let the agent succeed.
	spawnFn, ch := testutil.ChannelSpawnFunc()
	d.SetSpawnFunc(spawnFn)

	r1 := d.Tick(context.Background())
	require.Equal(t, 1, r1.Dispatched)

	// Inject store error before the outcome is processed.
	store.SetUpdateTaskErr(fmt.Errorf("injected store failure"))
	ch <- 0 // exit 0 = success
	d.Wait()

	// Process the outcome — terminateAndRelease should fail silently.
	d.Tick(context.Background())

	// Worker must still be active (not released) because store write failed.
	workers, err := store.ListWorkers()
	require.NoError(t, err)
	activeCount := 0
	for _, w := range workers {
		if w.Status == orch.WorkerActive {
			activeCount++
		}
	}
	assert.Equal(t, 1, activeCount, "worker should remain active when store write fails")

	// Clear the error and verify recovery: next tick can process normally.
	store.SetUpdateTaskErr(nil)
}

// TestBVV_ERR03_RetryStoreFailure_FallsThrough verifies that when a retry
// store write fails, the task falls through to terminal failure with the
// original task state intact (assignee preserved, not corrupted).
func TestBVV_ERR03_RetryStoreFailure_FallsThrough(t *testing.T) {
	store := testutil.NewMockStore()
	branch := "feat/x"
	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.MaxRetries = 3

	pool := orch.NewWorkerPool(store, nil, 1, "test-run", "/tmp/repo", t.TempDir())
	retries := orch.NewRetryState()
	gaps := orch.NewGapTracker(lifecycle.GapTolerance)
	handoffs := orch.NewHandoffState(lifecycle.MaxHandoffs)

	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		gaps, retries, handoffs,
		orch.RetryConfig{MaxRetries: 3, BaseTimeout: 30 * time.Minute},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	testutil.SingleTask(t, store, "t1", branch, "builder")

	spawnFn, ch := testutil.ChannelSpawnFunc()
	d.SetSpawnFunc(spawnFn)

	// Dispatch and let the agent fail (exit 1 = retryable).
	r1 := d.Tick(context.Background())
	require.Equal(t, 1, r1.Dispatched)

	// Inject store error so the retry write fails.
	store.SetUpdateTaskErr(fmt.Errorf("injected store failure"))
	ch <- 1 // exit 1 = failure (retryable)
	d.Wait()

	// Process outcome — retry fails, should fall through to terminal failure.
	// terminateAndRelease also calls UpdateTask, which will also fail.
	d.Tick(context.Background())

	// Clear error and verify the task's store state.
	store.SetUpdateTaskErr(nil)

	task, taskErr := store.GetTask("t1")
	require.NoError(t, taskErr)
	// Task should still be in_progress (both the retry and the terminal
	// writes failed), and assignee must be preserved (not cleared).
	assert.Equal(t, orch.StatusInProgress, task.Status,
		"task should remain in_progress when both retry and terminal writes fail")
	assert.NotEmpty(t, task.Assignee,
		"assignee must be preserved — retry copy-on-write prevents corruption")
}

// TestBVV_DSP04_TestModeStoreFailure verifies that test mode store errors
// during dispatch cause the task to be failed and the worker released,
// matching the production error path.
func TestBVV_DSP04_TestModeStoreFailure(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 2)
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// Inject store error — dispatch will fail the test-mode UpdateTask.
	store.SetUpdateTaskErr(fmt.Errorf("injected store failure"))

	r1 := d.Tick(context.Background())
	// No tasks should be successfully dispatched (both fail on UpdateTask).
	assert.Equal(t, 0, r1.Dispatched, "no tasks dispatched when store fails")

	// Clear error and check that workers are released (not leaked).
	store.SetUpdateTaskErr(nil)
	workers, err := store.ListWorkers()
	require.NoError(t, err)
	for _, w := range workers {
		assert.Equal(t, orch.WorkerIdle, w.Status,
			"worker %s should be idle after store failure cleanup", w.Name)
	}
}

// TestBVV_DSN01_DAGDrivenDispatch verifies BVV-DSN-01: dispatch ordering
// emerges from DAG edges, not from any phase/ordering field. A diamond graph
// (A → {B,C} → D) must dispatch A first, then B and C in parallel, then D —
// purely from dependency structure.
func TestBVV_DSN01_DAGDrivenDispatch(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 4, "builder")
	testutil.DiamondGraph(t, store, "feat/x", "builder")

	spawnLog := make([]string, 0)
	var mu sync.Mutex
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		spawnLog = append(spawnLog, task.ID)
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(0), 0, roleCfg)
	})

	ctx := context.Background()
	for tick := 0; tick < 20; tick++ {
		r := d.Tick(ctx)
		d.Wait()
		if r.LifecycleDone {
			break
		}
	}

	// Snapshot spawnLog under the mutex. d.Wait() only synchronizes outcome
	// processing — spawn goroutines may still be past mu.Unlock() but before
	// return, so reads without the lock race.
	mu.Lock()
	logSnapshot := append([]string(nil), spawnLog...)
	mu.Unlock()

	// A must be dispatched before B and C; both B and C must be dispatched before D.
	require.Len(t, logSnapshot, 4, "diamond graph should dispatch exactly the 4 expected tasks")

	counts := make(map[string]int, 4)
	indices := make(map[string]int, 4)
	for i, taskID := range logSnapshot {
		counts[taskID]++
		indices[taskID] = i
	}

	for _, taskID := range []string{"A", "B", "C", "D"} {
		assert.Equal(t, 1, counts[taskID], "task %s should be dispatched exactly once", taskID)
	}

	assert.Less(t, indices["A"], indices["B"], "A must dispatch before B")
	assert.Less(t, indices["A"], indices["C"], "A must dispatch before C")
	assert.Less(t, indices["B"], indices["D"], "B must dispatch before D")
	assert.Less(t, indices["C"], indices["D"], "C must dispatch before D")
}

// TestBVV_DSN02_OneTaskPerSession verifies BVV-DSN-02: each SpawnSession
// call handles exactly one task. The spawn function is invoked once per task.
func TestBVV_DSN02_OneTaskPerSession(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	testutil.LinearGraph(t, store, "feat/x", "builder", 3)

	var spawnCount atomic.Int64
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		spawnCount.Add(1)
		outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(0), 0, roleCfg)
	})

	ctx := context.Background()
	for tick := 0; tick < 20; tick++ {
		r := d.Tick(ctx)
		d.Wait()
		if r.LifecycleDone {
			break
		}
	}

	assert.Equal(t, int64(3), spawnCount.Load(),
		"SpawnFunc called exactly once per task (one-task-per-session)")
}

// TestBVV_S05_RoutingUsesLabelsOnly verifies BVV-S-05: the orchestrator routes
// tasks using the Role() label, not Title, Body, or any output content.
func TestBVV_S05_RoutingUsesLabelsOnly(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")

	// Create a task with content that would break routing if read.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID:     "content-task",
		Title:  "this title is irrelevant to routing",
		Body:   "this body contains no routing info",
		Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      "feat/x",
			orch.LabelRole:        "builder",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}))

	var routedRole string
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		routedRole = task.Role()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(0), 0, roleCfg)
	})

	ctx := context.Background()
	d.Tick(ctx)
	d.Wait()

	assert.Equal(t, "builder", routedRole,
		"routing uses Role() label, not Title or Body content")
}

// TestBVV_DSP02_TickBoundaryDispatch verifies BVV-DSP-02: when A completes
// and unlocks B, B is dispatched on the next Tick, not reentrantly during
// A's outcome processing. The spawn counter guards against reentrant dispatch.
func TestBVV_DSP02_TickBoundaryDispatch(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	testutil.LinearGraph(t, store, "feat/x", "builder", 2) // t-0 → t-1

	var spawnCount atomic.Int64
	baseFn, ch := testutil.ChannelSpawnFunc()
	d.SetSpawnFunc(func(ctx context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, attempt int, outcomes chan<- orch.TaskOutcome) {
		spawnCount.Add(1)
		baseFn(ctx, task, worker, roleCfg, attempt, outcomes)
	})

	ctx := context.Background()

	r1 := d.Tick(ctx)
	require.Equal(t, 1, r1.Dispatched, "tick 1 dispatches t-0")

	ch <- 0
	d.Wait()

	assert.EqualValues(t, 1, spawnCount.Load(),
		"outcome processing must not reentrantly dispatch t-1 within the same tick")
	task1, err := store.GetTask("t-1")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, task1.Status,
		"t-1 should remain open until the next Tick")

	r2 := d.Tick(ctx)
	assert.Equal(t, 1, r2.Dispatched, "tick 2 dispatches t-1")
	ch <- 0
	d.Wait()
	assert.EqualValues(t, 2, spawnCount.Load(), "second tick produces a second spawn")
}
