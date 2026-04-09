package orch

import (
	"fmt"
	"strings"
)

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
// Ported verbatim from _fork/ledger_fs.go:314-320.
func taskLess(a, b *Task) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.ID < b.ID
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
