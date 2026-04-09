//go:build verify

package orch_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// StoreFactory creates a fresh Store for testing. Returns the store and its
// backing directory so durability tests can reopen from the same path.
type StoreFactory func(t *testing.T) (orch.Store, string)

// ReopenFunc creates a new Store instance backed by the same directory.
type ReopenFunc func(t *testing.T, dir string) orch.Store

// RunStoreContractTests runs the full LDG spec test suite against any Store
// implementation. This is the canonical compliance gate — every Store must
// pass every sub-test identically.
func RunStoreContractTests(t *testing.T, factory StoreFactory, reopen ReopenFunc) {
	t.Helper()

	t.Run("LDG01_Durability", func(t *testing.T) {
		store, dir := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "persist", Status: orch.StatusOpen}))

		store2 := reopen(t, dir)
		t.Cleanup(func() { store2.Close() })
		got, err := store2.GetTask("persist")
		require.NoError(t, err)
		assert.Equal(t, "persist", got.ID)
		assert.Equal(t, orch.StatusOpen, got.Status)
	})

	t.Run("LDG02_SingleSourceOfTruth", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		got, err := store.GetTask("t1")
		require.NoError(t, err)
		assert.Equal(t, "t1", got.ID)
	})

	t.Run("LDG04_DependencyBlocked", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep", Status: orch.StatusInProgress}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocked", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("blocked", "dep"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)
		for _, r := range ready {
			assert.NotEqual(t, "blocked", r.ID)
		}
	})

	// D5 regression: a blocked dep is terminal, so downstream tasks become ready.
	// BVV-ERR-04a defines blocked as terminal; LDG-04's contrapositive says
	// terminal deps do NOT block downstream.
	t.Run("LDG04a_BlockedDepUnblocksDownstream", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocker", Status: orch.StatusBlocked}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "downstream", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("downstream", "blocker"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["downstream"], "downstream should be ready — blocker is terminal (blocked)")
	})

	t.Run("LDG06_CycleDetection", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c", Status: orch.StatusOpen}))

		require.NoError(t, store.AddDep("a", "b"))
		require.NoError(t, store.AddDep("b", "c"))
		err := store.AddDep("c", "a")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrCycle)
	})

	t.Run("LDG06_SelfCycle", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "x", Status: orch.StatusOpen}))
		err := store.AddDep("x", "x")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrCycle)
	})

	t.Run("LDG07_DeterministicTiebreaker", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "zebra", Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "alpha", Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "mid", Status: orch.StatusOpen, Priority: 0}))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)
		require.Len(t, ready, 3)
		assert.Equal(t, "alpha", ready[0].ID)
		assert.Equal(t, "mid", ready[1].ID)
		assert.Equal(t, "zebra", ready[2].ID)
	})

	t.Run("LDG08_AtomicAssign", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))

		require.NoError(t, store.Assign("t1", "w1"))

		task, _ := store.GetTask("t1")
		assert.Equal(t, orch.StatusAssigned, task.Status)
		assert.Equal(t, "w1", task.Assignee)

		worker, _ := store.GetWorker("w1")
		assert.Equal(t, orch.WorkerActive, worker.Status)
		assert.Equal(t, "t1", worker.CurrentTaskID)
	})

	t.Run("LDG09_RejectReassignmentToDifferentWorker", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t1", "w1"))

		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerIdle}))
		err := store.Assign("t1", "w2")
		require.Error(t, err)
	})

	t.Run("LDG10_SerializedAssignment", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t2", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = store.Assign("t1", "w1") }()
		go func() { defer wg.Done(); errs[1] = store.Assign("t2", "w1") }()
		wg.Wait()

		successes := 0
		for _, err := range errs {
			if err == nil {
				successes++
			}
		}
		assert.Equal(t, 1, successes, "exactly one concurrent assign should succeed")
	})

	// LDG-12: atomic writes. This test only verifies read-after-write
	// consistency, not crash-safety (which requires fault injection).
	t.Run("LDG12_ReadAfterWrite", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "atomic", Status: orch.StatusOpen}))
		got, err := store.GetTask("atomic")
		require.NoError(t, err)
		assert.Equal(t, "atomic", got.ID)
	})

	t.Run("LDG14_NewTaskInitialization", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "new", Status: orch.StatusOpen}))
		got, _ := store.GetTask("new")
		assert.Equal(t, orch.StatusOpen, got.Status)
		assert.Equal(t, "", got.Assignee)
	})

	t.Run("LDG14a_AssignedToInProgress", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t1", "w1"))

		task, _ := store.GetTask("t1")
		assert.Equal(t, orch.StatusAssigned, task.Status)

		task.Status = orch.StatusInProgress
		require.NoError(t, store.UpdateTask(task))

		got, _ := store.GetTask("t1")
		assert.Equal(t, orch.StatusInProgress, got.Status)
	})

	t.Run("LDG15_NoReassignment", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t2", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t1", "w1"))

		err := store.Assign("t2", "w1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrWorkerBusy)
	})

	t.Run("CreateTask_DuplicateReturnsErrTaskExists", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dup", Status: orch.StatusOpen}))

		err := store.CreateTask(&orch.Task{ID: "dup", Status: orch.StatusOpen})
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrTaskExists)
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store, _ := factory(t)
		_, err := store.GetTask("nonexistent")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	t.Run("AddDepIdempotent", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b", Status: orch.StatusOpen}))

		require.NoError(t, store.AddDep("a", "b"))
		require.NoError(t, store.AddDep("a", "b"))

		deps, _ := store.GetDeps("a")
		assert.Len(t, deps, 1)
	})

	t.Run("ReadyWithTerminalDeps", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep", Status: orch.StatusCompleted}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "waiter", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("waiter", "dep"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["waiter"], "waiter should be ready — dep is terminal")
	})

	// --- Status round-trip tests (prevents beads mapping regressions) ---

	// StatusFailed round-trip: create→update to failed→read back, verify not
	// confused with StatusCompleted (both map to beads.StatusClosed, distinguished
	// by orch:failed label).
	t.Run("StatusFailed_RoundTrip", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "fail-rt", Status: orch.StatusOpen}))
		task, err := store.GetTask("fail-rt")
		require.NoError(t, err)
		task.Status = orch.StatusFailed
		require.NoError(t, store.UpdateTask(task))

		got, err := store.GetTask("fail-rt")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusFailed, got.Status, "StatusFailed must survive round-trip")
	})

	// StatusCompleted round-trip: ensure not confused with StatusFailed.
	t.Run("StatusCompleted_RoundTrip", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "done-rt", Status: orch.StatusOpen}))
		task, err := store.GetTask("done-rt")
		require.NoError(t, err)
		task.Status = orch.StatusCompleted
		require.NoError(t, store.UpdateTask(task))

		got, err := store.GetTask("done-rt")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusCompleted, got.Status, "StatusCompleted must survive round-trip")
	})

	// StatusAssigned round-trip (regression: commit 574f7cb).
	t.Run("StatusAssigned_RoundTrip", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "asgn-rt", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-rt", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("asgn-rt", "w-rt"))

		got, err := store.GetTask("asgn-rt")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusAssigned, got.Status, "StatusAssigned must survive round-trip")
		assert.Equal(t, "w-rt", got.Assignee)
	})

	// StatusBlocked round-trip.
	t.Run("StatusBlocked_RoundTrip", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blk-rt", Status: orch.StatusOpen}))
		task, err := store.GetTask("blk-rt")
		require.NoError(t, err)
		task.Status = orch.StatusBlocked
		require.NoError(t, store.UpdateTask(task))

		got, err := store.GetTask("blk-rt")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusBlocked, got.Status, "StatusBlocked must survive round-trip")
	})

	// --- Not-found error path tests ---

	t.Run("UpdateTask_NotFound", func(t *testing.T) {
		store, _ := factory(t)
		err := store.UpdateTask(&orch.Task{ID: "ghost", Status: orch.StatusOpen})
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	t.Run("GetWorker_NotFound", func(t *testing.T) {
		store, _ := factory(t)
		_, err := store.GetWorker("ghost")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	t.Run("UpdateWorker_NotFound", func(t *testing.T) {
		store, _ := factory(t)
		err := store.UpdateWorker(&orch.Worker{Name: "ghost", Status: orch.WorkerIdle})
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	t.Run("Assign_TaskNotFound", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		err := store.Assign("ghost-task", "w1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	t.Run("Assign_WorkerNotFound", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen}))
		err := store.Assign("t1", "ghost-worker")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	// --- Worker contract tests ---

	t.Run("CreateWorker_DuplicateReturnsErrWorkerExists", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "dup", Status: orch.WorkerIdle}))
		err := store.CreateWorker(&orch.Worker{Name: "dup", Status: orch.WorkerIdle})
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrWorkerExists)
	})

	t.Run("ListWorkers_SortedByName", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "zulu", Status: orch.WorkerIdle}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "alpha", Status: orch.WorkerIdle}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "mid", Status: orch.WorkerIdle}))

		workers, err := store.ListWorkers()
		require.NoError(t, err)
		require.Len(t, workers, 3)
		assert.Equal(t, "alpha", workers[0].Name)
		assert.Equal(t, "mid", workers[1].Name)
		assert.Equal(t, "zulu", workers[2].Name)
	})

	t.Run("ListWorkers_Empty", func(t *testing.T) {
		store, _ := factory(t)
		workers, err := store.ListWorkers()
		require.NoError(t, err)
		assert.Empty(t, workers)
	})

	// UpdateTask with label mutation: old labels fully replaced.
	t.Run("UpdateTask_LabelMutation", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{
			ID:     "lbl-mut",
			Status: orch.StatusOpen,
			Labels: map[string]string{"role": "builder", "branch": "feat/x"},
		}))

		task, err := store.GetTask("lbl-mut")
		require.NoError(t, err)
		task.Labels = map[string]string{"role": "verifier", "branch": "main"}
		require.NoError(t, store.UpdateTask(task))

		got, err := store.GetTask("lbl-mut")
		require.NoError(t, err)
		assert.Equal(t, "verifier", got.Labels["role"], "role label should be updated")
		assert.Equal(t, "main", got.Labels["branch"], "branch label should be updated")
	})

	// --- BVV-specific additions ---

	// Label filter: ReadyTasks with branch label filter.
	t.Run("LDG_LabelFilter_ReadyTasks", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/y"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x", "role": "builder"}}))

		ready, err := store.ReadyTasks("branch:feat/x")
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["a"])
		assert.True(t, ids["c"])
		assert.False(t, ids["b"], "b has branch:feat/y, should not match")
	})

	// Label filter: ListTasks with label filter.
	t.Run("LDG_LabelFilter_ListTasks", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b", Status: orch.StatusCompleted, Labels: map[string]string{"branch": "feat/x"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/y"}}))

		tasks, err := store.ListTasks("branch:feat/x")
		require.NoError(t, err)
		assert.Len(t, tasks, 2, "ListTasks should return a and b (both have branch:feat/x)")

		// Without filter returns all.
		all, err := store.ListTasks()
		require.NoError(t, err)
		assert.Len(t, all, 3)
	})

	// Label filter: malformed filter returns error.
	t.Run("LDG_LabelFilter_MalformedReturnsError", func(t *testing.T) {
		store, _ := factory(t)
		_, err := store.ReadyTasks("novalue")
		require.Error(t, err)
		assert.True(t, errors.Is(err, orch.ErrInvalidLabelFilter), "malformed filter should return ErrInvalidLabelFilter, got: %v", err)

		_, err = store.ListTasks("novalue")
		require.Error(t, err)
		assert.True(t, errors.Is(err, orch.ErrInvalidLabelFilter))
	})

	// BVV-S-03 torture: 100 goroutines racing to assign 50 tasks to 50 workers.
	// Exactly 50 assignments must succeed; no double-assignment.
	t.Run("LDG_S03_NoDoubleAssignment", func(t *testing.T) {
		store, _ := factory(t)
		const numTasks = 50
		const numWorkers = 50
		const goroutines = 100

		for i := 0; i < numTasks; i++ {
			require.NoError(t, store.CreateTask(&orch.Task{
				ID:     fmt.Sprintf("t%d", i),
				Status: orch.StatusOpen,
			}))
		}
		for i := 0; i < numWorkers; i++ {
			require.NoError(t, store.CreateWorker(&orch.Worker{
				Name:   fmt.Sprintf("w%d", i),
				Status: orch.WorkerIdle,
			}))
		}

		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			successes int
		)
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			taskIdx := g % numTasks
			workerIdx := g % numWorkers
			go func(tID, wName string) {
				defer wg.Done()
				if err := store.Assign(tID, wName); err == nil {
					mu.Lock()
					successes++
					mu.Unlock()
				}
			}(fmt.Sprintf("t%d", taskIdx), fmt.Sprintf("w%d", workerIdx))
		}
		wg.Wait()

		assert.Equal(t, numWorkers, successes,
			"exactly %d assignments should succeed (1 per worker)", numWorkers)
	})

	// Criticality label round-trip: create task with criticality=critical,
	// read back, verify IsCritical() holds.
	t.Run("LDG_LabelRoundtrip_Criticality", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{
			ID:     "crit",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelCriticality: string(orch.Critical)},
		}))

		got, err := store.GetTask("crit")
		require.NoError(t, err)
		assert.True(t, got.IsCritical(), "IsCritical() should be true after round-trip")

		// Non-critical task.
		require.NoError(t, store.CreateTask(&orch.Task{
			ID:     "noncrit",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelCriticality: string(orch.NonCritical)},
		}))
		got2, _ := store.GetTask("noncrit")
		assert.False(t, got2.IsCritical(), "IsCritical() should be false for non_critical")
	})

	// ReadyTasks correctness (ported from fork, adapted for BVV).
	t.Run("ReadyTasks_Correctness", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocker", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocked", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "ready1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "assigned", Status: orch.StatusAssigned, Assignee: "w1"}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "done", Status: orch.StatusCompleted}))
		require.NoError(t, store.AddDep("blocked", "blocker"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["ready1"], "ready1 should be in ready set")
		assert.True(t, ids["blocker"], "blocker should be ready (no deps)")
		assert.False(t, ids["blocked"], "blocked should not be ready (dep not terminal)")
		assert.False(t, ids["assigned"], "assigned should not be ready (has assignee)")
		assert.False(t, ids["done"], "done should not be ready (not open)")
	})
}

func readyIDs(tasks []*orch.Task) map[string]bool {
	ids := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		ids[t.ID] = true
	}
	return ids
}
