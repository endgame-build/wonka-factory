// Package testutil provides test helpers for the orch package. MockStore is an
// in-memory Store implementation for fast unit tests that bypass filesystem I/O.
package testutil

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/endgame/wonka-factory/orch"
)

// MockStore implements orch.Store using in-memory maps. It mirrors FSStore's
// semantics (validateID, Assign atomicity, ReadyTasks filtering, sort order)
// so it passes RunStoreContractTests — LDG01 (durability) is satisfied by
// the test's closure-based reopen returning the same in-memory instance.
//
// Thread-safe via mutex: tests may spawn concurrent goroutines (LDG10, S03).
type MockStore struct {
	mu      sync.Mutex
	tasks   map[string]*orch.Task
	workers map[string]*orch.Worker
	deps    map[string][]string // taskID → []dependsOn

	// Error injection for testing store-failure paths. When non-nil, the
	// matching operation returns the error instead of succeeding. Each
	// independent hook lets tests pin the exact step under Reconcile that
	// surfaces a store failure. Thread-safe: guarded by mu.
	CreateTaskErr   error
	UpdateTaskErr   error
	UpdateWorkerErr error
	ListTasksErr    error
	ListWorkersErr  error
	GetTaskErr      error // applied BEFORE ErrNotFound lookup; set to ErrNotFound itself to simulate deletion
}

// Compile-time interface compliance check.
var _ orch.Store = (*MockStore)(nil)

// NewMockStore returns an empty in-memory store.
func NewMockStore() *MockStore {
	return &MockStore{
		tasks:   make(map[string]*orch.Task),
		workers: make(map[string]*orch.Worker),
		deps:    make(map[string][]string),
	}
}

// SetCreateTaskErr sets (or clears) the error returned by CreateTask.
// Thread-safe: acquires the store mutex. Used to simulate durable-store
// failures in the escalation-creation path (silent-failure audit I-3).
func (s *MockStore) SetCreateTaskErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CreateTaskErr = err
}

// SetUpdateTaskErr sets (or clears) the error returned by UpdateTask.
// Thread-safe: acquires the store mutex.
func (s *MockStore) SetUpdateTaskErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpdateTaskErr = err
}

// SetUpdateWorkerErr sets (or clears) the error returned by UpdateWorker.
// Thread-safe: acquires the store mutex.
func (s *MockStore) SetUpdateWorkerErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpdateWorkerErr = err
}

// SetListTasksErr sets (or clears) the error returned by ListTasks.
// Thread-safe: acquires the store mutex.
func (s *MockStore) SetListTasksErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ListTasksErr = err
}

// SetListWorkersErr sets (or clears) the error returned by ListWorkers.
// Thread-safe: acquires the store mutex.
func (s *MockStore) SetListWorkersErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ListWorkersErr = err
}

// SetGetTaskErr sets (or clears) the error returned by GetTask. Applies to
// every task ID — set to ErrNotFound to simulate operator deletion between
// crash and resume. Thread-safe: acquires the store mutex.
func (s *MockStore) SetGetTaskErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GetTaskErr = err
}

// clone helpers — return copies so callers can't mutate internal state.

func cloneTask(t *orch.Task) *orch.Task {
	cp := *t
	if t.Labels != nil {
		cp.Labels = make(map[string]string, len(t.Labels))
		for k, v := range t.Labels {
			cp.Labels[k] = v
		}
	}
	return &cp
}

func cloneWorker(w *orch.Worker) *orch.Worker {
	cp := *w
	return &cp
}

// --- Task operations ---

func (s *MockStore) CreateTask(t *orch.Task) error {
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.CreateTaskErr != nil {
		return s.CreateTaskErr
	}
	if _, exists := s.tasks[t.ID]; exists {
		return fmt.Errorf("task %q: %w", t.ID, orch.ErrTaskExists)
	}
	cp := cloneTask(t)
	now := time.Now()
	cp.CreatedAt = now
	cp.UpdatedAt = now
	s.tasks[t.ID] = cp
	return nil
}

func (s *MockStore) GetTask(id string) (*orch.Task, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.GetTaskErr != nil {
		return nil, s.GetTaskErr
	}
	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("task %q: %w", id, orch.ErrNotFound)
	}
	return cloneTask(t), nil
}

func (s *MockStore) UpdateTask(t *orch.Task) error {
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.UpdateTaskErr != nil {
		return s.UpdateTaskErr
	}
	if _, ok := s.tasks[t.ID]; !ok {
		return fmt.Errorf("task %q: %w", t.ID, orch.ErrNotFound)
	}
	cp := cloneTask(t)
	cp.UpdatedAt = time.Now()
	s.tasks[t.ID] = cp
	return nil
}

func (s *MockStore) ListTasks(labels ...string) ([]*orch.Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ListTasksErr != nil {
		return nil, s.ListTasksErr
	}
	var result []*orch.Task
	for _, t := range s.tasks {
		if labelsMatch(t, labels) {
			result = append(result, cloneTask(t))
		}
	}
	sortTasks(result)
	return result, nil
}

func (s *MockStore) ReadyTasks(labels ...string) ([]*orch.Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var ready []*orch.Task
	for _, t := range s.tasks {
		if t.Status != orch.StatusOpen || t.Assignee != "" {
			continue
		}
		allDepsDone := true
		for _, depID := range s.deps[t.ID] {
			dep, ok := s.tasks[depID]
			if !ok || !dep.Status.Terminal() {
				allDepsDone = false
				break
			}
		}
		if allDepsDone && labelsMatch(t, labels) {
			ready = append(ready, cloneTask(t))
		}
	}
	sortTasks(ready)
	return ready, nil
}

func (s *MockStore) Assign(taskID, workerName string) error {
	if err := ValidateID(taskID); err != nil {
		return err
	}
	if err := ValidateID(workerName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q: %w", taskID, orch.ErrNotFound)
	}
	if task.Assignee != "" {
		return fmt.Errorf("task %q: %w", taskID, orch.ErrAlreadyAssigned)
	}
	if task.Status != orch.StatusOpen {
		return fmt.Errorf("task %q status %q: %w", taskID, task.Status, orch.ErrTaskNotReady)
	}

	worker, ok := s.workers[workerName]
	if !ok {
		return fmt.Errorf("worker %q: %w", workerName, orch.ErrNotFound)
	}
	if worker.Status != orch.WorkerIdle {
		return fmt.Errorf("worker %q: %w", workerName, orch.ErrWorkerBusy)
	}

	// Atomic update: both task and worker transition together.
	task.Status = orch.StatusAssigned
	task.Assignee = workerName
	task.UpdatedAt = time.Now()

	worker.Status = orch.WorkerActive
	worker.CurrentTaskID = taskID
	return nil
}

// --- Worker operations ---

func (s *MockStore) CreateWorker(w *orch.Worker) error {
	if err := ValidateID(w.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.workers[w.Name]; exists {
		return fmt.Errorf("worker %q: %w", w.Name, orch.ErrWorkerExists)
	}
	s.workers[w.Name] = cloneWorker(w)
	return nil
}

func (s *MockStore) GetWorker(name string) (*orch.Worker, error) {
	if err := ValidateID(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workers[name]
	if !ok {
		return nil, fmt.Errorf("worker %q: %w", name, orch.ErrNotFound)
	}
	return cloneWorker(w), nil
}

func (s *MockStore) ListWorkers() ([]*orch.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ListWorkersErr != nil {
		return nil, s.ListWorkersErr
	}
	var result []*orch.Worker
	for _, w := range s.workers {
		result = append(result, cloneWorker(w))
	}
	sortWorkers(result)
	return result, nil
}

func (s *MockStore) UpdateWorker(w *orch.Worker) error {
	if err := ValidateID(w.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.UpdateWorkerErr != nil {
		return s.UpdateWorkerErr
	}
	if _, ok := s.workers[w.Name]; !ok {
		return fmt.Errorf("worker %q: %w", w.Name, orch.ErrNotFound)
	}
	s.workers[w.Name] = cloneWorker(w)
	return nil
}

// --- Dependency operations ---

func (s *MockStore) AddDep(taskID, dependsOn string) error {
	if err := ValidateID(taskID); err != nil {
		return err
	}
	if err := ValidateID(dependsOn); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotent.
	if slices.Contains(s.deps[taskID], dependsOn) {
		return nil
	}

	// Cycle detection: DFS from dependsOn. If we reach taskID, it's a cycle.
	if s.reachable(dependsOn, taskID) {
		return fmt.Errorf("adding %s→%s: %w", taskID, dependsOn, orch.ErrCycle)
	}

	s.deps[taskID] = append(s.deps[taskID], dependsOn)
	return nil
}

// InjectDep appends a dependency edge WITHOUT running AddDep's cycle check.
// Test-only: used by BVV-TG-08 spec tests that need to construct cyclic
// graphs to verify ValidateLifecycleGraph catches raw-DB tampering that
// legitimate AddDep would reject. Do not use in production code paths.
func (s *MockStore) InjectDep(taskID, dependsOn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.deps[taskID], dependsOn) {
		return
	}
	s.deps[taskID] = append(s.deps[taskID], dependsOn)
}

func (s *MockStore) GetDeps(taskID string) ([]string, error) {
	if err := ValidateID(taskID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Return a copy so callers can't mutate our internal slice.
	orig := s.deps[taskID]
	if len(orig) == 0 {
		return nil, nil
	}
	cp := make([]string, len(orig))
	copy(cp, orig)
	return cp, nil
}

func (s *MockStore) Close() error { return nil }

// --- Internal helpers ---

// reachable performs DFS from start following dep edges. Returns true if target
// is reachable. Must be called under s.mu.
func (s *MockStore) reachable(start, target string) bool {
	visited := make(map[string]bool)
	var dfs func(node string) bool
	dfs = func(node string) bool {
		if node == target {
			return true
		}
		if visited[node] {
			return false
		}
		visited[node] = true
		for _, next := range s.deps[node] {
			if dfs(next) {
				return true
			}
		}
		return false
	}
	return dfs(start)
}

// ValidateID rejects identifiers with path traversal characters.
// Mirrors orch.validateID (unexported) using stdlib string functions.
func ValidateID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || strings.Contains(id, "..") {
		return fmt.Errorf("id %q: %w", id, orch.ErrInvalidID)
	}
	return nil
}

// validateLabelFilters checks "key:value" format.
// Mirrors orch.validateLabelFilters (unexported).
func validateLabelFilters(filters []string) error {
	for _, f := range filters {
		if !strings.Contains(f, ":") {
			return fmt.Errorf("%w: %q", orch.ErrInvalidLabelFilter, f)
		}
	}
	return nil
}

// labelsMatch returns true if the task's Labels contain every filter.
// Mirrors orch.labelsMatch (unexported).
func labelsMatch(t *orch.Task, filters []string) bool {
	for _, f := range filters {
		k, v, _ := strings.Cut(f, ":")
		if t.Labels[k] != v {
			return false
		}
	}
	return true
}

// sortTasks sorts by priority ASC then ID ASC. Mirrors orch.sortTasks (unexported).
func sortTasks(tasks []*orch.Task) {
	slices.SortFunc(tasks, func(a, b *orch.Task) int {
		if a.Priority != b.Priority {
			return cmp.Compare(a.Priority, b.Priority)
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// sortWorkers sorts by name ASC. Mirrors orch.sortWorkers (unexported).
func sortWorkers(ws []*orch.Worker) {
	slices.SortFunc(ws, func(a, b *orch.Worker) int { return cmp.Compare(a.Name, b.Name) })
}
