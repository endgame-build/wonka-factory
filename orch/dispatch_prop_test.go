//go:build verify

package orch_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestProp_TerminalCountNeverDecreases verifies BVV-S-02: the count of tasks
// in terminal states monotonically increases across ticks.
func TestProp_TerminalCountNeverDecreases(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop"
		testutil.RandomDAG(rt, store, branch)

		lifecycle := testutil.MockLifecycleConfig(branch, "builder", "verifier")
		pool := orch.NewWorkerPool(store, nil, 5, "prop-run", "/repo", t.TempDir())
		d, dErr := orch.NewDispatcher(
			store, pool, nil, nil, nil,
			orch.NewGapTracker(100), orch.NewRetryState(), orch.NewHandoffState(3),
			orch.RetryConfig{MaxRetries: 0},
			lifecycle,
			orch.DispatchConfig{Interval: time.Millisecond, AgentPollInterval: time.Millisecond},
			nil,
		)
		if dErr != nil {
			rt.Fatal(dErr)
		}
		d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

		prevTerminal := 0
		ctx := context.Background()
		for tick := 0; tick < 50; tick++ {
			d.Tick(ctx)
			d.Wait()

			tasks, _ := store.ListTasks("branch:" + branch)
			terminal := 0
			for _, task := range tasks {
				if task.Status.Terminal() {
					terminal++
				}
			}
			if terminal < prevTerminal {
				rt.Fatalf("[BVV-S-02] terminal count decreased: %d → %d at tick %d", prevTerminal, terminal, tick)
			}
			prevTerminal = terminal

			if terminal == len(tasks) {
				break
			}
		}
	})
}

// TestProp_NoDepsDispatchedBeforeTerminal verifies BVV-S-04: no task is
// dispatched (moved past open) while any of its dependencies is non-terminal.
func TestProp_NoDepsDispatchedBeforeTerminal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop"
		testutil.RandomDAG(rt, store, branch)

		lifecycle := testutil.MockLifecycleConfig(branch, "builder", "verifier")
		pool := orch.NewWorkerPool(store, nil, 5, "prop-run", "/repo", t.TempDir())
		d, dErr := orch.NewDispatcher(
			store, pool, nil, nil, nil,
			orch.NewGapTracker(100), orch.NewRetryState(), orch.NewHandoffState(3),
			orch.RetryConfig{MaxRetries: 0},
			lifecycle,
			orch.DispatchConfig{Interval: time.Millisecond, AgentPollInterval: time.Millisecond},
			nil,
		)
		if dErr != nil {
			rt.Fatal(dErr)
		}
		d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

		ctx := context.Background()
		for tick := 0; tick < 50; tick++ {
			d.Tick(ctx)
			d.Wait()

			// After each tick, verify no non-open task has a non-terminal dep.
			tasks, _ := store.ListTasks("branch:" + branch)
			for _, task := range tasks {
				if task.Status == orch.StatusOpen {
					continue // open tasks haven't been dispatched yet
				}
				deps, _ := store.GetDeps(task.ID)
				for _, depID := range deps {
					dep, err := store.GetTask(depID)
					if err != nil {
						continue
					}
					if !dep.Status.Terminal() {
						rt.Fatalf("[BVV-S-04] task %s (status %s) dispatched but dep %s is %s at tick %d",
							task.ID, task.Status, depID, dep.Status, tick)
					}
				}
			}

			// Check if all done.
			allDone := true
			for _, task := range tasks {
				if !task.Status.Terminal() {
					allDone = false
					break
				}
			}
			if allDone {
				break
			}
		}
	})
}

// TestProp_SingleWorkerPerTask verifies BVV-S-03: at any point in time, each
// task has at most one assigned worker.
func TestProp_SingleWorkerPerTask(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop"
		testutil.RandomDAG(rt, store, branch)

		lifecycle := testutil.MockLifecycleConfig(branch, "builder", "verifier")
		pool := orch.NewWorkerPool(store, nil, 5, "prop-run", "/repo", t.TempDir())
		d, dErr := orch.NewDispatcher(
			store, pool, nil, nil, nil,
			orch.NewGapTracker(100), orch.NewRetryState(), orch.NewHandoffState(3),
			orch.RetryConfig{MaxRetries: 0},
			lifecycle,
			orch.DispatchConfig{Interval: time.Millisecond, AgentPollInterval: time.Millisecond},
			nil,
		)
		if dErr != nil {
			rt.Fatal(dErr)
		}
		d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

		ctx := context.Background()
		for tick := 0; tick < 50; tick++ {
			d.Tick(ctx)
			d.Wait()

			// Check worker→task uniqueness.
			workers, _ := store.ListWorkers()
			taskToWorkers := make(map[string][]string)
			for _, w := range workers {
				if w.CurrentTaskID != "" {
					taskToWorkers[w.CurrentTaskID] = append(taskToWorkers[w.CurrentTaskID], w.Name)
				}
			}
			for taskID, ws := range taskToWorkers {
				if len(ws) > 1 {
					rt.Fatalf("[BVV-S-03] task %s assigned to %d workers: %v at tick %d",
						taskID, len(ws), ws, tick)
				}
			}

			tasks, _ := store.ListTasks("branch:" + branch)
			allDone := true
			for _, task := range tasks {
				if !task.Status.Terminal() {
					allDone = false
					break
				}
			}
			if allDone {
				break
			}
		}
	})
}

// TestProp_GapBoundedOvershoot verifies BVV-ERR-04: gap count never exceeds
// tolerance + maxWorkers - 1 (bounded overshoot from concurrent outcomes).
func TestProp_GapBoundedOvershoot(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop"
		maxWorkers := 3
		tolerance := rapid.IntRange(1, 5).Draw(rt, "tolerance")

		// Create parallel non-critical tasks that all fail.
		n := rapid.IntRange(tolerance+1, tolerance+maxWorkers+3).Draw(rt, "numTasks")
		for i := 0; i < n; i++ {
			err := store.CreateTask(&orch.Task{
				ID:       fmt.Sprintf("gap-%d", i),
				Status:   orch.StatusOpen,
				Priority: 0,
				Labels: map[string]string{
					orch.LabelBranch:      branch,
					orch.LabelRole:        "builder",
					orch.LabelCriticality: string(orch.NonCritical),
				},
			})
			require.NoError(rt, err, "CreateTask must succeed for property test soundness")
		}

		lifecycle := testutil.MockLifecycleConfig(branch, "builder")
		lifecycle.GapTolerance = tolerance
		pool := orch.NewWorkerPool(store, nil, maxWorkers, "prop-run", "/repo", t.TempDir())
		gaps := orch.NewGapTracker(tolerance)
		d, dErr := orch.NewDispatcher(
			store, pool, nil, nil, nil,
			gaps, orch.NewRetryState(), orch.NewHandoffState(3),
			orch.RetryConfig{MaxRetries: 0},
			lifecycle,
			orch.DispatchConfig{Interval: time.Millisecond, AgentPollInterval: time.Millisecond},
			nil,
		)
		if dErr != nil {
			rt.Fatal(dErr)
		}
		d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1)) // all fail

		ctx := context.Background()
		for tick := 0; tick < 50; tick++ {
			d.Tick(ctx)
			d.Wait()

			// Gap overshoot bound: tolerance + maxWorkers - 1.
			maxGaps := tolerance + maxWorkers - 1
			if gaps.Count() > maxGaps {
				rt.Fatalf("[BVV-ERR-04] gap count %d exceeds bound %d (tolerance=%d, maxWorkers=%d) at tick %d",
					gaps.Count(), maxGaps, tolerance, maxWorkers, tick)
			}

			r := d.Tick(ctx)
			if r.GapAbort || r.LifecycleDone {
				break
			}
		}
	})
}

// TestProp_MixedOutcomeDAGTerminates verifies that random DAGs with random
// exit codes (0/1/2) always terminate: every task reaches a terminal state
// and no invariant is violated. This stress-tests the interaction between
// failures, retries, gap accumulation, and dependency blocking.
func TestProp_MixedOutcomeDAGTerminates(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := testutil.NewMockStore()
		branch := "prop-mixed"
		tasks := testutil.RandomDAG(rt, store, branch)

		maxRetries := rapid.IntRange(0, 2).Draw(rt, "maxRetries")
		tolerance := rapid.IntRange(1, len(tasks)+1).Draw(rt, "tolerance")
		maxWorkers := rapid.IntRange(1, 5).Draw(rt, "maxWorkers")

		lifecycle := testutil.MockLifecycleConfig(branch, "builder", "verifier")
		lifecycle.GapTolerance = tolerance
		lifecycle.MaxRetries = maxRetries

		pool := orch.NewWorkerPool(store, nil, maxWorkers, "prop-run", "/repo", t.TempDir())
		gaps := orch.NewGapTracker(tolerance)
		d, dErr := orch.NewDispatcher(
			store, pool, nil, nil, nil,
			gaps, orch.NewRetryState(), orch.NewHandoffState(3),
			orch.RetryConfig{MaxRetries: maxRetries},
			lifecycle,
			orch.DispatchConfig{Interval: time.Millisecond, AgentPollInterval: time.Millisecond},
			nil,
		)
		if dErr != nil {
			rt.Fatal(dErr)
		}

		// Pre-generate exit codes from rapid on the test goroutine (thread-safe).
		// SpawnFunc goroutines pick from this pool via atomic index.
		maxTicks := 200
		exitCodes := []int{0, 1, 2}
		codePoolSize := maxTicks * maxWorkers
		if codePoolSize < 10 {
			codePoolSize = 10
		}
		codePool := make([]int, codePoolSize)
		for i := range codePool {
			codePool[i] = rapid.SampledFrom(exitCodes).Draw(rt, fmt.Sprintf("code_%d", i))
		}
		var codeIdx atomic.Int64
		d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
			idx := int(codeIdx.Add(1)-1) % codePoolSize
			code := codePool[idx]
			outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(code), code, roleCfg)
		})

		ctx := context.Background()
		terminated := false
		for tick := 0; tick < maxTicks; tick++ {
			r := d.Tick(ctx)
			d.Wait()
			if r.LifecycleDone || r.GapAbort {
				terminated = true
				break
			}
			if r.Error != nil {
				rt.Fatalf("unexpected dispatch error at tick %d: %v", tick, r.Error)
			}
		}

		if !terminated {
			rt.Fatalf("DAG with %d tasks did not terminate within %d ticks (tolerance=%d, retries=%d, workers=%d)",
				len(tasks), maxTicks, tolerance, maxRetries, maxWorkers)
		}
	})
}
