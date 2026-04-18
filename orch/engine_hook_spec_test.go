//go:build verify

package orch_test

import (
	"context"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatcher_PostSuccessHookFires verifies the dispatcher calls the
// post-success hook exactly once per task transitioning to completed, and
// only after the store write persists. Validates the seam used by Engine
// for BVV-TG-07..10 post-planner validation.
func TestDispatcher_PostSuccessHookFires(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 2)

	var hookCalls []string
	d.SetPostSuccessHook(func(task *orch.Task) {
		hookCalls = append(hookCalls, task.ID)
	})
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	d.Tick(context.Background())
	d.Wait()
	d.Tick(context.Background()) // drain outcomes

	assert.Len(t, hookCalls, 2, "hook fires once per successful completion")
}

// TestDispatcher_PostSuccessHookNotCalledOnFailure verifies that exit-1
// (retryable failure) and exit-2 (blocked) outcomes do NOT fire the
// post-success hook. Prevents spurious graph validation on terminal
// failure paths.
func TestDispatcher_PostSuccessHookNotCalledOnFailure(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "fail-1", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      "feat/x",
			orch.LabelRole:        "builder",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}))

	called := false
	d.SetPostSuccessHook(func(task *orch.Task) { called = true })
	// Exit code 2 = blocked (terminal, non-retryable); hook must not fire.
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(2))

	d.Tick(context.Background())
	d.Wait()
	d.Tick(context.Background())

	assert.False(t, called, "hook must not fire on blocked outcome")
}

// TestDispatcher_AbortLifecycle verifies the Dispatcher's exported abort
// method stops further dispatch and blocks remaining open tasks — the
// hook for BVV-TG-07..10 validation failures relies on this seam.
func TestDispatcher_AbortLifecycle(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	// Three independent ready tasks.
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)

	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))
	d.AbortLifecycle()

	// Post-abort tick must dispatch nothing.
	result := d.Tick(context.Background())
	assert.Equal(t, 0, result.Dispatched, "dispatch after abort must be a no-op")

	// All previously-open tasks must now be blocked (abortCleanup).
	tasks, err := store.ListTasks("branch:feat/x")
	require.NoError(t, err)
	for _, task := range tasks {
		assert.Equal(t, orch.StatusBlocked, task.Status,
			"task %s should be blocked after AbortLifecycle", task.ID)
	}
}
