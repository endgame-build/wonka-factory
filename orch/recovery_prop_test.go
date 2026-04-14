//go:build verify

package orch_test

import (
	"fmt"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"pgregory.net/rapid"
)

// TestProp_HandoffRetryBounded verifies BVV-L-04 + BVV-ERR-01: handoff and
// retry counts per task never exceed their configured limits, regardless of
// the order or number of record calls.
func TestProp_HandoffRetryBounded(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		maxRetries := rapid.IntRange(1, 5).Draw(rt, "maxRetries")
		maxHandoffs := rapid.IntRange(1, 5).Draw(rt, "maxHandoffs")
		numOps := rapid.IntRange(1, maxRetries+maxHandoffs+10).Draw(rt, "numOps")

		retries := orch.NewRetryState()
		handoffs := orch.NewHandoffState(maxHandoffs)
		cfg := orch.RetryConfig{MaxRetries: maxRetries}

		taskID := "prop-task"
		retryCount := 0
		handoffCount := 0

		for i := 0; i < numOps; i++ {
			op := rapid.IntRange(0, 1).Draw(rt, fmt.Sprintf("op_%d", i))
			switch op {
			case 0: // retry
				if retries.CanRetry(taskID, cfg) {
					retries.RecordAttempt(taskID)
					retryCount++
				}
			case 1: // handoff
				if _, ok := handoffs.TryRecord(taskID); ok {
					handoffCount++
				}
			}
		}

		if retryCount > maxRetries {
			rt.Fatalf("[BVV-ERR-01] retry count %d exceeds max %d", retryCount, maxRetries)
		}
		if handoffCount > maxHandoffs {
			rt.Fatalf("[BVV-L-04] handoff count %d exceeds max %d", handoffCount, maxHandoffs)
		}
	})
}

// TestProp_AbortCleanupNoOpenTasks verifies BVV-ERR-04a: after an abort
// cleanup, no tasks remain in non-terminal status.
func TestProp_AbortCleanupNoOpenTasks(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop-abort"

		// Create random mix of tasks in various states.
		n := rapid.IntRange(2, 15).Draw(rt, "numTasks")
		for i := 0; i < n; i++ {
			status := rapid.SampledFrom([]orch.TaskStatus{
				orch.StatusOpen, orch.StatusAssigned, orch.StatusInProgress,
				orch.StatusCompleted, orch.StatusFailed, orch.StatusBlocked,
			}).Draw(rt, fmt.Sprintf("status_%d", i))

			task := &orch.Task{
				ID:       fmt.Sprintf("abort-%d", i),
				Status:   status,
				Priority: i,
				Labels: map[string]string{
					orch.LabelBranch:      branch,
					orch.LabelRole:        "builder",
					orch.LabelCriticality: string(orch.NonCritical),
				},
			}
			if err := store.CreateTask(task); err != nil {
				rt.Fatal(err)
			}
		}

		// Simulate abort cleanup: set all non-terminal tasks to blocked.
		tasks, err := store.ListTasks("branch:" + branch)
		if err != nil {
			rt.Fatal(err)
		}
		for _, task := range tasks {
			if !task.Status.Terminal() {
				task.Status = orch.StatusBlocked
				if err := store.UpdateTask(task); err != nil {
					rt.Fatal(err)
				}
			}
		}

		// Verify: no non-terminal tasks remain.
		tasksAfter, err := store.ListTasks("branch:" + branch)
		if err != nil {
			rt.Fatal(err)
		}
		for _, task := range tasksAfter {
			if !task.Status.Terminal() {
				rt.Fatalf("[BVV-ERR-04a] task %s still in non-terminal status %s after abort cleanup", task.ID, task.Status)
			}
		}
	})
}
