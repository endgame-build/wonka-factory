package orch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	if _, err := os.Stat(depsPath); errors.Is(err, os.ErrNotExist) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.taskPath(t.ID)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("task %q: %w", t.ID, ErrTaskExists)
	}
	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now
	return atomicWriteJSON(path, t)
}

func (s *FSStore) GetTask(id string) (*Task, error) {
	var t Task
	if err := readJSON(s.taskPath(id), &t); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("task %q: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return &t, nil
}

func (s *FSStore) UpdateTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.taskPath(t.ID)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("task %q: %w", t.ID, ErrNotFound)
	}
	t.UpdatedAt = time.Now()
	return atomicWriteJSON(path, t)
}

func (s *FSStore) GetChildren(parentID string) ([]*Task, error) {
	tasks, err := s.allTasks()
	if err != nil {
		return nil, err
	}
	var children []*Task
	for _, t := range tasks {
		if t.ParentID == parentID {
			children = append(children, t)
		}
	}
	sort.Slice(children, func(i, j int) bool { return taskLess(children[i], children[j]) })
	return children, nil
}

func (s *FSStore) ReadyTasks() ([]*Task, error) {
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
		if allDepsDone {
			ready = append(ready, t)
		}
	}

	sort.Slice(ready, func(i, j int) bool { return taskLess(ready[i], ready[j]) })
	return ready, nil
}

func (s *FSStore) Assign(taskID, workerName string) error {
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
		_ = atomicWriteJSON(s.taskPath(taskID), &origTask) //nolint:errcheck // best-effort rollback
		return err
	}
	return nil
}

// --- Worker operations ---

func (s *FSStore) CreateWorker(w *Worker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.workerPath(w.Name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("worker %q already exists", w.Name)
	}
	return atomicWriteJSON(path, w)
}

func (s *FSStore) GetWorker(name string) (*Worker, error) {
	var w Worker
	if err := readJSON(s.workerPath(name), &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("worker %q: %w", name, ErrNotFound)
		}
		return nil, err
	}
	return &w, nil
}

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
	return workers, nil
}

func (s *FSStore) UpdateWorker(w *Worker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer s.lock.Unlock() //nolint:errcheck // flock.Unlock error is always nil

	path := s.workerPath(w.Name)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worker %q: %w", w.Name, ErrNotFound)
	}
	return atomicWriteJSON(path, w)
}

// --- Dependency operations ---

func (s *FSStore) AddDep(taskID, dependsOn string) error {
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

	// Check for duplicate.
	for _, d := range deps[taskID] {
		if d == dependsOn {
			return nil // idempotent
		}
	}

	// Cycle detection: DFS from dependsOn following edges. If we reach taskID, it's a cycle.
	if reachable(deps, dependsOn, taskID) {
		return fmt.Errorf("adding %s→%s: %w", taskID, dependsOn, ErrCycle)
	}

	deps[taskID] = append(deps[taskID], dependsOn)
	return s.saveDeps(deps)
}

func (s *FSStore) GetDeps(taskID string) ([]string, error) {
	deps, err := s.loadDeps()
	if err != nil {
		return nil, err
	}
	return deps[taskID], nil
}

// taskLess is the canonical sort order: priority ascending, then lexicographic ID (LDG-07).
func taskLess(a, b *Task) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.ID < b.ID
}

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

// reachable returns true if target is reachable from start via DFS on the adjacency list.
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

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// readJSON reads a JSON file into v.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
