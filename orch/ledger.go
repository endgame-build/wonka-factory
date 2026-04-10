package orch

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// validateID rejects identifiers that could escape filesystem directories via
// path traversal. Task IDs come from the planning agent (Charlie) and worker
// names from CLI configuration — both are external inputs.
func validateID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || strings.Contains(id, "..") {
		return fmt.Errorf("id %q: %w", id, ErrInvalidID)
	}
	return nil
}

// Store is the assignment ledger interface consumed by the dispatcher, engine,
// and recovery subsystems. Implementations must be safe for concurrent use
// from a single orchestrator process.
//
// BVV-DSP-16: the default implementation is Beads (Dolt-backed); FS is the fallback.
// BVV-DSN-04: the interface is phase-agnostic — no method references lifecycle phases.
//
// Label filter semantics (ReadyTasks, ListTasks):
//   - Variadic "key:value" strings; results AND-match all filters.
//   - Empty variadic = no filter (return all matches to other predicates).
//   - Malformed filter (missing ":") returns ErrInvalidLabelFilter.
//
// Ordering contract (LDG-07):
//   - ReadyTasks and ListTasks return results sorted by taskLess
//     (priority ASC — lower number = higher priority, then ID ASC).
//   - ListWorkers returns workers sorted by name ASC.
//
// UpdateTask is a dumb writer: implementations must NOT enforce BVV-S-02
// (terminal irreversibility). The dispatcher enforces that invariant via
// invariant.go (Phase 4).
type Store interface {
	CreateTask(t *Task) error
	GetTask(id string) (*Task, error)
	UpdateTask(t *Task) error
	ListTasks(labels ...string) ([]*Task, error)
	ReadyTasks(labels ...string) ([]*Task, error)
	Assign(taskID, workerName string) error

	CreateWorker(w *Worker) error
	GetWorker(name string) (*Worker, error)
	ListWorkers() ([]*Worker, error)
	UpdateWorker(w *Worker) error

	AddDep(taskID, dependsOn string) error
	GetDeps(taskID string) ([]string, error)

	Close() error
}

// taskLess is the canonical sort order: priority ascending (lower number =
// dispatched first), then lexicographic ID. Deterministic per LDG-07.
func taskLess(a, b *Task) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.ID < b.ID
}

// sortTasks sorts a task slice in-place by the canonical LDG-07 ordering.
func sortTasks(tasks []*Task) {
	slices.SortFunc(tasks, func(a, b *Task) int {
		if a.Priority != b.Priority {
			return cmp.Compare(a.Priority, b.Priority)
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// sortWorkers sorts a worker slice in-place by name ASC. Shared by both
// Store backends for the ListWorkers ordering contract.
func sortWorkers(ws []*Worker) {
	slices.SortFunc(ws, func(a, b *Worker) int { return cmp.Compare(a.Name, b.Name) })
}

// validateLabelFilters checks that all filter strings are in "key:value" format.
// Returns ErrInvalidLabelFilter wrapping the first malformed filter.
func validateLabelFilters(filters []string) error {
	for _, f := range filters {
		if !strings.Contains(f, ":") {
			return fmt.Errorf("%w: %q", ErrInvalidLabelFilter, f)
		}
	}
	return nil
}

// labelsMatch returns true if the task's Labels map contains every filter.
// Each filter must be "key:value"; the caller must validate via
// validateLabelFilters before calling this function.
func labelsMatch(t *Task, filters []string) bool {
	for _, f := range filters {
		k, v, _ := strings.Cut(f, ":")
		if t.Labels[k] != v {
			return false
		}
	}
	return true
}
