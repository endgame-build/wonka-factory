//go:build verify

package orch

// TLA+ Traceability Matrix
//
// Each runtime assertion below corresponds to a TLA+ operator (or set of
// operators) in the formal model under docs/specs/tla/. Line numbers
// reference BVV.tla unless otherwise noted. Update this matrix whenever an
// assertion is added, renamed, or removed. The matrix is intentionally
// grep-parseable: scripts/trace-requirement.sh keys off the requirement IDs
// in the rightmost column to surface the spec → model → assertion chain.
//
//   Go Assertion                       TLA+ Operator             BVV.tla  Req ID
//   ----------------------------       ----------------------    -------  -----------------
//   AssertLifecycleExclusion           LifecycleExclusion        :295     BVV-S-01
//   AssertTerminalIrreversibility      TerminalIrreversibility   :307     BVV-S-02
//   AssertSingleAssignment             SingleAssignment          :321     BVV-S-03
//   AssertDependencyOrdering           DependencyOrdering        :327     BVV-S-04
//   AssertZeroContentInspection        (by construction)         —        BVV-S-05
//   AssertBoundedDegradation           BoundedDegradation        :344     BVV-S-07
//   AssertWatchdogNoStatusChange       (by construction)         —        BVV-S-10
//   AssertLifecycleReleaseDrained      (by construction)         —        BVV-ERR-10a
//   AssertWorkerConservation           TypeOK (worker/session)   :268     WC
//   AssertPostPlannerWellFormed        (no TLA+ counterpart)     —        BVV-TG-07..10

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

// AssertLifecycleReleaseDrained panics if any worker is still active at
// voluntary lock release time. BVV-ERR-10a: all sessions must be drained
// before the orchestrator releases the lifecycle lock.
func AssertLifecycleReleaseDrained(store Store) {
	if busy := CheckReleaseDrained(store); len(busy) > 0 {
		panic(fmt.Sprintf("[BVV-ERR-10a] release with active workers: %v", busy))
	}
}

// AssertZeroContentInspection panics if the role used to dispatch a task was
// not derived from its labels. BVV-S-05: routing uses task metadata only —
// the orchestrator never inspects task.Body (agent-owned) for routing.
//
// Called immediately before SpawnSession. The check is a tautology for
// correct callers — it compares the resolved role against task.Role(), which
// reads from Labels. The value of the guard is that any regression that
// resolves a role from task.Body or another content source will produce a
// (resolvedRole != task.Role()) mismatch and panic. The second check
// (resolvedRole != "") catches callers that dispatch without a real role
// decision (e.g. defaulting instead of escalating on unknown role per
// BVV-DSP-03a).
func AssertZeroContentInspection(task *Task, resolvedRole Role) {
	if task == nil {
		panic("[BVV-S-05] zero content inspection: nil task")
	}
	if resolvedRole == "" {
		panic(fmt.Sprintf("[BVV-S-05] zero content inspection: empty role for task %q (label path bypassed)", task.ID))
	}
	if got := task.Role(); got != resolvedRole {
		panic(fmt.Sprintf("[BVV-S-05] zero content inspection: task %q routed as %q but label role is %q", task.ID, resolvedRole, got))
	}
}

// AssertWorkerConservation panics if idle + active exceeds maxWorkers.
// Ported from the fork: the TLA+ model showed double-decrement races
// between watchdog and dispatch can corrupt pool accounting.
func AssertWorkerConservation(workers []*Worker, maxWorkers int) {
	idle, active := 0, 0
	for _, w := range workers {
		switch w.Status {
		case WorkerIdle:
			idle++
		case WorkerActive:
			active++
		}
	}
	total := idle + active
	if total > maxWorkers {
		panic(fmt.Sprintf("[WC] worker count %d (idle=%d, active=%d) exceeds max %d", total, idle, active, maxWorkers))
	}
}

// AssertWatchdogNoStatusChange panics if any task's status differs between
// before and after snapshots. BVV-S-10: the watchdog must never mutate task
// status — it emits events and manipulates HandoffState only.
//
// Single-threaded helper: this is a snapshot-diff that attributes ANY
// observed status change to the caller. It is unsafe to use across a
// window in which a concurrent writer (e.g. the dispatch goroutine)
// might legitimately transition tasks — false positives are guaranteed
// in that case. In production, BVV-S-10 is enforced structurally inside
// Watchdog.CheckOnce (no direct task-status Store calls in the watchdog
// body) and dynamically by AssertTerminalIrreversibility inside
// SpawnSession (which catches any race where the dispatcher transitioned
// the task to terminal between the watchdog's Terminal() check and the
// indirect StatusInProgress write). This function is retained for spec
// tests that exercise the property in a controlled, single-writer setup.
func AssertWatchdogNoStatusChange(before, after []*Task) {
	statusMap := make(map[string]TaskStatus, len(before))
	for _, t := range before {
		statusMap[t.ID] = t.Status
	}
	for _, t := range after {
		if prev, ok := statusMap[t.ID]; ok && prev != t.Status {
			panic(fmt.Sprintf("[BVV-S-10] watchdog changed task %q status from %q to %q", t.ID, prev, t.Status))
		}
	}
}

// guardWorkerConservation loads workers from the store and asserts WC. It
// exists so call sites outside the pool that don't already have a workers
// slice (dispatch Tick, WorkerPool.Release) incur zero I/O in non-verify
// builds — the noverify stub does nothing.
func guardWorkerConservation(store Store, maxWorkers int) {
	if workers, err := store.ListWorkers(); err == nil {
		AssertWorkerConservation(workers, maxWorkers)
	}
}

// AssertPostPlannerWellFormed panics if the post-planner task graph for the
// given branch fails any of BVV-TG-07..10. Called from Engine.onPlannerCompleted
// immediately after ValidateLifecycleGraph returns nil.
//
// This is a re-execution of the same validator the engine just ran — so a
// buggy validator breaks both call sites together. The assertion's value is
// not catching validator regressions; it is:
//
//  1. surfacing graph violations as a grep-traceable panic ([BVV-TG-07..10])
//     for any future code path that calls it without first checking the err;
//  2. providing a named, independently-testable contract that tests can pin
//     the post-planner invariant against without reaching into engine state;
//  3. offering a stable assertion API for future call sites (admin tools,
//     watchdog extensions) that need to re-verify the invariant without
//     re-implementing the validator's traversal.
func AssertPostPlannerWellFormed(store Store, branch string, roles map[string]RoleConfig) {
	if err := ValidateLifecycleGraph(store, branch, roles); err != nil {
		panic(fmt.Sprintf("[BVV-TG-07..10] post-planner well-formedness violated: %v", err))
	}
}
