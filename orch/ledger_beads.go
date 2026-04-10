package orch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	beads "github.com/steveyegge/beads"
)

// defaultActor is the beads audit trail actor used when NewBeadsStore receives
// an empty actor string, and by the store factory for its default constructor.
const defaultActor = "orch"

// beadsNotFound returns true if the error indicates a missing issue.
// The Beads SDK does not export sentinel errors (storage.ErrNotFound lives in
// an internal package), so we match by message substring. This is fragile
// under beads version bumps — if beads reworks its error phrasing, every
// ErrNotFound-sensitive call site (recovery, Assign, UpdateTask) silently
// changes behavior. File an upstream issue to export a sentinel.
func beadsNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// beadsDuplicate returns true if the error indicates a duplicate issue.
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
//
// BVV-DSP-16: Beads is the default store backend.
type BeadsStore struct {
	storage   beads.Storage
	workerDir string
	actor     string     // actor string for beads mutations (e.g., "orch:<runID>")
	mu        sync.Mutex // goroutine-level serialisation
}

// NewBeadsStore creates a Store backed by a Beads Dolt database in dir.
// Workers are stored as JSON files under {dir}/workers/.
// The actor parameter identifies the orchestrator in beads audit trail
// (e.g., "orch" or "orch:<runID>").
func NewBeadsStore(dir, actor string) (*BeadsStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create beads dir: %w", err)
	}
	workerDir := filepath.Join(dir, "workers")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		return nil, fmt.Errorf("create worker dir: %w", err)
	}
	if actor == "" {
		actor = defaultActor
	}

	dbPath := filepath.Join(dir, "dolt")
	storage, err := beads.Open(context.Background(), dbPath)
	if err != nil {
		return nil, fmt.Errorf("open beads storage: %w", err)
	}

	return &BeadsStore{
		storage:   storage,
		workerDir: workerDir,
		actor:     actor,
	}, nil
}

// Close releases the underlying Beads database connection.
func (b *BeadsStore) Close() error {
	return b.storage.Close()
}

// --- Label conventions ---
//
// BVV uses Task.Labels (map[string]string) for all domain metadata (role,
// branch, criticality). These are stored as beads labels in "key:value" format.
//
// The "orch:" namespace is reserved for internal status distinguishers:
//
//   - orch:failed — present when orch status is StatusFailed. Both
//     StatusCompleted and StatusFailed map to beads.StatusClosed (beads has
//     no "failed" state), so taskLabelsToBeads attaches this label whenever
//     Status == StatusFailed, and beadsStatusToOrch inspects it on read-back.
//     Do not repurpose this label.
//
//   - StatusAssigned is derived from beads.StatusOpen + non-empty Issue.Assignee
//     (see toTask). No label needed — Issue.Assignee is the source of truth.

const (
	labelPrefix = "orch:"
	// labelFailed distinguishes StatusFailed from StatusCompleted. See the
	// package comment above for the rationale.
	labelFailed = "orch:failed"
)

// --- Task ↔ Issue mapping ---

// toIssue / toTask translate between BVV Task and beads Issue. The only
// non-obvious mapping is Task.Body ↔ Issue.Description; every other field
// carries its name across the boundary.
func (b *BeadsStore) toIssue(t *Task) *beads.Issue {
	return &beads.Issue{
		ID:          t.ID,
		Title:       t.Title,
		Description: t.Body,
		Status:      orchStatusToBeads(t.Status),
		Priority:    t.Priority,
		IssueType:   beads.TypeTask,
		Assignee:    t.Assignee,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

// taskLabelsToBeads converts the Task.Labels map to beads label strings
// ("key:value") plus the orch:failed distinguisher if applicable.
func taskLabelsToBeads(t *Task) []string {
	var labels []string
	for k, v := range t.Labels {
		labels = append(labels, k+":"+v)
	}
	if t.Status == StatusFailed {
		labels = append(labels, labelFailed)
	}
	return labels
}

func (b *BeadsStore) toTask(issue *beads.Issue, beadsLabels []string) *Task {
	t := &Task{
		ID:        issue.ID,
		Title:     issue.Title,
		Body:      issue.Description,
		Status:    beadsStatusToOrch(issue.Status, beadsLabels),
		Priority:  issue.Priority,
		Assignee:  issue.Assignee,
		Labels:    make(map[string]string),
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.UpdatedAt,
	}
	// StatusAssigned maps to beads.StatusOpen; beads.Issue.Assignee is the
	// source of truth for the Open → Assigned distinction (requires beads ≥ v0.63.3).
	if t.Status == StatusOpen && t.Assignee != "" {
		t.Status = StatusAssigned
	}
	// Parse user labels (key:value format), skip orch-internal labels.
	for _, l := range beadsLabels {
		if strings.HasPrefix(l, labelPrefix) {
			continue // skip orch:failed and any other orch-internal labels
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			t.Labels[k] = v
		}
	}
	return t
}

// orchStatusToBeads translates a BVV TaskStatus to its beads encoding.
// Panics on unmapped values: callers must only pass the six TaskStatus
// constants declared in types.go. A panic here indicates a caller bug or a
// missed case when TaskStatus is extended. BVV-S-02 relies on this mapping
// being total and faithful.
func orchStatusToBeads(s TaskStatus) beads.Status {
	switch s {
	case StatusOpen:
		return beads.StatusOpen
	case StatusAssigned:
		return beads.StatusOpen // distinguished by issue.Assignee on read-back
	case StatusInProgress:
		return beads.StatusInProgress
	case StatusCompleted:
		return beads.StatusClosed
	case StatusFailed:
		return beads.StatusClosed // distinguished by labelFailed
	case StatusBlocked:
		return beads.StatusBlocked // native in beads@v0.63.3
	default:
		panic(fmt.Sprintf("[BVV-S-02] orchStatusToBeads: unmapped TaskStatus %q", s))
	}
}

// beadsStatusToOrch is the inverse of orchStatusToBeads. Panics on unmapped
// beads statuses — a new beads status appearing on read-back means either the
// beads SDK was upgraded without updating this switch, or data written by an
// incompatible writer ended up in the Dolt database. BVV-S-02 relies on the
// round-trip being total.
func beadsStatusToOrch(s beads.Status, beadsLabels []string) TaskStatus {
	switch s {
	case beads.StatusOpen:
		return StatusOpen
	case beads.StatusInProgress:
		return StatusInProgress
	case beads.StatusClosed:
		if slices.Contains(beadsLabels, labelFailed) {
			return StatusFailed
		}
		return StatusCompleted
	case beads.StatusBlocked:
		return StatusBlocked
	default:
		panic(fmt.Sprintf("[BVV-S-02] beadsStatusToOrch: unmapped beads.Status %q", s))
	}
}

// --- Store interface: Task operations ---

func (b *BeadsStore) CreateTask(t *Task) error {
	if err := validateID(t.ID); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx := context.Background()
	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now

	return b.storage.RunInTransaction(ctx, b.actor, func(tx beads.Transaction) error {
		if err := tx.CreateIssue(ctx, b.toIssue(t), b.actor); err != nil {
			if beadsDuplicate(err) {
				return fmt.Errorf("task %q: %w", t.ID, ErrTaskExists)
			}
			return fmt.Errorf("create task: %w", err)
		}
		for _, label := range taskLabelsToBeads(t) {
			if err := tx.AddLabel(ctx, t.ID, label, b.actor); err != nil {
				return fmt.Errorf("add label %s: %w", label, err)
			}
		}
		return nil
	})
}

func (b *BeadsStore) GetTask(id string) (*Task, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	return b.readTask(context.Background(), id)
}

// readTask fetches a task from beads (issue + labels) without acquiring b.mu.
// Reads are safe for concurrent callers because beads provides its own
// transaction isolation; the mutex only guards combined task+worker writes.
// Shared by GetTask and Assign.
func (b *BeadsStore) readTask(ctx context.Context, id string) (*Task, error) {
	issue, err := b.storage.GetIssue(ctx, id)
	if err != nil {
		if beadsNotFound(err) {
			return nil, fmt.Errorf("task %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get task: %w", err)
	}
	labels, err := b.storage.GetLabels(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get labels for %s: %w", id, err)
	}
	return b.toTask(issue, labels), nil
}

// UpdateTask persists the task's current state. BVV-S-02 (terminal
// irreversibility) is documented on the Store interface and enforced by the
// dispatcher, not here.
//
// Existence is checked by beads inside the transaction (UpdateIssueInTx reads
// the old issue before updating), so we translate that error rather than
// performing a separate GetIssue round-trip.
func (b *BeadsStore) UpdateTask(t *Task) error {
	if err := validateID(t.ID); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx := context.Background()
	t.UpdatedAt = time.Now()

	return b.storage.RunInTransaction(ctx, b.actor, func(tx beads.Transaction) error {
		updates := map[string]any{
			"status":      string(orchStatusToBeads(t.Status)),
			"priority":    t.Priority,
			"assignee":    t.Assignee,
			"title":       t.Title,
			"description": t.Body,
		}
		if err := tx.UpdateIssue(ctx, t.ID, updates, b.actor); err != nil {
			if beadsNotFound(err) {
				return fmt.Errorf("update task %s: %w", t.ID, ErrNotFound)
			}
			return fmt.Errorf("update issue: %w", err)
		}

		// Replace all labels: remove old orch: + user labels, add new ones.
		oldLabels, err := tx.GetLabels(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("get old labels for %s: %w", t.ID, err)
		}
		for _, l := range oldLabels {
			if err := tx.RemoveLabel(ctx, t.ID, l, b.actor); err != nil {
				return fmt.Errorf("remove label %s for %s: %w", l, t.ID, err)
			}
		}
		for _, label := range taskLabelsToBeads(t) {
			if err := tx.AddLabel(ctx, t.ID, label, b.actor); err != nil {
				return fmt.Errorf("add label %s: %w", label, err)
			}
		}
		return nil
	})
}

// ListTasks returns all tasks matching the given label filters.
func (b *BeadsStore) ListTasks(labels ...string) ([]*Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}

	ctx := context.Background()
	var issues []*beads.Issue
	var err error

	if len(labels) > 0 {
		// Use the first label for the beads query, then in-memory filter the rest.
		issues, err = b.storage.GetIssuesByLabel(ctx, labels[0])
	} else {
		issues, err = b.storage.SearchIssues(ctx, "", beads.IssueFilter{})
	}
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	tasks := make([]*Task, 0, len(issues))
	for _, issue := range issues {
		beadsLabels, err := b.storage.GetLabels(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("get labels for %s: %w", issue.ID, err)
		}
		t := b.toTask(issue, beadsLabels)
		if labelsMatch(t, labels) {
			tasks = append(tasks, t)
		}
	}
	sortTasks(tasks)
	return tasks, nil
}

// ReadyTasks returns tasks where status=open, all deps terminal, assignee
// empty, and all label filters match. Sorted by taskLess (LDG-07).
//
// BVV-DSP-01: ready = open ∧ deps-terminal ∧ unassigned ∧ labels-match.
//
// TODO(BVV-DSP, Phase 3, deferred-D4): BVV spec §5.1a defines in_progress as
// "open + orch:in_progress". This implementation uses beads.StatusInProgress
// directly and omits the label. Re-evaluate when dispatch filters by
// in_progress state; see docs/BVV_IMPLEMENTATION_PLAN.md §2.6.
func (b *BeadsStore) ReadyTasks(labels ...string) ([]*Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}

	ctx := context.Background()
	issues, err := b.storage.GetReadyWork(ctx, beads.WorkFilter{})
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}

	tasks := make([]*Task, 0, len(issues))
	for _, issue := range issues {
		beadsLabels, err := b.storage.GetLabels(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("get labels for %s: %w", issue.ID, err)
		}
		t := b.toTask(issue, beadsLabels)
		// Filter: open, unassigned, labels match.
		if t.Status != StatusOpen {
			continue
		}
		if t.Assignee != "" {
			continue
		}
		if !labelsMatch(t, labels) {
			continue
		}
		tasks = append(tasks, t)
	}

	sortTasks(tasks)
	return tasks, nil
}

// Assign sets task.Assignee=workerName and worker.CurrentTaskID=taskID.
// StatusAssigned is derived on read-back from StatusOpen+Assignee (see toTask);
// the beads issue status is not explicitly changed.
// BVV-S-03: at most one worker per task (see BVVTaskMachine.tla Assign action).
// LDG-08: task+worker update serialized by mu (single-process). Task assignee
// is set via beads transaction; worker JSON is updated separately with rollback
// on failure. Not cross-store atomic.
func (b *BeadsStore) Assign(taskID, workerName string) error {
	if err := validateID(taskID); err != nil {
		return err
	}
	if err := validateID(workerName); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx := context.Background()
	task, err := b.readTask(ctx, taskID)
	if err != nil {
		return err
	}
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

	// Update task in beads: set assignee (no orch:assigned label needed — Assignee
	// field is the source of truth since beads.Issue.Assignee exists in v0.63.3).
	if err := b.storage.RunInTransaction(ctx, b.actor, func(tx beads.Transaction) error {
		updates := map[string]any{
			"assignee": workerName,
		}
		return tx.UpdateIssue(ctx, taskID, updates, b.actor)
	}); err != nil {
		return fmt.Errorf("assign update task: %w", err)
	}

	// Update worker JSON on filesystem.
	worker.Status = WorkerActive
	worker.CurrentTaskID = taskID
	if err := atomicWriteJSON(b.workerPath(workerName), worker); err != nil {
		// Rollback task assignment on worker write failure.
		rbErr := b.storage.RunInTransaction(ctx, b.actor, func(tx beads.Transaction) error {
			return tx.UpdateIssue(ctx, taskID, map[string]any{"assignee": ""}, b.actor)
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
	if err := validateID(w.Name); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.workerPath(w.Name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("worker %q: %w", w.Name, ErrWorkerExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worker %q stat: %w", w.Name, err)
	}
	return atomicWriteJSON(path, w)
}

func (b *BeadsStore) GetWorker(name string) (*Worker, error) {
	return b.getWorker(name)
}

func (b *BeadsStore) getWorker(name string) (*Worker, error) {
	if err := validateID(name); err != nil {
		return nil, err
	}
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

// ListWorkers returns all workers sorted by name ASC (deterministic for tests).
func (b *BeadsStore) ListWorkers() ([]*Worker, error) {
	entries, err := os.ReadDir(b.workerDir)
	if err != nil {
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
	sortWorkers(workers)
	return workers, nil
}

func (b *BeadsStore) UpdateWorker(w *Worker) error {
	if err := validateID(w.Name); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.workerPath(w.Name)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("update worker %s: %w", w.Name, ErrNotFound)
		}
		return fmt.Errorf("update worker %q stat: %w", w.Name, err)
	}
	return atomicWriteJSON(path, w)
}

func (b *BeadsStore) workerPath(name string) string {
	return filepath.Join(b.workerDir, name+".json")
}

// --- Store interface: Dependency operations ---

func (b *BeadsStore) AddDep(taskID, dependsOn string) error {
	if err := validateID(taskID); err != nil {
		return err
	}
	if err := validateID(dependsOn); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx := context.Background()
	existing, err := b.storage.GetDependencies(ctx, taskID)
	if err != nil {
		return fmt.Errorf("add dep check existing: %w", err)
	}
	if slices.ContainsFunc(existing, func(i *beads.Issue) bool { return i.ID == dependsOn }) {
		return nil // idempotent
	}

	// Cycle detection: build the full dependency graph via per-issue queries.
	// For ~65 tasks in a lifecycle this is acceptable.
	allIssues, err := b.storage.SearchIssues(ctx, "", beads.IssueFilter{})
	if err != nil {
		return fmt.Errorf("add dep search issues: %w", err)
	}
	graph := make(map[string][]string)
	for _, issue := range allIssues {
		deps, err := b.storage.GetDependencies(ctx, issue.ID)
		if err != nil {
			return fmt.Errorf("add dep load deps for %s: %w", issue.ID, err)
		}
		for _, d := range deps {
			graph[issue.ID] = append(graph[issue.ID], d.ID)
		}
	}
	// Simulate adding the edge and check for cycle (LDG-06).
	graph[taskID] = append(graph[taskID], dependsOn)
	if reachable(graph, dependsOn, taskID) {
		return fmt.Errorf("add dep %s→%s: %w", taskID, dependsOn, ErrCycle)
	}

	dep := &beads.Dependency{
		IssueID:     taskID,
		DependsOnID: dependsOn,
		Type:        beads.DepBlocks,
	}
	if err := b.storage.AddDependency(ctx, dep, b.actor); err != nil {
		return fmt.Errorf("add dep: %w", err)
	}
	return nil
}

func (b *BeadsStore) GetDeps(taskID string) ([]string, error) {
	if err := validateID(taskID); err != nil {
		return nil, err
	}
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
