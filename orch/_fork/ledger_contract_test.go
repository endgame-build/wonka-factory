package orch_test

import (
	"sync"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore creates a BeadsStore (the default), falling back to FSStore
// when the Beads/Dolt infrastructure is unavailable.
func newTestStore(t *testing.T) orch.Store {
	t.Helper()
	return newTestStoreInDir(t, t.TempDir())
}

// newTestStoreInDir creates a Store in dir using the default backend (Beads
// with FS fallback), and registers cleanup.
func newTestStoreInDir(t *testing.T, dir string) orch.Store {
	t.Helper()
	store, err := orch.NewStore("", dir)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

// StoreFactory creates a fresh Store for testing. Returns the store and its
// backing directory so durability tests can reopen from the same path.
type StoreFactory func(t *testing.T) (orch.Store, string)

// ReopenFunc creates a new Store instance backed by the same directory.
// Used for LDG-01 durability testing where a second Store must be opened
// against an existing directory to verify persistence across restarts.
type ReopenFunc func(t *testing.T, dir string) orch.Store

// RunStoreContractTests runs the full LDG spec test suite against any Store
// implementation. This is the canonical compliance gate — every Store must
// pass every sub-test identically.
func RunStoreContractTests(t *testing.T, factory StoreFactory, reopen ReopenFunc) {
	t.Helper()

	t.Run("LDG01_Durability", func(t *testing.T) {
		store, dir := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "persist", Type: orch.TypeAgent, Status: orch.StatusOpen}))

		store2 := reopen(t, dir)
		t.Cleanup(func() { store2.Close() })
		got, err := store2.GetTask("persist")
		require.NoError(t, err)
		assert.Equal(t, "persist", got.ID)
		assert.Equal(t, orch.StatusOpen, got.Status)
	})

	t.Run("LDG02_SingleSourceOfTruth", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		got, err := store.GetTask("t1")
		require.NoError(t, err)
		assert.Equal(t, "t1", got.ID)
	})

	t.Run("LDG03_ParentChildRelationships", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "parent", Type: orch.TypePhase, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c1", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c2", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "other", Type: orch.TypeAgent, Status: orch.StatusOpen}))

		children, err := store.GetChildren("parent")
		require.NoError(t, err)
		assert.Len(t, children, 2)
	})

	t.Run("LDG04_DependencyBlocked", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep", Type: orch.TypeAgent, Status: orch.StatusInProgress}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocked", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("blocked", "dep"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)
		for _, r := range ready {
			assert.NotEqual(t, "blocked", r.ID)
		}
	})

	t.Run("LDG06_CycleDetection", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c", Type: orch.TypeAgent, Status: orch.StatusOpen}))

		require.NoError(t, store.AddDep("a", "b"))
		require.NoError(t, store.AddDep("b", "c"))
		err := store.AddDep("c", "a")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrCycle)
	})

	t.Run("LDG06_SelfCycle", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "x", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		err := store.AddDep("x", "x")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrCycle)
	})

	t.Run("LDG07_DeterministicTiebreaker", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "zebra", Type: orch.TypeAgent, Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "alpha", Type: orch.TypeAgent, Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "mid", Type: orch.TypeAgent, Status: orch.StatusOpen, Priority: 0}))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)
		require.Len(t, ready, 3)
		assert.Equal(t, "alpha", ready[0].ID)
		assert.Equal(t, "mid", ready[1].ID)
		assert.Equal(t, "zebra", ready[2].ID)
	})

	t.Run("LDG08_AtomicAssign", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))

		require.NoError(t, store.Assign("t1", "w1"))

		task, _ := store.GetTask("t1")
		assert.Equal(t, orch.StatusAssigned, task.Status)
		assert.Equal(t, "w1", task.Assignee)

		worker, _ := store.GetWorker("w1")
		assert.Equal(t, orch.WorkerActive, worker.Status)
		assert.Equal(t, "t1", worker.CurrentTaskID)
	})

	// LDG-09: re-assignment to a different worker is rejected (already-assigned guard).
	t.Run("LDG09_RejectReassignmentToDifferentWorker", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t1", "w1"))

		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerIdle}))
		err := store.Assign("t1", "w2")
		require.Error(t, err)
	})

	t.Run("LDG10_SerializedAssignment", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t2", Type: orch.TypeAgent, Status: orch.StatusOpen}))
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

	// LDG-12, CHK-01: atomic writes persist state durably.
	t.Run("LDG12_AtomicWrites", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "atomic", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		got, err := store.GetTask("atomic")
		require.NoError(t, err)
		assert.Equal(t, "atomic", got.ID)
	})

	t.Run("LDG14_NewTaskInitialization", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "new", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		got, _ := store.GetTask("new")
		assert.Equal(t, orch.StatusOpen, got.Status)
		assert.Equal(t, "", got.Assignee)
	})

	t.Run("LDG14a_AssignedToInProgress", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
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
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t2", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t1", "w1"))

		err := store.Assign("t2", "w1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrWorkerBusy)
	})

	// LDG-16, LDG-17: parent status derives to completed when all children are terminal and none failed.
	t.Run("LDG16_ParentStatusUpdate", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "parent", Type: orch.TypePhase, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c1", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusCompleted}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c2", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusCompleted}))

		status, err := orch.DeriveParentStatus(store, "parent")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusCompleted, status)
	})

	t.Run("CreateTask_DuplicateReturnsErrTaskExists", func(t *testing.T) {
		store, _ := factory(t)
		task := &orch.Task{ID: "dup", Type: orch.TypeAgent, Status: orch.StatusOpen}
		require.NoError(t, store.CreateTask(task))

		err := store.CreateTask(&orch.Task{ID: "dup", Type: orch.TypeAgent, Status: orch.StatusOpen})
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
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b", Type: orch.TypeAgent, Status: orch.StatusOpen}))

		require.NoError(t, store.AddDep("a", "b"))
		require.NoError(t, store.AddDep("a", "b"))

		deps, _ := store.GetDeps("a")
		assert.Len(t, deps, 1)
	})

	t.Run("ReadyWithTerminalDeps", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep", Type: orch.TypeAgent, Status: orch.StatusCompleted}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "waiter", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("waiter", "dep"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := make(map[string]bool)
		for _, r := range ready {
			ids[r.ID] = true
		}
		assert.True(t, ids["waiter"], "waiter should be ready — dep is terminal")
	})
}

// RunStorePropTests runs property-based parent derivation and ReadyTasks tests
// against any Store implementation.
func RunStorePropTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("Prop_ParentDerivation_AllCompleted", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "parent", Type: orch.TypePhase, Status: orch.StatusOpen}))
		for _, id := range []string{"c1", "c2", "c3"} {
			require.NoError(t, store.CreateTask(&orch.Task{ID: id, ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusCompleted}))
		}
		status, err := orch.DeriveParentStatus(store, "parent")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusCompleted, status)
	})

	t.Run("Prop_ParentDerivation_AnyFailed", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "parent", Type: orch.TypePhase, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c1", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusCompleted}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c2", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusFailed}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c3", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusCompleted}))

		status, err := orch.DeriveParentStatus(store, "parent")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusFailed, status)
	})

	t.Run("Prop_ParentDerivation_NonTerminal", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "parent", Type: orch.TypePhase, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c1", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusCompleted}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c2", ParentID: "parent", Type: orch.TypeAgent, Status: orch.StatusInProgress}))

		status, err := orch.DeriveParentStatus(store, "parent")
		require.NoError(t, err)
		assert.Equal(t, orch.StatusOpen, status)
	})

	t.Run("Prop_ReadyTasksCorrectness", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocker", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocked", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "ready1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "assigned", Type: orch.TypeAgent, Status: orch.StatusAssigned, Assignee: "w1"}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "done", Type: orch.TypeAgent, Status: orch.StatusCompleted}))
		require.NoError(t, store.AddDep("blocked", "blocker"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := make(map[string]bool)
		for _, r := range ready {
			ids[r.ID] = true
		}
		assert.True(t, ids["ready1"], "ready1 should be in ready set")
		assert.True(t, ids["blocker"], "blocker should be ready (no deps)")
		assert.False(t, ids["blocked"], "blocked should not be ready (dep not terminal)")
		assert.False(t, ids["assigned"], "assigned should not be ready (has assignee)")
		assert.False(t, ids["done"], "done should not be ready (not open)")
	})
}
