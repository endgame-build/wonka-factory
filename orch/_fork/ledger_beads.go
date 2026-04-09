package orch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	beads "github.com/steveyegge/beads"
)

// beadsNotFound returns true if the error indicates a missing issue.
// The Beads SDK does not export sentinel errors, so we match by message substring.
// TODO: switch to errors.Is() when the SDK adds exported sentinels — substring
// matching is fragile if error wording changes or task IDs contain "not found".
func beadsNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// beadsDuplicate returns true if the error indicates a duplicate issue.
// Same fragility caveat as beadsNotFound.
func beadsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "UNIQUE constraint")
}

// BeadsStore implements Store using the Beads issue tracker SDK for tasks and
// dependencies (Dolt-backed with native dependency-aware ready queries) and
// filesystem JSON for workers (which have no Beads analog).
type BeadsStore struct {
	storage   beads.Storage
	workerDir string
	mu        sync.Mutex // goroutine-level serialisation
}

// NewBeadsStore creates a Store backed by a Beads Dolt database in dir.
// Workers are stored as JSON files under {dir}/workers/.
func NewBeadsStore(dir string) (*BeadsStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create beads dir: %w", err)
	}
	workerDir := filepath.Join(dir, "workers")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		return nil, fmt.Errorf("create worker dir: %w", err)
	}

	// beads.Open opens in embedded Dolt mode (creates DB if missing).
	// The Dolt database is created at {dir}/dolt/.
	dbPath := filepath.Join(dir, "dolt")
	storage, err := beads.Open(context.Background(), dbPath)
	if err != nil {
		return nil, fmt.Errorf("open beads storage: %w", err)
	}

	return &BeadsStore{
		storage:   storage,
		workerDir: workerDir,
	}, nil
}

// Close releases the underlying Beads database connection.
func (b *BeadsStore) Close() error {
	return b.storage.Close()
}

// --- Label key conventions ---
//
// Orch-specific Task fields that have no native Issue equivalent are stored
// as labels with an "orch:" prefix:
//
//   orch:parent={parentID}
//   orch:type={taskType}
//   orch:agent={agentID}
//   orch:output={outputPath}
//   orch:failed                  (present when status is StatusFailed)
//   orch:assigned                (present when status is StatusAssigned)

const (
	labelPrefix   = "orch:"
	labelParent   = "orch:parent="
	labelType     = "orch:type="
	labelAgent    = "orch:agent="
	labelOutput   = "orch:output="
	labelFailed   = "orch:failed"
	labelAssigned = "orch:assigned"
)

// --- Task ↔ Issue mapping ---

func (b *BeadsStore) toIssue(t *Task) *beads.Issue {
	return &beads.Issue{
		ID:        t.ID,
		Title:     t.ID,
		Status:    orchStatusToBeads(t.Status),
		Priority:  t.Priority,
		IssueType: beads.TypeTask,
		Assignee:  t.Assignee,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func (b *BeadsStore) toTask(issue *beads.Issue, labels []string) *Task {
	t := &Task{
		ID:        issue.ID,
		Status:    beadsStatusToOrch(issue.Status, labels),
		Priority:  issue.Priority,
		Assignee:  issue.Assignee,
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.UpdatedAt,
	}
	for _, l := range labels {
		switch {
		case strings.HasPrefix(l, labelParent):
			t.ParentID = strings.TrimPrefix(l, labelParent)
		case strings.HasPrefix(l, labelType):
			t.Type = TaskType(strings.TrimPrefix(l, labelType))
		case strings.HasPrefix(l, labelAgent):
			t.AgentID = strings.TrimPrefix(l, labelAgent)
		case strings.HasPrefix(l, labelOutput):
			t.Output = strings.TrimPrefix(l, labelOutput)
		}
	}
	return t
}

func orchStatusToBeads(s TaskStatus) beads.Status {
	switch s {
	case StatusOpen:
		return beads.StatusOpen
	case StatusAssigned:
		return beads.StatusOpen // distinguished by labelAssigned
	case StatusInProgress:
		return beads.StatusInProgress
	case StatusCompleted:
		return beads.StatusClosed
	case StatusFailed:
		return beads.StatusClosed // distinguished by labelFailed
	default:
		return beads.StatusOpen
	}
}

func beadsStatusToOrch(s beads.Status, labels []string) TaskStatus {
	hasLabel := func(target string) bool {
		for _, l := range labels {
			if l == target {
				return true
			}
		}
		return false
	}
	switch s {
	case beads.StatusOpen:
		if hasLabel(labelAssigned) {
			return StatusAssigned
		}
		return StatusOpen
	case beads.StatusInProgress:
		return StatusInProgress
	case beads.StatusClosed:
		if hasLabel(labelFailed) {
			return StatusFailed
		}
		return StatusCompleted
	default:
		return StatusOpen
	}
}

// orchLabels returns the orch-specific labels to store for a Task.
func orchLabels(t *Task) []string {
	var labels []string
	if t.ParentID != "" {
		labels = append(labels, labelParent+t.ParentID)
	}
	if t.Type != "" {
		labels = append(labels, labelType+string(t.Type))
	}
	if t.AgentID != "" {
		labels = append(labels, labelAgent+t.AgentID)
	}
	if t.Output != "" {
		labels = append(labels, labelOutput+t.Output)
	}
	if t.Status == StatusFailed {
		labels = append(labels, labelFailed)
	}
	if t.Status == StatusAssigned {
		labels = append(labels, labelAssigned)
	}
	return labels
}

// --- Store interface: Task operations ---

func (b *BeadsStore) CreateTask(t *Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now

	return b.storage.RunInTransaction(context.Background(), "orch", func(tx beads.Transaction) error {
		if err := tx.CreateIssue(context.Background(), b.toIssue(t), "orch"); err != nil {
			if beadsDuplicate(err) {
				return fmt.Errorf("task %q: %w", t.ID, ErrTaskExists)
			}
			return fmt.Errorf("create task: %w", err)
		}
		for _, label := range orchLabels(t) {
			if err := tx.AddLabel(context.Background(), t.ID, label, "orch"); err != nil {
				return fmt.Errorf("add label %s: %w", label, err)
			}
		}
		return nil
	})
}

func (b *BeadsStore) GetTask(id string) (*Task, error) {
	issue, err := b.storage.GetIssue(context.Background(), id)
	if err != nil {
		if beadsNotFound(err) {
			return nil, fmt.Errorf("get task %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get task: %w", err)
	}
	labels, err := b.storage.GetLabels(context.Background(), id)
	if err != nil {
		return nil, fmt.Errorf("get labels: %w", err)
	}
	return b.toTask(issue, labels), nil
}

func (b *BeadsStore) UpdateTask(t *Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, err := b.storage.GetIssue(context.Background(), t.ID); err != nil {
		if beadsNotFound(err) {
			return fmt.Errorf("update task %s: %w", t.ID, ErrNotFound)
		}
		return fmt.Errorf("update task check: %w", err)
	}

	t.UpdatedAt = time.Now()

	return b.storage.RunInTransaction(context.Background(), "orch", func(tx beads.Transaction) error {
		updates := map[string]any{
			"status":   orchStatusToBeads(t.Status),
			"priority": t.Priority,
			"assignee": t.Assignee,
		}
		if err := tx.UpdateIssue(context.Background(), t.ID, updates, "orch"); err != nil {
			return fmt.Errorf("update issue: %w", err)
		}

		oldLabels, err := tx.GetLabels(context.Background(), t.ID)
		if err != nil {
			return fmt.Errorf("get old labels for %s: %w", t.ID, err)
		}
		for _, l := range oldLabels {
			if strings.HasPrefix(l, labelPrefix) {
				if err := tx.RemoveLabel(context.Background(), t.ID, l, "orch"); err != nil {
					return fmt.Errorf("remove stale label %s for %s: %w", l, t.ID, err)
				}
			}
		}
		for _, label := range orchLabels(t) {
			if err := tx.AddLabel(context.Background(), t.ID, label, "orch"); err != nil {
				return fmt.Errorf("add label %s: %w", label, err)
			}
		}
		return nil
	})
}

func (b *BeadsStore) GetChildren(parentID string) ([]*Task, error) {
	issues, err := b.storage.GetIssuesByLabel(context.Background(), labelParent+parentID)
	if err != nil {
		return nil, fmt.Errorf("get children: %w", err)
	}

	children := make([]*Task, 0, len(issues))
	for _, issue := range issues {
		labels, err := b.storage.GetLabels(context.Background(), issue.ID)
		if err != nil {
			return nil, fmt.Errorf("get labels for %s: %w", issue.ID, err)
		}
		children = append(children, b.toTask(issue, labels))
	}
	sort.Slice(children, func(i, j int) bool { return taskLess(children[i], children[j]) })
	return children, nil
}

// ReadyTasks returns tasks where status=open, all deps are terminal,
// and assignee is empty. Results are sorted by priority then ID (LDG-07).
func (b *BeadsStore) ReadyTasks() ([]*Task, error) {
	// Use Beads' native dependency-aware ready query.
	filter := beads.WorkFilter{
		Status:     beads.StatusOpen,
		Unassigned: true,
	}
	issues, err := b.storage.GetReadyWork(context.Background(), filter)
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}

	// Fetch labels for each issue and filter out "assigned" tasks
	// (which are beads open but orch-assigned).
	tasks := make([]*Task, 0, len(issues))
	for _, issue := range issues {
		labels, err := b.storage.GetLabels(context.Background(), issue.ID)
		if err != nil {
			return nil, fmt.Errorf("get labels for %s: %w", issue.ID, err)
		}
		t := b.toTask(issue, labels)
		// Exclude orch-assigned tasks (they're beads-open but not orch-ready).
		if t.Status == StatusAssigned {
			continue
		}
		if t.Assignee != "" {
			continue
		}
		tasks = append(tasks, t)
	}

	// Re-sort to guarantee LDG-07 ordering (priority asc, then lexicographic ID).
	sort.Slice(tasks, func(i, j int) bool { return taskLess(tasks[i], tasks[j]) })
	return tasks, nil
}

// Assign atomically sets task.Status=assigned, task.Assignee=workerName,
// and worker.CurrentTaskID=taskID (LDG-08).
func (b *BeadsStore) Assign(taskID, workerName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	issue, err := b.storage.GetIssue(context.Background(), taskID)
	if err != nil {
		if beadsNotFound(err) {
			return fmt.Errorf("assign task %s: %w", taskID, ErrNotFound)
		}
		return fmt.Errorf("assign get task: %w", err)
	}
	labels, err := b.storage.GetLabels(context.Background(), taskID)
	if err != nil {
		return fmt.Errorf("assign get labels: %w", err)
	}
	task := b.toTask(issue, labels)

	if task.Assignee != "" {
		return fmt.Errorf("assign %s: %w", taskID, ErrAlreadyAssigned)
	}
	if task.Status != StatusOpen {
		return fmt.Errorf("assign %s: %w", taskID, ErrTaskNotReady)
	}

	worker, err := b.getWorker(workerName)
	if err != nil {
		return err
	}
	if worker.Status != WorkerIdle {
		return fmt.Errorf("assign to %s: %w", workerName, ErrWorkerBusy)
	}

	if err := b.storage.RunInTransaction(context.Background(), "orch", func(tx beads.Transaction) error {
		updates := map[string]any{
			"assignee": workerName,
		}
		if err := tx.UpdateIssue(context.Background(), taskID, updates, "orch"); err != nil {
			return err
		}
		if err := tx.AddLabel(context.Background(), taskID, labelAssigned, "orch"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("assign update task: %w", err)
	}

	worker.Status = WorkerActive
	worker.CurrentTaskID = taskID
	if err := atomicWriteJSON(b.workerPath(workerName), worker); err != nil {
		// Rollback task assignment on worker write failure.
		rbErr := b.storage.RunInTransaction(context.Background(), "orch", func(tx beads.Transaction) error {
			if e := tx.UpdateIssue(context.Background(), taskID, map[string]any{"assignee": ""}, "orch"); e != nil {
				return fmt.Errorf("rollback assignee: %v", e)
			}
			if e := tx.RemoveLabel(context.Background(), taskID, labelAssigned, "orch"); e != nil {
				return fmt.Errorf("rollback label: %v", e)
			}
			return nil
		})
		if rbErr != nil {
			return fmt.Errorf("assign update worker: %w (rollback failed: %v)", err, rbErr)
		}
		return fmt.Errorf("assign update worker: %w", err)
	}
	return nil
}

// --- Store interface: Worker operations (filesystem JSON) ---

func (b *BeadsStore) CreateWorker(w *Worker) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.workerPath(w.Name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("create worker %s: worker already exists", w.Name)
	}
	return atomicWriteJSON(path, w)
}

func (b *BeadsStore) GetWorker(name string) (*Worker, error) {
	return b.getWorker(name)
}

func (b *BeadsStore) getWorker(name string) (*Worker, error) {
	path := b.workerPath(name)
	var w Worker
	if err := readJSON(path, &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("get worker %s: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("get worker: %w", err)
	}
	return &w, nil
}

func (b *BeadsStore) ListWorkers() ([]*Worker, error) {
	entries, err := os.ReadDir(b.workerDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list workers: %w", err)
	}
	var workers []*Worker
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var w Worker
		if err := readJSON(filepath.Join(b.workerDir, entry.Name()), &w); err != nil {
			return nil, fmt.Errorf("list workers: read %s: %w", entry.Name(), err)
		}
		workers = append(workers, &w)
	}
	return workers, nil
}

func (b *BeadsStore) UpdateWorker(w *Worker) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.workerPath(w.Name)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("update worker %s: %w", w.Name, ErrNotFound)
	}
	return atomicWriteJSON(path, w)
}

func (b *BeadsStore) workerPath(name string) string {
	return filepath.Join(b.workerDir, name+".json")
}

// --- Store interface: Dependency operations ---

func (b *BeadsStore) AddDep(taskID, dependsOn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, err := b.storage.GetDependencies(context.Background(), taskID)
	if err != nil {
		return fmt.Errorf("add dep check existing: %w", err)
	}
	for _, issue := range existing {
		if issue.ID == dependsOn {
			return nil // already exists
		}
	}

	// Cycle detection: build the full dependency graph via per-issue queries.
	// For ~65 tasks in a pipeline this is acceptable.
	allIssues, err := b.storage.SearchIssues(context.Background(), "", beads.IssueFilter{})
	if err != nil {
		return fmt.Errorf("add dep search issues: %w", err)
	}
	graph := make(map[string][]string)
	for _, issue := range allIssues {
		deps, err := b.storage.GetDependencies(context.Background(), issue.ID)
		if err != nil {
			return fmt.Errorf("add dep load deps for %s: %w", issue.ID, err)
		}
		for _, d := range deps {
			graph[issue.ID] = append(graph[issue.ID], d.ID)
		}
	}
	// Simulate adding the edge and check for cycle.
	graph[taskID] = append(graph[taskID], dependsOn)
	if reachable(graph, dependsOn, taskID) {
		return fmt.Errorf("add dep %s→%s: %w", taskID, dependsOn, ErrCycle)
	}

	dep := &beads.Dependency{
		IssueID:     taskID,
		DependsOnID: dependsOn,
		Type:        beads.DepBlocks,
	}
	if err := b.storage.AddDependency(context.Background(), dep, "orch"); err != nil {
		return fmt.Errorf("add dep: %w", err)
	}
	return nil
}

func (b *BeadsStore) GetDeps(taskID string) ([]string, error) {
	issues, err := b.storage.GetDependencies(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("get deps: %w", err)
	}
	deps := make([]string, 0, len(issues))
	for _, issue := range issues {
		deps = append(deps, issue.ID)
	}
	return deps, nil
}
