//go:build verify

package testutil_test

import (
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
)

// Compile-time interface check.
var _ orch.Store = (*testutil.MockStore)(nil)

// TestMockStore_BasicOps is a smoke test for MockStore's core operations
// before running the full contract suite (which is in orch_test and needs
// the StoreFactory wiring — see ledger_test.go).
func TestMockStore_BasicOps(t *testing.T) {
	s := testutil.NewMockStore()

	// CreateTask + GetTask round-trip.
	err := s.CreateTask(&orch.Task{
		ID:     "t1",
		Title:  "test task",
		Status: orch.StatusOpen,
		Labels: map[string]string{"branch": "feat/x", "role": "builder"},
	})
	assert.NoError(t, err)

	got, err := s.GetTask("t1")
	assert.NoError(t, err)
	assert.Equal(t, "t1", got.ID)
	assert.Equal(t, "test task", got.Title)
	assert.Equal(t, orch.StatusOpen, got.Status)
	assert.Equal(t, "builder", got.Labels["role"])

	// Mutation isolation: modifying the returned task must not affect the store.
	got.Title = "mutated"
	got2, _ := s.GetTask("t1")
	assert.Equal(t, "test task", got2.Title, "store should return clones, not internal pointers")

	// Duplicate CreateTask.
	err = s.CreateTask(&orch.Task{ID: "t1", Status: orch.StatusOpen})
	assert.ErrorIs(t, err, orch.ErrTaskExists)

	// GetTask not found.
	_, err = s.GetTask("nonexistent")
	assert.ErrorIs(t, err, orch.ErrNotFound)

	// Worker + Assign.
	assert.NoError(t, s.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))
	assert.NoError(t, s.Assign("t1", "w1"))

	task, _ := s.GetTask("t1")
	assert.Equal(t, orch.StatusAssigned, task.Status)
	assert.Equal(t, "w1", task.Assignee)

	worker, _ := s.GetWorker("w1")
	assert.Equal(t, orch.WorkerActive, worker.Status)
	assert.Equal(t, "t1", worker.CurrentTaskID)

	// ReadyTasks — assigned task should NOT appear.
	assert.NoError(t, s.CreateTask(&orch.Task{ID: "t2", Status: orch.StatusOpen, Labels: map[string]string{"branch": "feat/x"}}))
	ready, err := s.ReadyTasks("branch:feat/x")
	assert.NoError(t, err)
	assert.Len(t, ready, 1)
	assert.Equal(t, "t2", ready[0].ID)

	// Dependencies + cycle detection.
	assert.NoError(t, s.CreateTask(&orch.Task{ID: "a", Status: orch.StatusOpen}))
	assert.NoError(t, s.CreateTask(&orch.Task{ID: "b", Status: orch.StatusOpen}))
	assert.NoError(t, s.AddDep("a", "b"))
	err = s.AddDep("b", "a")
	assert.ErrorIs(t, err, orch.ErrCycle)

	// Path traversal rejection.
	err = s.CreateTask(&orch.Task{ID: "../escape", Status: orch.StatusOpen})
	assert.ErrorIs(t, err, orch.ErrInvalidID)
}
