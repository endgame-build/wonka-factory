package orch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// FSStore implements Store using filesystem JSON files with atomic writes.
//
// Layout:
//
//	{dir}/tasks/{id}.json
//	{dir}/workers/{name}.json
//	{dir}/deps.json
//	{dir}/ledger.lock
type FSStore struct {
	dir  string
	mu   sync.Mutex // goroutine-level serialisation (flock is process-level only)
	lock *flock.Flock
}

// NewFSStore creates a new filesystem-backed store. Creates directories if needed.
func NewFSStore(dir string) (*FSStore, error) {
	for _, sub := range []string{"tasks", "workers"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", sub, err)
		}
	}
	// Initialise empty deps.json if it doesn't exist.
	depsPath := filepath.Join(dir, "deps.json")
	if _, err := os.Stat(depsPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat deps.json: %w", err)
		}
		if err := atomicWriteJSON(depsPath, map[string][]string{}); err != nil {
			return nil, fmt.Errorf("init deps.json: %w", err)
		}
	}
	return &FSStore{
		dir:  dir,
		lock: flock.New(filepath.Join(dir, "ledger.lock")),
	}, nil
}

// Close is a no-op for FSStore (no persistent connections to release).
func (s *FSStore) Close() error { return nil }

// --- Task operations ---

func (s *FSStore) CreateTask(t *Task) error {
	if err := validateID(t.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.taskPath(t.ID)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("task %q: %w", t.ID, ErrTaskExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("task %q stat: %w", t.ID, err)
	}
	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now
	return atomicWriteJSON(path, t)
}

func (s *FSStore) GetTask(id string) (*Task, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	var t Task
	if err := readJSON(s.taskPath(id), &t); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("task %q: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return &t, nil
}

// UpdateTask persists the task's current state. BVV-S-02 (terminal
// irreversibility) is documented on the Store interface and enforced by the
// dispatcher, not here.
func (s *FSStore) UpdateTask(t *Task) error {
	if err := validateID(t.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.taskPath(t.ID)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("task %q: %w", t.ID, ErrNotFound)
		}
		return fmt.Errorf("update task %q stat: %w", t.ID, err)
	}
	t.UpdatedAt = time.Now()
	return atomicWriteJSON(path, t)
}

// ListTasks returns all tasks matching the given label filters, sorted by
// taskLess (LDG-07). Empty filters returns all tasks.
func (s *FSStore) ListTasks(labels ...string) ([]*Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}

	tasks, err := s.allTasks()
	if err != nil {
		return nil, err
	}

	var result []*Task
	for _, t := range tasks {
		if labelsMatch(t, labels) {
			result = append(result, t)
		}
	}
	sortTasks(result)
	return result, nil
}

// ReadyTasks returns tasks where status=open, all deps terminal, assignee
// empty, and all label filters match. Sorted by taskLess (LDG-07).
//
// BVV-DSP-01: ready = open ∧ deps-terminal ∧ unassigned ∧ labels-match.
// BVV-DSN-04: label filtering enables lifecycle scoping (branch label).
func (s *FSStore) ReadyTasks(labels ...string) ([]*Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}

	tasks, err := s.allTasks()
	if err != nil {
		return nil, err
	}

	deps, err := s.loadDeps()
	if err != nil {
		return nil, err
	}

	// Build a status map for fast lookup.
	statusMap := make(map[string]TaskStatus, len(tasks))
	for _, t := range tasks {
		statusMap[t.ID] = t.Status
	}

	var ready []*Task
	for _, t := range tasks {
		if t.Status != StatusOpen || t.Assignee != "" {
			continue
		}
		// Check all dependencies are terminal.
		allDepsDone := true
		for _, depID := range deps[t.ID] {
			st, ok := statusMap[depID]
			if !ok || !st.Terminal() {
				allDepsDone = false
				break
			}
		}
		if allDepsDone && labelsMatch(t, labels) {
			ready = append(ready, t)
		}
	}

	sortTasks(ready)
	return ready, nil
}

// Assign atomically sets a task to assigned and a worker to active.
// BVV-S-03: at most one worker per task (see BVVTaskMachine.tla Assign action).
// LDG-08: atomic task+worker update under flock.
func (s *FSStore) Assign(taskID, workerName string) error {
	if err := validateID(taskID); err != nil {
		return err
	}
	if err := validateID(workerName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	// Read task — verify preconditions.
	var task Task
	if err := readJSON(s.taskPath(taskID), &task); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("task %q: %w", taskID, ErrNotFound)
		}
		return err
	}
	if task.Assignee != "" {
		return fmt.Errorf("task %q: %w", taskID, ErrAlreadyAssigned)
	}
	if task.Status != StatusOpen {
		return fmt.Errorf("task %q status %q: %w", taskID, task.Status, ErrTaskNotReady)
	}

	// Read worker — verify idle.
	var worker Worker
	if err := readJSON(s.workerPath(workerName), &worker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("worker %q: %w", workerName, ErrNotFound)
		}
		return err
	}
	if worker.Status != WorkerIdle {
		return fmt.Errorf("worker %q: %w", workerName, ErrWorkerBusy)
	}

	// Atomic update: task + worker together under the flock (LDG-08).
	// Save original task for rollback if the worker write fails.
	origTask := task

	now := time.Now()
	task.Status = StatusAssigned
	task.Assignee = workerName
	task.UpdatedAt = now
	if err := atomicWriteJSON(s.taskPath(taskID), &task); err != nil {
		return err
	}

	worker.Status = WorkerActive
	worker.CurrentTaskID = taskID
	if err := atomicWriteJSON(s.workerPath(workerName), &worker); err != nil {
		// Rollback the task write to maintain consistency.
		if rbErr := atomicWriteJSON(s.taskPath(taskID), &origTask); rbErr != nil {
			return fmt.Errorf("assign update worker: %w (rollback failed: %v)", err, rbErr)
		}
		return fmt.Errorf("assign update worker (rolled back task): %w", err)
	}
	return nil
}

// --- Worker operations ---

func (s *FSStore) CreateWorker(w *Worker) error {
	if err := validateID(w.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.workerPath(w.Name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("worker %q: %w", w.Name, ErrWorkerExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worker %q stat: %w", w.Name, err)
	}
	return atomicWriteJSON(path, w)
}

func (s *FSStore) GetWorker(name string) (*Worker, error) {
	if err := validateID(name); err != nil {
		return nil, err
	}
	var w Worker
	if err := readJSON(s.workerPath(name), &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("worker %q: %w", name, ErrNotFound)
		}
		return nil, err
	}
	return &w, nil
}

// ListWorkers returns all workers sorted by name ASC (deterministic for tests).
func (s *FSStore) ListWorkers() ([]*Worker, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "workers"))
	if err != nil {
		return nil, err
	}
	var workers []*Worker
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var w Worker
		if err := readJSON(filepath.Join(s.dir, "workers", e.Name()), &w); err != nil {
			return nil, err
		}
		workers = append(workers, &w)
	}
	sortWorkers(workers)
	return workers, nil
}

func (s *FSStore) UpdateWorker(w *Worker) error {
	if err := validateID(w.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.workerPath(w.Name)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("worker %q: %w", w.Name, ErrNotFound)
		}
		return fmt.Errorf("update worker %q stat: %w", w.Name, err)
	}
	return atomicWriteJSON(path, w)
}

// --- Dependency operations ---

func (s *FSStore) AddDep(taskID, dependsOn string) error {
	if err := validateID(taskID); err != nil {
		return err
	}
	if err := validateID(dependsOn); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	deps, err := s.loadDeps()
	if err != nil {
		return err
	}

	if slices.Contains(deps[taskID], dependsOn) {
		return nil // idempotent
	}

	// Cycle detection: DFS from dependsOn following edges. If we reach taskID, it's a cycle (LDG-06).
	if reachable(deps, dependsOn, taskID) {
		return fmt.Errorf("adding %s→%s: %w", taskID, dependsOn, ErrCycle)
	}

	deps[taskID] = append(deps[taskID], dependsOn)
	return s.saveDeps(deps)
}

func (s *FSStore) GetDeps(taskID string) ([]string, error) {
	if err := validateID(taskID); err != nil {
		return nil, err
	}
	deps, err := s.loadDeps()
	if err != nil {
		return nil, err
	}
	return deps[taskID], nil
}

// --- Internal helpers ---

func (s *FSStore) taskPath(id string) string {
	return filepath.Join(s.dir, "tasks", id+".json")
}

func (s *FSStore) workerPath(name string) string {
	return filepath.Join(s.dir, "workers", name+".json")
}

func (s *FSStore) allTasks() ([]*Task, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "tasks"))
	if err != nil {
		return nil, err
	}
	var tasks []*Task
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var t Task
		if err := readJSON(filepath.Join(s.dir, "tasks", e.Name()), &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, &t)
	}
	return tasks, nil
}

func (s *FSStore) loadDeps() (map[string][]string, error) {
	var deps map[string][]string
	if err := readJSON(filepath.Join(s.dir, "deps.json"), &deps); err != nil {
		return nil, fmt.Errorf("load deps.json: %w", err)
	}
	if deps == nil {
		deps = make(map[string][]string)
	}
	return deps, nil
}

func (s *FSStore) saveDeps(deps map[string][]string) error {
	return atomicWriteJSON(filepath.Join(s.dir, "deps.json"), deps)
}

// reachable returns true if target is reachable from start via DFS on the adjacency list (LDG-06).
func reachable(deps map[string][]string, start, target string) bool {
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
		for _, next := range deps[node] {
			if dfs(next) {
				return true
			}
		}
		return false
	}
	return dfs(start)
}

// atomicWriteJSON marshals v to JSON and writes it atomically via tmp+rename (LDG-12).
func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmp := f.Name()
	committed := false
	defer func() {
		if !committed {
			f.Close()
			os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	// Past this point, tmp has been renamed away; tell the deferred cleanup
	// not to touch it.
	committed = true
	return nil
}

// readJSON reads a JSON file into v.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return nil
}
