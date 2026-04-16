//go:build verify

package orch_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/require"
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

// TestProp_AbortCleanupNoOpenTasks verifies BVV-ERR-04a: after the Dispatcher's
// abort cleanup runs (triggered by critical task failure), no open tasks remain.
// Uses the production abortCleanup path via Dispatcher.Tick, not inline logic.
func TestProp_AbortCleanupNoOpenTasks(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop-abort"

		// Create 1 critical task and N non-critical open tasks.
		n := rapid.IntRange(1, 10).Draw(rt, "numNonCritical")
		require.NoError(rt, store.CreateTask(&orch.Task{
			ID: "crit", Status: orch.StatusOpen, Priority: 0,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        "builder",
				orch.LabelCriticality: string(orch.Critical),
			},
		}))
		for i := 0; i < n; i++ {
			require.NoError(rt, store.CreateTask(&orch.Task{
				ID: fmt.Sprintf("nc-%d", i), Status: orch.StatusOpen, Priority: i + 1,
				Labels: map[string]string{
					orch.LabelBranch:      branch,
					orch.LabelRole:        "builder",
					orch.LabelCriticality: string(orch.NonCritical),
				},
			}))
			// Non-critical tasks depend on critical task.
			require.NoError(rt, store.AddDep(fmt.Sprintf("nc-%d", i), "crit"))
		}

		lifecycle := testutil.MockLifecycleConfig(branch, "builder")
		pool := orch.NewWorkerPool(store, nil, 1, "abort-run", "/repo", t.TempDir())
		d, dErr := orch.NewDispatcher(
			store, pool, nil, nil, nil,
			orch.NewGapTracker(10), orch.NewRetryState(), orch.NewHandoffState(3),
			orch.RetryConfig{MaxRetries: 0},
			lifecycle,
			orch.DispatchConfig{Interval: time.Millisecond, AgentPollInterval: time.Millisecond},
			nil,
		)
		if dErr != nil {
			rt.Fatal(dErr)
		}

		// Critical task fails (exit 1) → triggers abort cleanup.
		d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1))

		ctx := context.Background()
		for tick := 0; tick < 20; tick++ {
			r := d.Tick(ctx)
			d.Wait()
			if r.GapAbort || r.LifecycleDone {
				break
			}
		}

		// Verify: no open tasks remain after abort cleanup.
		tasks, err := store.ListTasks("branch:" + branch)
		if err != nil {
			rt.Fatal(err)
		}
		for _, task := range tasks {
			if task.Status == orch.StatusOpen {
				rt.Fatalf("[BVV-ERR-04a] task %s still open after abort cleanup", task.ID)
			}
		}
	})
}
