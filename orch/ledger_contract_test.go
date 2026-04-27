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
		require.NoError(t, store.CreateTask(&orch.Task{ID: "persist-1", Status: orch.StatusOpen}))

		store2 := reopen(t, dir)
		t.Cleanup(func() { store2.Close() })
		got, err := store2.GetTask("persist-1")
		require.NoError(t, err)
		assert.Equal(t, "persist-1", got.ID)
		assert.Equal(t, orch.StatusOpen, got.Status)
	})

	t.Run("LDG02_SingleSourceOfTruth", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		got, err := store.GetTask("t-1")
		require.NoError(t, err)
		assert.Equal(t, "t-1", got.ID)
	})

	t.Run("LDG04_DependencyBlocked", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep-1", Status: orch.StatusInProgress}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocked-1", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("blocked-1", "dep-1"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)
		for _, r := range ready {
			assert.NotEqual(t, "blocked-1", r.ID)
		}
	})

	// D5 regression: a blocked dep is terminal, so downstream tasks become ready.
	// BVV-ERR-04a defines blocked as terminal; LDG-04's contrapositive says
	// terminal deps do NOT block downstream.
	t.Run("LDG04a_BlockedDepUnblocksDownstream", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocker-1", Status: orch.StatusBlocked}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "downstream-1", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("downstream-1", "blocker-1"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["downstream-1"], "downstream should be ready — blocker is terminal (blocked)")
	})

	t.Run("LDG06_CycleDetection", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c-1", Status: orch.StatusOpen}))

		require.NoError(t, store.AddDep("a-1", "b-1"))
		require.NoError(t, store.AddDep("b-1", "c-1"))
		err := store.AddDep("c-1", "a-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrCycle)
	})

	t.Run("LDG06_SelfCycle", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "x-1", Status: orch.StatusOpen}))
		err := store.AddDep("x-1", "x-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrCycle)
	})

	t.Run("LDG07_DeterministicTiebreaker", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "zebra-1", Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "alpha-1", Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "mid-1", Status: orch.StatusOpen, Priority: 0}))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)
		require.Len(t, ready, 3)
		assert.Equal(t, "alpha-1", ready[0].ID)
		assert.Equal(t, "mid-1", ready[1].ID)
		assert.Equal(t, "zebra-1", ready[2].ID)
	})

	t.Run("LDG08_AtomicAssign", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-1", Status: orch.WorkerIdle}))

		require.NoError(t, store.Assign("t-1", "w-1"))

		task, _ := store.GetTask("t-1")
		assert.Equal(t, orch.StatusAssigned, task.Status)
		assert.Equal(t, "w-1", task.Assignee)

		worker, _ := store.GetWorker("w-1")
		assert.Equal(t, orch.WorkerActive, worker.Status)
		assert.Equal(t, "t-1", worker.CurrentTaskID)
	})

	t.Run("LDG09_RejectReassignmentToDifferentWorker", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t-1", "w-1"))

		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-2", Status: orch.WorkerIdle}))
		err := store.Assign("t-1", "w-2")
		require.Error(t, err)
	})

	t.Run("LDG10_SerializedAssignment", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-2", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-1", Status: orch.WorkerIdle}))

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = store.Assign("t-1", "w-1") }()
		go func() { defer wg.Done(); errs[1] = store.Assign("t-2", "w-1") }()
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
		require.NoError(t, store.CreateTask(&orch.Task{ID: "atomic-1", Status: orch.StatusOpen}))
		got, err := store.GetTask("atomic-1")
		require.NoError(t, err)
		assert.Equal(t, "atomic-1", got.ID)
	})

	t.Run("LDG14_NewTaskInitialization", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "new-1", Status: orch.StatusOpen}))
		got, _ := store.GetTask("new-1")
		assert.Equal(t, orch.StatusOpen, got.Status)
		assert.Equal(t, "", got.Assignee)
	})

	t.Run("LDG14a_AssignedToInProgress", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t-1", "w-1"))

		task, _ := store.GetTask("t-1")
		assert.Equal(t, orch.StatusAssigned, task.Status)

		task.Status = orch.StatusInProgress
		require.NoError(t, store.UpdateTask(task))

		got, _ := store.GetTask("t-1")
		assert.Equal(t, orch.StatusInProgress, got.Status)
	})

	t.Run("LDG15_NoReassignment", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-2", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("t-1", "w-1"))

		err := store.Assign("t-2", "w-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrWorkerBusy)
	})

	t.Run("CreateTask_DuplicateReturnsErrTaskExists", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dup-1", Status: orch.StatusOpen}))

		err := store.CreateTask(&orch.Task{ID: "dup-1", Status: orch.StatusOpen})
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
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b-1", Status: orch.StatusOpen}))

		require.NoError(t, store.AddDep("a-1", "b-1"))
		require.NoError(t, store.AddDep("a-1", "b-1"))

		deps, _ := store.GetDeps("a-1")
		assert.Len(t, deps, 1)
	})

	t.Run("ReadyWithTerminalDeps", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep-1", Status: orch.StatusCompleted}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "waiter-1", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("waiter-1", "dep-1"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["waiter-1"], "waiter should be ready — dep is terminal")
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

	// StatusAssigned round-trip: beads maps StatusAssigned→StatusOpen and
	// distinguishes via Issue.Assignee. A regression in toTask that forgets
	// the Assignee check will read back StatusOpen instead of StatusAssigned.
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
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-1", Status: orch.WorkerIdle}))
		err := store.Assign("ghost-task", "w-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, orch.ErrNotFound)
	})

	t.Run("Assign_WorkerNotFound", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "t-1", Status: orch.StatusOpen}))
		err := store.Assign("t-1", "ghost-worker")
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

	// validateID path-traversal rejection: every Store method that accepts
	// an external task ID or worker name must reject traversal attempts
	// with ErrInvalidID rather than leaking ENOENT or writing outside the
	// ledger directory. Contract-level so both FSStore and BeadsStore stay
	// consistent (BeadsStore still resolves workers on the filesystem).
	t.Run("ValidateID_RejectsPathTraversal", func(t *testing.T) {
		store, _ := factory(t)
		const bad = "../escape"

		err := store.CreateTask(&orch.Task{ID: bad, Status: orch.StatusOpen})
		assert.ErrorIs(t, err, orch.ErrInvalidID, "CreateTask should reject traversal")

		_, err = store.GetTask(bad)
		assert.ErrorIs(t, err, orch.ErrInvalidID, "GetTask should reject traversal")

		err = store.UpdateTask(&orch.Task{ID: bad, Status: orch.StatusOpen})
		assert.ErrorIs(t, err, orch.ErrInvalidID, "UpdateTask should reject traversal")

		err = store.CreateWorker(&orch.Worker{Name: bad, Status: orch.WorkerIdle})
		assert.ErrorIs(t, err, orch.ErrInvalidID, "CreateWorker should reject traversal")

		_, err = store.GetWorker(bad)
		assert.ErrorIs(t, err, orch.ErrInvalidID, "GetWorker should reject traversal")

		err = store.UpdateWorker(&orch.Worker{Name: bad, Status: orch.WorkerIdle})
		assert.ErrorIs(t, err, orch.ErrInvalidID, "UpdateWorker should reject traversal")

		err = store.Assign(bad, "whatever")
		assert.ErrorIs(t, err, orch.ErrInvalidID, "Assign should reject traversal taskID")

		err = store.Assign("whatever", bad)
		assert.ErrorIs(t, err, orch.ErrInvalidID, "Assign should reject traversal workerName")

		err = store.AddDep(bad, "whatever")
		assert.ErrorIs(t, err, orch.ErrInvalidID, "AddDep should reject traversal taskID")

		err = store.AddDep("whatever", bad)
		assert.ErrorIs(t, err, orch.ErrInvalidID, "AddDep should reject traversal dependsOn")

		_, err = store.GetDeps(bad)
		assert.ErrorIs(t, err, orch.ErrInvalidID, "GetDeps should reject traversal")
	})

	// Title/Body round-trip: both fields must survive create + update cycles
	// across every backend. Regression guard: beads UpdateIssue is map-based,
	// so Title and Body must be explicitly included in the updates map (see
	// BeadsStore.UpdateTask) — previously omitted and silently dropped.
	t.Run("TitleBody_RoundTrip", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{
			ID:     "tb-rt",
			Title:  "initial title",
			Body:   "initial body text\nwith multiple lines",
			Status: orch.StatusOpen,
		}))

		got, err := store.GetTask("tb-rt")
		require.NoError(t, err)
		assert.Equal(t, "initial title", got.Title, "Title must survive CreateTask")
		assert.Equal(t, "initial body text\nwith multiple lines", got.Body, "Body must survive CreateTask")

		got.Title = "updated title"
		got.Body = "updated body"
		require.NoError(t, store.UpdateTask(got))

		got2, err := store.GetTask("tb-rt")
		require.NoError(t, err)
		assert.Equal(t, "updated title", got2.Title, "Title must survive UpdateTask")
		assert.Equal(t, "updated body", got2.Body, "Body must survive UpdateTask")
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
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a-1", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b-1", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/y"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c-1", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x", "role": "builder"}}))

		ready, err := store.ReadyTasks("branch:feat/x")
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["a-1"])
		assert.True(t, ids["c-1"])
		assert.False(t, ids["b-1"], "b has branch:feat/y, should not match")
	})

	// Label filter: ListTasks with label filter.
	t.Run("LDG_LabelFilter_ListTasks", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "a-1", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "b-1", Status: orch.StatusCompleted, Labels: map[string]string{"branch": "feat/x"}}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "c-1", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/y"}}))

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

	// BVV-S-03 torture: generate high cross-worker and cross-task contention
	// by spawning every (task, worker) pair as a racing goroutine. With
	// numTasks=numWorkers=20 that is 400 goroutines contending over shared
	// state. A correct Store must guarantee both halves of BVV-S-03:
	//
	//   1. No task ends up with two different assignees.
	//   2. No worker ends up holding two different tasks simultaneously.
	//
	// Asserting only the total success count (as the previous version did)
	// is insufficient — a broken Store could assign two workers to the same
	// task and still report the "right" count. This test verifies the
	// invariant directly by inspecting ledger state after the race.
	t.Run("LDG_S03_NoDoubleAssignment", func(t *testing.T) {
		store, _ := factory(t)
		const numTasks = 20
		const numWorkers = 20

		taskID := func(i int) string { return fmt.Sprintf("t-%d", i) }
		workerName := func(i int) string { return fmt.Sprintf("w-%d", i) }

		for i := 0; i < numTasks; i++ {
			require.NoError(t, store.CreateTask(&orch.Task{
				ID:     taskID(i),
				Status: orch.StatusOpen,
			}))
		}
		for i := 0; i < numWorkers; i++ {
			require.NoError(t, store.CreateWorker(&orch.Worker{
				Name:   workerName(i),
				Status: orch.WorkerIdle,
			}))
		}

		// Every (task, worker) pair races simultaneously. This creates both
		// task-level contention (workers 0..19 all targeting task 0) and
		// worker-level contention (tasks 0..19 all targeting worker 0).
		//
		// The semaphore caps concurrent Assign goroutines at 32 — without it,
		// CLI-backed stores fork-bomb with 400 simultaneous subprocesses, and
		// even idle goroutines waste stack memory until they get scheduled.
		// Acquiring before `go func` (rather than inside) keeps the goroutine
		// population bounded by maxConcurrent; FSStore and BeadsStore are
		// unaffected because their Assign is fast. The store-level invariant
		// (no double-assignment) holds at any concurrency.
		const maxConcurrent = 32
		sem := make(chan struct{}, maxConcurrent)
		var wg sync.WaitGroup
		wg.Add(numTasks * numWorkers)
		for ti := 0; ti < numTasks; ti++ {
			for wi := 0; wi < numWorkers; wi++ {
				sem <- struct{}{}
				go func(tID, wName string) {
					defer wg.Done()
					defer func() { <-sem }()
					_ = store.Assign(tID, wName)
				}(taskID(ti), workerName(wi))
			}
		}
		wg.Wait()

		// Invariant 1: no task has more than one assignee, and each assigned
		// task's assignee points to a worker that actually holds it.
		taskToWorker := make(map[string]string)
		for i := 0; i < numTasks; i++ {
			task, err := store.GetTask(taskID(i))
			require.NoError(t, err)
			if task.Assignee != "" {
				taskToWorker[task.ID] = task.Assignee
				assert.Equal(t, orch.StatusAssigned, task.Status,
					"task %s has assignee %s but status %s", task.ID, task.Assignee, task.Status)
			}
		}

		// Invariant 2: no worker holds more than one task, and each active
		// worker's CurrentTaskID points to a task that actually names it.
		workerToTask := make(map[string]string)
		for i := 0; i < numWorkers; i++ {
			worker, err := store.GetWorker(workerName(i))
			require.NoError(t, err)
			if worker.Status == orch.WorkerActive {
				workerToTask[worker.Name] = worker.CurrentTaskID
				assert.NotEmpty(t, worker.CurrentTaskID,
					"worker %s is active but has no CurrentTaskID", worker.Name)
			}
		}

		// Sanity floor: a Store that rejects every Assign would pass the
		// invariant checks above trivially (empty maps are consistent).
		// All tasks start open and all workers idle, so at least one
		// Assign is guaranteed to succeed.
		assert.GreaterOrEqual(t, len(taskToWorker), 1,
			"expected at least one successful assignment under contention")

		// Cross-consistency: the task→worker and worker→task maps must be
		// mutual inverses. A broken Store could leave a task pointing at a
		// worker that is already busy with a different task (or vice versa).
		assert.Equal(t, len(taskToWorker), len(workerToTask),
			"mismatched assignment counts: %d tasks assigned, %d workers active",
			len(taskToWorker), len(workerToTask))
		for tID, wName := range taskToWorker {
			assert.Equal(t, tID, workerToTask[wName],
				"task %s claims worker %s, but worker holds %q", tID, wName, workerToTask[wName])
		}
		for wName, tID := range workerToTask {
			assert.Equal(t, wName, taskToWorker[tID],
				"worker %s claims task %s, but task holds %q", wName, tID, taskToWorker[tID])
		}
	})

	// Criticality label round-trip: create task with criticality=critical,
	// read back, verify IsCritical() holds.
	t.Run("LDG_LabelRoundtrip_Criticality", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{
			ID:     "crit-1",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelCriticality: string(orch.Critical)},
		}))

		got, err := store.GetTask("crit-1")
		require.NoError(t, err)
		assert.True(t, got.IsCritical(), "IsCritical() should be true after round-trip")

		// Non-critical task.
		require.NoError(t, store.CreateTask(&orch.Task{
			ID:     "noncrit-1",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelCriticality: string(orch.NonCritical)},
		}))
		got2, _ := store.GetTask("noncrit-1")
		assert.False(t, got2.IsCritical(), "IsCritical() should be false for non_critical")
	})

	// ReadyTasks correctness: ready set excludes terminal, assigned, and
	// dependency-blocked tasks.
	t.Run("ReadyTasks_Correctness", func(t *testing.T) {
		store, _ := factory(t)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocker-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blocked-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "ready-1", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "assigned-1", Status: orch.StatusAssigned, Assignee: "w-1"}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "done-1", Status: orch.StatusCompleted}))
		require.NoError(t, store.AddDep("blocked-1", "blocker-1"))

		ready, err := store.ReadyTasks()
		require.NoError(t, err)

		ids := readyIDs(ready)
		assert.True(t, ids["ready-1"], "ready1 should be in ready set")
		assert.True(t, ids["blocker-1"], "blocker should be ready (no deps)")
		assert.False(t, ids["blocked-1"], "blocked should not be ready (dep not terminal)")
		assert.False(t, ids["assigned-1"], "assigned should not be ready (has assignee)")
		assert.False(t, ids["done-1"], "done should not be ready (not open)")
	})
}

func readyIDs(tasks []*orch.Task) map[string]bool {
	ids := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		ids[t.ID] = true
	}
	return ids
}
