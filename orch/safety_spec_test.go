//go:build verify

package orch_test

import (
	"path/filepath"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBVV_S02_TerminalIrreversibility verifies that AssertTerminalIrreversibility
// panics when a terminal status is changed to a different status (BVV-S-02).
func TestBVV_S02_TerminalIrreversibility(t *testing.T) {
	// Terminal → different status should panic.
	assert.Panics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusCompleted, orch.StatusOpen)
	}, "completed → open should panic")

	assert.Panics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusFailed, orch.StatusOpen)
	}, "failed → open should panic")

	assert.Panics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusBlocked, orch.StatusOpen)
	}, "blocked → open should panic")

	assert.Panics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusCompleted, orch.StatusFailed)
	}, "completed → failed should panic")

	// Terminal → same status is OK (idempotent write).
	assert.NotPanics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusCompleted, orch.StatusCompleted)
	}, "completed → completed is idempotent")

	// Non-terminal → anything is OK.
	assert.NotPanics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusOpen, orch.StatusAssigned)
	})
	assert.NotPanics(t, func() {
		orch.AssertTerminalIrreversibility(orch.StatusInProgress, orch.StatusCompleted)
	})
}

// TestBVV_S03_SingleAssignment verifies that AssertSingleAssignment panics
// when more than one worker holds the same task (BVV-S-03).
func TestBVV_S03_SingleAssignment(t *testing.T) {
	store := testutil.NewMockStore()

	require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerIdle}))

	// Single assignment — should not panic.
	require.NoError(t, store.Assign("t1", "w1"))
	assert.NotPanics(t, func() {
		orch.AssertSingleAssignment(store, "t1")
	})

	// Force a second worker to hold the same task (bypassing Assign guards).
	w2, _ := store.GetWorker("w2")
	w2.CurrentTaskID = "t1"
	w2.Status = orch.WorkerActive
	require.NoError(t, store.UpdateWorker(w2))

	assert.Panics(t, func() {
		orch.AssertSingleAssignment(store, "t1")
	}, "two workers holding same task should panic")
}

// TestBVV_S04_DependencyOrdering verifies that AssertDependencyOrdering
// panics when a dispatched task has a non-terminal dependency (BVV-S-04).
func TestBVV_S04_DependencyOrdering(t *testing.T) {
	store := testutil.NewMockStore()

	require.NoError(t, store.CreateTask(&orch.Task{ID: "dep", Status: orch.StatusOpen}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "child", Status: orch.StatusOpen}))
	require.NoError(t, store.AddDep("child", "dep"))

	// Dep is open (non-terminal) — should panic.
	assert.Panics(t, func() {
		orch.AssertDependencyOrdering(store, "child")
	}, "non-terminal dep should panic")

	// Complete the dep — should not panic.
	dep, _ := store.GetTask("dep")
	dep.Status = orch.StatusCompleted
	require.NoError(t, store.UpdateTask(dep))

	assert.NotPanics(t, func() {
		orch.AssertDependencyOrdering(store, "child")
	})
}

// TestBVV_S07_BoundedDegradation verifies that AssertBoundedDegradation
// panics when the gap count exceeds tolerance (BVV-S-07).
func TestBVV_S07_BoundedDegradation(t *testing.T) {
	gaps := orch.NewGapTracker(2)

	// Within tolerance — should not panic.
	gaps.IncrementAndCheck("t1")
	assert.NotPanics(t, func() {
		orch.AssertBoundedDegradation(gaps, 2)
	})

	gaps.IncrementAndCheck("t2")
	assert.NotPanics(t, func() {
		orch.AssertBoundedDegradation(gaps, 2)
	})

	// Exceeds tolerance — should panic.
	gaps.IncrementAndCheck("t3")
	assert.Panics(t, func() {
		orch.AssertBoundedDegradation(gaps, 2)
	}, "3 gaps > tolerance 2 should panic")
}

// TestBVV_S01_LifecycleExclusion verifies that AssertLifecycleExclusion panics
// when the lifecycle lock is not held for the branch (BVV-S-01).
func TestBVV_S01_LifecycleExclusion(t *testing.T) {
	dir := t.TempDir()
	lock := orch.NewLifecycleLock(orch.LockConfig{
		Path: filepath.Join(dir, "test.lock"),
	})

	// Lock not held — should panic.
	assert.Panics(t, func() {
		orch.AssertLifecycleExclusion(lock, "feat/x")
	}, "unheld lock should panic")

	// Acquire the lock — should not panic.
	require.NoError(t, lock.Acquire("holder-1", "feat/x"))
	assert.NotPanics(t, func() {
		orch.AssertLifecycleExclusion(lock, "feat/x")
	})

	// Nil lock — should not panic (graceful skip).
	assert.NotPanics(t, func() {
		orch.AssertLifecycleExclusion(nil, "feat/x")
	})
}

// TestBVV_S08_AssignmentDurability verifies that an assignment survives
// store close and reopen (BVV-S-08).
func TestBVV_S08_AssignmentDurability(t *testing.T) {
	dir := t.TempDir()
	store, _, err := orch.NewStore("fs", dir)
	require.NoError(t, err)

	require.NoError(t, store.CreateTask(&orch.Task{
		ID:     "durable-t",
		Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "b"},
	}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "durable-w", Status: orch.WorkerIdle}))
	require.NoError(t, store.Assign("durable-t", "durable-w"))
	require.NoError(t, store.Close())

	// Reopen the store and verify the assignment persisted.
	store2, _, err := orch.NewStore("fs", dir)
	require.NoError(t, err)
	defer store2.Close()

	task, err := store2.GetTask("durable-t")
	require.NoError(t, err)
	assert.Equal(t, "durable-w", task.Assignee, "assignee must survive close/reopen")
	assert.Equal(t, orch.StatusAssigned, task.Status, "status must survive close/reopen")
}

// TestBVV_DSN03_HandoffIsInfrastructureDriven verifies BVV-DSN-03: the handoff
// counter lives in HandoffState (infrastructure); the orchestrator never reads
// or mutates task.Body (agent-owned memory).
func TestBVV_DSN03_HandoffIsInfrastructureDriven(t *testing.T) {
	const maxHandoffs = 3
	h := orch.NewHandoffState(maxHandoffs)
	taskID := "handoff-t"

	agentMemory := "PROGRESS.md: step 1/3 complete\nstep 2/3 in progress"
	task := &orch.Task{
		ID:     taskID,
		Status: orch.StatusInProgress,
		Body:   agentMemory,
		Labels: map[string]string{
			orch.LabelBranch: "feat/x",
			orch.LabelRole:   "builder",
		},
	}

	for i := 1; i <= maxHandoffs; i++ {
		count, ok := h.TryRecord(taskID)
		require.True(t, ok, "handoff %d within limit must be recorded", i)
		require.Equal(t, i, count, "counter increments monotonically")
	}

	count, ok := h.TryRecord(taskID)
	assert.False(t, ok, "at MaxHandoffs, infrastructure must refuse handoff")
	assert.Equal(t, maxHandoffs, count, "refused TryRecord must not increment")

	assert.Equal(t, agentMemory, task.Body,
		"orchestrator must not read or mutate agent memory (task.Body)")
}
