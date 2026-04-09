//go:build verify

package orch

import "fmt"

// Runtime assertions for formal spec verification.
// Enabled with -tags verify. Panic on violation with requirement ID.

// AssertUniqueOutputs verifies no two tasks share an output path [WFC-01 post-expansion].
func AssertUniqueOutputs(tasks []*Task) {
	seen := make(map[string]string) // output → task ID
	for _, t := range tasks {
		if t.Output == "" {
			continue
		}
		if prev, ok := seen[t.Output]; ok {
			panic(fmt.Sprintf("[WFC-01] duplicate output %q: tasks %q and %q", t.Output, prev, t.ID))
		}
		seen[t.Output] = t.ID
	}
}

// AssertPostAssign verifies assignment preconditions held [LDG-08, LDG-14].
func AssertPostAssign(task *Task, worker *Worker) {
	if task.Status != StatusAssigned {
		panic(fmt.Sprintf("[LDG-08] post-assign: task %q status is %q, expected assigned", task.ID, task.Status))
	}
	if task.Assignee != worker.Name {
		panic(fmt.Sprintf("[LDG-08] post-assign: task %q assignee is %q, expected %q", task.ID, task.Assignee, worker.Name))
	}
	if worker.CurrentTaskID != task.ID {
		panic(fmt.Sprintf("[LDG-08] post-assign: worker %q current_task_id is %q, expected %q", worker.Name, worker.CurrentTaskID, task.ID))
	}
}

// AssertGapBound verifies gaps < tolerance while pipeline is running [S5].
func AssertGapBound(gaps, tolerance int) {
	if gaps >= tolerance {
		panic(fmt.Sprintf("[S5] gaps (%d) >= gap_tolerance (%d) while pipeline running", gaps, tolerance))
	}
}

// AssertSingleLockHolder verifies at most one lock holder [S7].
func AssertSingleLockHolder(holders []string) {
	active := 0
	for _, h := range holders {
		if h != "" {
			active++
		}
	}
	if active > 1 {
		panic(fmt.Sprintf("[S7] multiple lock holders: %v", holders))
	}
}

// AssertWorkerConservation verifies idle + active = total workers [WC].
// The TLA+ model found that without this invariant, the watchdog and
// environment can double-decrement active workers.
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
		panic(fmt.Sprintf("[WC] worker count %d (idle=%d, active=%d) exceeds max %d",
			total, idle, active, maxWorkers))
	}
}

// AssertNoTaskStatusByWatchdog verifies the watchdog never changes task status [SUP-04, S11].
// Call with task snapshots before and after a watchdog cycle.
func AssertNoTaskStatusByWatchdog(before, after []*Task) {
	statusMap := make(map[string]TaskStatus, len(before))
	for _, t := range before {
		statusMap[t.ID] = t.Status
	}
	for _, t := range after {
		if prev, ok := statusMap[t.ID]; ok && prev != t.Status {
			panic(fmt.Sprintf("[SUP-04] watchdog changed task %q status from %q to %q",
				t.ID, prev, t.Status))
		}
	}
}
