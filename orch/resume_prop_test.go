//go:build verify

package orch_test

import (
	"fmt"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"pgregory.net/rapid"
)

// propTask creates a task in the store without requiring *testing.T.
func propTask(store *testutil.MockStore, id, branch, role string) *orch.Task {
	task := &orch.Task{
		ID:       id,
		Title:    "task " + id,
		Status:   orch.StatusOpen,
		Priority: 0,
		Labels: map[string]string{
			orch.LabelBranch:      branch,
			orch.LabelRole:        role,
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}
	_ = store.CreateTask(task) // ignore duplicate errors
	return task
}

// TestProp_ReconcileNeverReversesTerminal verifies BVV-S-02: for any random
// task set, Reconcile never changes a terminal task's status.
func TestProp_ReconcileNeverReversesTerminal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "feat/prop"

		n := rapid.IntRange(2, 10).Draw(rt, "numTasks")
		type snapshot struct {
			id     string
			status orch.TaskStatus
		}
		var terminals []snapshot

		statuses := []orch.TaskStatus{
			orch.StatusOpen, orch.StatusAssigned, orch.StatusInProgress,
			orch.StatusCompleted, orch.StatusFailed, orch.StatusBlocked,
		}

		for i := 0; i < n; i++ {
			id := fmt.Sprintf("task-%d", i)
			task := propTask(store, id, branch, "builder")
			status := statuses[rapid.IntRange(0, len(statuses)-1).Draw(rt, "status")]
			task.Status = status
			if status == orch.StatusAssigned || status == orch.StatusInProgress {
				task.Assignee = "w1"
			}
			_ = store.UpdateTask(task)
			if status.Terminal() {
				terminals = append(terminals, snapshot{task.ID, status})
			}
		}

		tmux := newMockSession("run-prop")

		_, err := orch.Reconcile(store, tmux, branch, "")
		if err != nil {
			return
		}

		for _, snap := range terminals {
			got, getErr := store.GetTask(snap.id)
			if getErr != nil {
				continue
			}
			if got.Status != snap.status {
				rt.Fatalf("[BVV-S-02] terminal task %s changed from %s to %s", snap.id, snap.status, got.Status)
			}
		}
	})
}

// TestProp_ReconcileIdempotent verifies that running Reconcile twice on the
// same state produces identical results (all steps are idempotent).
func TestProp_ReconcileIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "feat/idem"

		n := rapid.IntRange(1, 5).Draw(rt, "numTasks")
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("idem-%d", i)
			task := propTask(store, id, branch, "builder")
			if rapid.Bool().Draw(rt, "inProgress") {
				task.Status = orch.StatusInProgress
				task.Assignee = "w1"
				_ = store.UpdateTask(task)
			}
		}

		tmux := newMockSession("run-idem")

		result1, err1 := orch.Reconcile(store, tmux, branch, "")
		if err1 != nil {
			return
		}

		result2, err2 := orch.Reconcile(store, tmux, branch, "")
		if err2 != nil {
			rt.Fatalf("second Reconcile failed: %v", err2)
		}

		if result2.Reconciled != 0 {
			rt.Fatalf("idempotency violated: second Reconcile reconciled %d tasks (first: %d)",
				result2.Reconciled, result1.Reconciled)
		}
	})
}
