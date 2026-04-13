//go:build verify

package orch

import "fmt"

// AssertTerminalIrreversibility panics if a terminal status is being changed
// to a different status. BVV-S-02: completed/failed/blocked are irreversible.
func AssertTerminalIrreversibility(prev, next TaskStatus) {
	if prev.Terminal() && next != prev {
		panic(fmt.Sprintf("[BVV-S-02] terminal irreversibility violated: %s → %s", prev, next))
	}
}

// AssertSingleAssignment panics if more than one worker holds the given task.
// BVV-S-03: at most one worker assigned per task.
func AssertSingleAssignment(store Store, taskID string) {
	workers, err := store.ListWorkers()
	if err != nil {
		return // can't verify — skip
	}
	count := 0
	for _, w := range workers {
		if w.CurrentTaskID == taskID {
			count++
		}
	}
	if count > 1 {
		panic(fmt.Sprintf("[BVV-S-03] single assignment violated: task %s assigned to %d workers", taskID, count))
	}
}

// AssertDependencyOrdering panics if a dispatched task has non-terminal deps.
// BVV-S-04: tasks must not be dispatched before all dependencies are terminal.
func AssertDependencyOrdering(store Store, taskID string) {
	deps, err := store.GetDeps(taskID)
	if err != nil {
		return
	}
	for _, depID := range deps {
		dep, err := store.GetTask(depID)
		if err != nil {
			continue
		}
		if !dep.Status.Terminal() {
			panic(fmt.Sprintf("[BVV-S-04] dependency ordering violated: task %s dispatched but dep %s is %s", taskID, depID, dep.Status))
		}
	}
}

// AssertLifecycleExclusion panics if the lifecycle lock is not held.
// BVV-S-01: at most one orchestrator per branch.
func AssertLifecycleExclusion(lock *LifecycleLock, branch string) {
	if lock == nil {
		return
	}
	if !lock.IsHeld() {
		panic(fmt.Sprintf("[BVV-S-01] lifecycle exclusion violated: lock not held for branch %s", branch))
	}
}

// AssertBoundedDegradation panics if the gap count exceeds tolerance.
// BVV-S-07: degradation must be bounded.
func AssertBoundedDegradation(gaps *GapTracker, tolerance int) {
	if gaps == nil {
		return
	}
	if gaps.Count() > tolerance {
		panic(fmt.Sprintf("[BVV-S-07] bounded degradation violated: %d gaps > tolerance %d", gaps.Count(), tolerance))
	}
}
