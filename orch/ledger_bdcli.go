package orch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BDCLIStore implements Store by shelling out to the `bd` CLI for tasks and
// dependencies, and storing workers as filesystem JSON (bd has no worker
// concept, so reproducing one is gratuitous).
//
// Why CLI instead of the Beads Go SDK: bd 1.0.0 ships in embedded mode
// (cgo Dolt linked into the bd binary). The Beads Go SDK still expects an
// external Dolt SQL server, so wonka built CGO_ENABLED=0 cannot open a store
// the bd binary can read. Shelling out matches the same abstraction Charlie
// already uses (`bd --json`), keeps wonka's release matrix free of cgo, and
// shrinks the dependency closure by ~50 transitive packages.
//
// Concurrency: a process-local sync.Mutex serialises mutations within one
// orchestrator. Cross-process safety relies on the per-branch lifecycle lock
// in lock.go — operators never run two wonkas on the same branch. bd's own
// `--claim` flag offers true CAS semantics that we may adopt in a future
// release as belt-and-suspenders; today we mirror BeadsStore's contract
// rather than introducing a divergence during the migration window.
//
// Cost budget: each bd invocation forks a process, so List/Ready operations
// use a "two-call enrichment" pattern — `bd list -l <filter>` returns IDs,
// then a single `bd show <id1> <id2> ...` returns full labels and deps.
// That keeps the shell-out count to two regardless of result size, instead
// of N+1 per-issue lookups.
type BDCLIStore struct {
	repoPath  string     // bd database directory (typically <repo>/.beads/); used as cmd.Dir so bd locates its config
	workerDir string     // <repoPath>/workers/ — bd has no worker primitive
	bdPath    string     // resolved at construction so we fail fast if bd disappears
	baseEnv   []string   // os.Environ() snapshot + BEADS_ACTOR=<actor>; reused per call
	execCmd   bdExecFunc // injectable for tests
	mu        sync.Mutex // serialises mutations within this process
}

// bdExecFunc is the test seam for replacing exec.CommandContext. The store
// closes over the working directory and binary path itself, so the seam only
// needs the inputs that vary per call: context, env, and argv.
type bdExecFunc func(ctx context.Context, env []string, args ...string) (stdout, stderr []byte, err error)

// bdInvocationTimeout caps every bd subprocess. The plan's p99 budget for
// the slowest mutation (Assign under contention) is 1.5s; 5× that floor
// means real congestion or a hung bd surfaces as ErrStoreUnavailable rather
// than blocking the dispatcher loop.
const bdInvocationTimeout = 5 * time.Second

// bdLockRetryBudget caps the total wall time spent retrying a single bd
// invocation after an embedded-Dolt exclusive-lock collision. bd 1.0's
// embedded backend allows only one writer at a time, and Charlie agents
// hold the lock briefly during their own `bd create`/`bd dep add` calls.
// Wonka's reads and writes have to wait those out — typical hold times
// are <500 ms, so a 2 s budget covers the long tail without dragging the
// dispatch loop.
const bdLockRetryBudget = 2 * time.Second

// bdLockRetryInitialDelay is the first sleep after a lock collision; we
// double on each retry up to half the budget. Tuned so a single fast
// Charlie write (~100 ms) is unblocked on the second attempt.
const bdLockRetryInitialDelay = 75 * time.Millisecond

// isExclusiveLockError reports whether bd's stderr names an embedded-Dolt
// exclusive-lock collision — the only retryable transient bd produces in
// the wonka↔Charlie shared-database setup.
func isExclusiveLockError(stderr string) bool {
	return strings.Contains(stderr, "exclusive lock") &&
		strings.Contains(stderr, "embedded")
}

// NewBDCLIStore constructs a BDCLIStore rooted at the bd database directory.
// Resolves the bd binary at construction time so a missing CLI surfaces
// before any operation — operators see ErrBeadsCLIMissing rather than a
// confusing "exec: bd not found" buried inside a CreateTask call.
func NewBDCLIStore(dir, actor string) (*BDCLIStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("bd-cli store: empty directory")
	}
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return nil, fmt.Errorf("bd-cli store: %w", ErrBeadsCLIMissing)
	}
	workerDir := filepath.Join(dir, "workers")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		return nil, fmt.Errorf("bd-cli store: create worker dir: %w", err)
	}
	if actor == "" {
		actor = defaultActor
	}
	s := &BDCLIStore{
		repoPath:  dir,
		workerDir: workerDir,
		bdPath:    bdPath,
		// Match BeadsStore's audit-trail convention: write the actor string
		// verbatim. defaultActor is "orch" today; engine.go callers can pass
		// a richer "orch:<runID>" later without forcing a wrapper prefix.
		baseEnv: append(os.Environ(), "BEADS_ACTOR="+actor),
	}
	s.execCmd = s.defaultExec
	return s, nil
}

// Close is a no-op — there is no persistent bd connection to release.
func (s *BDCLIStore) Close() error { return nil }

// --- Subprocess plumbing ---

// defaultExec runs the bd binary with the store's resolved binary path and
// working directory. The context timeout from runBd controls cancellation;
// on context cancel exec.Cmd SIGKILLs the child, surfaces the kill as err,
// and lets runBd map it to ErrStoreUnavailable.
func (s *BDCLIStore) defaultExec(ctx context.Context, env []string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, s.bdPath, args...) //nolint:gosec // bdPath resolved via exec.LookPath, args programmer-controlled
	cmd.Dir = s.repoPath
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// runBd invokes bd with bdInvocationTimeout and the cached BEADS_ACTOR env.
// Returns stdout, the trimmed stderr message, and a wrapped error suitable
// for mapBdError. A context-deadline kill or missing-binary error
// short-circuits to ErrStoreUnavailable since both indicate an infra
// problem rather than a domain rejection.
//
// On embedded-Dolt exclusive-lock collisions (Charlie holding the database
// during its own `bd create`/`bd dep add` writes), retries with exponential
// backoff up to bdLockRetryBudget before giving up. Other failure classes
// short-circuit on the first attempt — there's no point retrying a
// not-found or a cycle rejection.
func (s *BDCLIStore) runBd(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
	deadline := time.Now().Add(bdLockRetryBudget)
	delay := bdLockRetryInitialDelay
	for {
		stdout, stderr, err = s.runBdOnce(ctx, args...)
		if err == nil || !isExclusiveLockError(stderr) {
			return stdout, stderr, err
		}
		if time.Now().Add(delay).After(deadline) {
			return stdout, stderr, err
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return stdout, stderr, ctx.Err()
		}
		delay *= 2
		if delay > bdLockRetryBudget/2 {
			delay = bdLockRetryBudget / 2
		}
	}
}

// runBdOnce is a single bd invocation without retry. Pulled out so runBd's
// retry loop has a clean unit to reissue on lock collisions.
func (s *BDCLIStore) runBdOnce(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, bdInvocationTimeout)
	defer cancel()

	out, errOut, runErr := s.execCmd(timeoutCtx, s.baseEnv, args...)
	stderr = strings.TrimSpace(string(errOut))

	if runErr == nil {
		return out, stderr, nil
	}

	// Distinguish infra failures (timeout, killed, exec error) from a clean
	// bd exit-code-N — the former returns ErrStoreUnavailable, the latter
	// flows through mapBdError so the caller can see ErrNotFound / ErrCycle.
	if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
		return out, stderr, fmt.Errorf("bd timed out after %s: %w", bdInvocationTimeout, ErrStoreUnavailable)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return out, stderr, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return out, stderr, runErr
	}
	// e.g. exec failed to start — the binary moved or the host is starved.
	return out, stderr, fmt.Errorf("bd invocation failed: %w: %w", ErrStoreUnavailable, runErr)
}

// runBdMapped is the common shape for "run bd, classify failures, wrap with
// op context". Without this helper every call site reimplemented the
// (mapBdError → fmt.Errorf) cascade and quietly drifted in wrap format.
func (s *BDCLIStore) runBdMapped(ctx context.Context, op string, args ...string) ([]byte, error) {
	out, stderr, err := s.runBd(ctx, args...)
	if err == nil {
		return out, nil
	}
	if mapped := mapBdError(stderr); mapped != nil {
		return out, fmt.Errorf("%s: %w", op, mapped)
	}
	return out, fmt.Errorf("%s: %w (%s)", op, err, stderr)
}

// mapBdError translates bd's stderr text into Store sentinel errors. Pure
// function so unit tests can assert every message pairing without spawning
// bd. Returns nil if no known sentinel matches; the caller wraps with
// operation context for diagnostic clarity.
func mapBdError(stderr string) error {
	switch {
	case strings.Contains(stderr, "no issue found"),
		strings.Contains(stderr, "no issues found"),
		strings.Contains(stderr, "not found"):
		return ErrNotFound
	case strings.Contains(stderr, "already exists"),
		strings.Contains(stderr, "UNIQUE constraint"):
		return ErrTaskExists
	case strings.Contains(stderr, "would create a cycle"),
		strings.Contains(stderr, "circular dependency"):
		return ErrCycle
	}
	return nil
}

// --- Status mapping ---
//
// BVV-S-02 round-trip relies on these being total over the constants in
// types.go. Adding a TaskStatus without updating both directions trips the
// panics below at first use, which is the intended early-warning behavior.

// orchStatusToBdString translates a TaskStatus to bd's CLI status string.
// StatusAssigned maps to "open" because bd has no "assigned" — assignment
// is encoded as open + non-empty assignee, mirroring BeadsStore.
func orchStatusToBdString(s TaskStatus) string {
	switch s {
	case StatusOpen, StatusAssigned:
		return "open"
	case StatusInProgress:
		return "in_progress"
	case StatusCompleted, StatusFailed:
		return "closed"
	case StatusBlocked:
		return "blocked"
	default:
		panic(fmt.Sprintf("[BVV-S-02] orchStatusToBdString: unmapped TaskStatus %q", s))
	}
}

// bdStringToOrchStatus is the inverse mapping. The labels argument
// distinguishes StatusFailed from StatusCompleted (both serialise to
// "closed" in bd, with the orch:failed label as the discriminator).
//
// bd-only statuses (deferred, pinned, hooked) collapse to StatusBlocked —
// they all mean "not currently dispatchable" from wonka's perspective, and
// dispatchable terminality is the only distinction the dispatcher cares
// about. Operators or Charlie writing those statuses by hand surface as
// blocked tasks, not lifecycle crashes.
func bdStringToOrchStatus(s string, labels []string) TaskStatus {
	switch s {
	case "open":
		return StatusOpen
	case "in_progress":
		return StatusInProgress
	case "closed":
		if slices.Contains(labels, labelFailed) {
			return StatusFailed
		}
		return StatusCompleted
	case "blocked", "deferred", "pinned", "hooked":
		return StatusBlocked
	default:
		panic(fmt.Sprintf("[BVV-S-02] bdStringToOrchStatus: unmapped bd status %q", s))
	}
}

// --- Label encoding ---

// taskLabelsToBd flattens a Task.Labels map into the comma-separated form
// bd accepts on --labels / --set-labels. Sorted for deterministic output
// (tests, audit-trail diffs). Adds orch:failed when status==StatusFailed
// so the round-trip from bd's "closed" disambiguates failed vs completed.
//
// Returns ErrInvalidLabelFilter if any label key/value contains a comma,
// since bd splits its label list on commas with no escape mechanism. The
// constraint matches Charlie's existing convention — see CHARLIE.md.
func taskLabelsToBd(t *Task) ([]string, error) {
	out := make([]string, 0, len(t.Labels)+1)
	for k, v := range t.Labels {
		if strings.ContainsRune(k, ',') || strings.ContainsRune(v, ',') {
			return nil, fmt.Errorf("%w: comma in label %s:%s", ErrInvalidLabelFilter, k, v)
		}
		out = append(out, k+":"+v)
	}
	if t.Status == StatusFailed {
		out = append(out, labelFailed)
	}
	slices.Sort(out)
	return out, nil
}

// --- JSON shapes ---

// bdIssue mirrors the fields wonka consumes from `bd show --long --json`
// and `bd list --json`. Fields not present in a particular bd subcommand's
// output stay zero-valued; callers must fetch via bd show when they need
// labels or dependencies (bd list strips both).
type bdIssue struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Priority     int       `json:"priority"`
	Assignee     string    `json:"assignee,omitempty"`
	Labels       []string  `json:"labels,omitempty"`
	Dependencies []bdDep   `json:"dependencies,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// bdDep handles the two shapes bd emits for dependency edges:
// `bd show --long --json` writes `{"id": "<dep>", "dependency_type": "blocks"}`,
// while `bd export` writes `{"depends_on_id": "<dep>"}`. Either field
// populates DependsOn() so call sites stay shape-agnostic.
type bdDep struct {
	ID             string `json:"id"`
	DependsOnID    string `json:"depends_on_id"`
	DependencyType string `json:"dependency_type"`
}

func (d bdDep) target() string {
	if d.DependsOnID != "" {
		return d.DependsOnID
	}
	return d.ID
}

func (i *bdIssue) toTask() *Task {
	t := &Task{
		ID:        i.ID,
		Title:     i.Title,
		Body:      i.Description,
		Status:    bdStringToOrchStatus(i.Status, i.Labels),
		Priority:  i.Priority,
		Assignee:  i.Assignee,
		Labels:    make(map[string]string),
		CreatedAt: i.CreatedAt,
		UpdatedAt: i.UpdatedAt,
	}
	if t.Status == StatusOpen && t.Assignee != "" {
		t.Status = StatusAssigned
	}
	for _, l := range i.Labels {
		if strings.HasPrefix(l, labelPrefix) {
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			t.Labels[k] = v
		}
	}
	return t
}

// --- Store interface: tasks ---

func (s *BDCLIStore) CreateTask(t *Task) error {
	if err := validateID(t.ID); err != nil {
		return err
	}
	labels, err := taskLabelsToBd(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()

	// Pre-check: bd create --id <existing> --force silently partial-overwrites
	// title/description rather than erroring. To honor the ErrTaskExists
	// contract we look the issue up first. Cost is one extra bd call per
	// CreateTask, paid only at task-graph creation time.
	if _, err := s.fetchIssue(ctx, t.ID); err == nil {
		return fmt.Errorf("task %q: %w", t.ID, ErrTaskExists)
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("create task pre-check %s: %w", t.ID, err)
	}

	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now

	// bd rejects empty titles ("title required"). Production writers always
	// set Title (seed.go uses ID as the default), so the only callers that
	// hit the empty case are contract tests that don't care about title
	// content — falling back to ID gives bd a non-empty value without
	// surfacing a misleading sentinel string in the audit trail.
	title := t.Title
	if title == "" {
		title = t.ID
	}

	// `bd create` has no --status flag — all issues are born "open". For
	// any other initial status we follow up with `bd update --status` while
	// still holding mu, so an outside reader never observes the transitional
	// open state. Wonka's production writer (seed.go) always creates open,
	// so the follow-up only fires for tests that pre-load non-open tasks.
	args := []string{
		"create",
		"--id", t.ID,
		"--force", // unconditional: orch IDs (e.g. plan-<branch>) never share bd's repo prefix
		"--title", title,
		"--description", t.Body,
		"--priority", strconv.Itoa(t.Priority),
		"--json",
	}
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}
	if t.Assignee != "" {
		args = append(args, "--assignee", t.Assignee)
	}

	if _, err := s.runBdMapped(ctx, fmt.Sprintf("create task %s", t.ID), args...); err != nil {
		return err
	}

	if t.Status != StatusOpen {
		op := fmt.Sprintf("create task %s: status follow-up", t.ID)
		if _, err := s.runBdMapped(ctx, op, "update", t.ID, "--status", orchStatusToBdString(t.Status)); err != nil {
			return err
		}
	}
	return nil
}

func (s *BDCLIStore) GetTask(id string) (*Task, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	issue, err := s.fetchIssue(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return issue.toTask(), nil
}

// fetchIssue reads a single issue with full labels + deps. Used by GetTask,
// CreateTask's pre-check, and GetDeps.
func (s *BDCLIStore) fetchIssue(ctx context.Context, id string) (*bdIssue, error) {
	out, err := s.runBdMapped(ctx, "get task "+id, "show", id, "--long", "--json")
	if err != nil {
		return nil, err
	}
	var issues []bdIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("get task %s: parse bd show output: %w", id, err)
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return &issues[0], nil
}

// fetchIssues batch-reads multiple IDs in one `bd show` invocation. Empty
// input short-circuits to an empty slice so callers don't need a guard.
// If bd cannot find any of the IDs it returns a JSON error envelope rather
// than the array; mapBdError translates that into ErrNotFound, which the
// caller treats as "all consumed IDs vanished concurrently" — surfacing it
// as an empty result keeps List/Ready behavior consistent with the snapshot
// the operator expects.
func (s *BDCLIStore) fetchIssues(ctx context.Context, ids []string) ([]bdIssue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"show"}, ids...)
	args = append(args, "--long", "--json")

	out, err := s.runBdMapped(ctx, "show batch", args...)
	if err != nil {
		// Concurrent deletion of every requested ID surfaces as ErrNotFound
		// from mapBdError; degrade to an empty result so List/Ready behave
		// like a snapshot.
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var issues []bdIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("show batch: parse bd show output: %w", err)
	}
	return issues, nil
}

func (s *BDCLIStore) UpdateTask(t *Task) error {
	if err := validateID(t.ID); err != nil {
		return err
	}
	labels, err := taskLabelsToBd(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx := context.Background()
	t.UpdatedAt = time.Now()

	// Same empty-title fallback as CreateTask: bd rejects an empty --title
	// even on update. Production writers always set Title; this keeps
	// contract tests using bare {ID, Status} task literals working.
	title := t.Title
	if title == "" {
		title = t.ID
	}

	args := []string{
		"update", t.ID,
		"--title", title,
		"--description", t.Body,
		"--priority", strconv.Itoa(t.Priority),
		"--status", orchStatusToBdString(t.Status),
		"--assignee", t.Assignee,
	}
	// bd has no flag to clear all labels — `--set-labels ""` would create
	// one literal "" label. Skip the flag when labels is empty; in wonka
	// practice every task carries at least the branch label.
	if len(labels) > 0 {
		args = append(args, "--set-labels", strings.Join(labels, ","))
	}

	_, err = s.runBdMapped(ctx, "update task "+t.ID, args...)
	return err
}

func (s *BDCLIStore) ListTasks(labels ...string) ([]*Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}
	ctx := context.Background()

	ids, err := s.listIDs(ctx, labels)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	issues, err := s.fetchIssues(ctx, ids)
	if err != nil {
		return nil, err
	}
	tasks := make([]*Task, 0, len(issues))
	for i := range issues {
		t := issues[i].toTask()
		if labelsMatch(t, labels) {
			tasks = append(tasks, t)
		}
	}
	sortTasks(tasks)
	return tasks, nil
}

// listIDs runs `bd list` with one server-side label pre-filter (if any) and
// returns just the IDs. `--all` includes closed issues so ListTasks can see
// completed/failed work; `-n 0` removes the default 50-row truncation.
// Subsequent in-Go filtering catches remaining labels the caller specified.
func (s *BDCLIStore) listIDs(ctx context.Context, labels []string) ([]string, error) {
	args := []string{"list", "--all", "--json", "-n", "0"}
	if len(labels) > 0 {
		args = append(args, "-l", labels[0])
	}
	out, err := s.runBdMapped(ctx, "list tasks", args...)
	if err != nil {
		return nil, err
	}
	var rows []bdIssue
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("list tasks: parse bd list output: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// ReadyTasks computes readiness locally rather than delegating to `bd ready`.
// bd treats status=blocked as still-blocking for downstream tasks, but
// BVV-ERR-04a defines blocked as terminal — downstream of a blocked task
// should be ready. Computing the predicate ourselves matches FSStore and
// BeadsStore semantics; the cost is one extra bd round-trip (we already
// need bd show for labels, so we just substitute bd list for bd ready).
func (s *BDCLIStore) ReadyTasks(labels ...string) ([]*Task, error) {
	if err := validateLabelFilters(labels); err != nil {
		return nil, err
	}
	ctx := context.Background()

	// `--all` (rather than `bd ready`) is essential — readiness is a function
	// of the entire graph including terminal nodes (which bd ready hides).
	ids, err := s.listIDs(ctx, labels)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	issues, err := s.fetchIssues(ctx, ids)
	if err != nil {
		return nil, err
	}

	// Build status map keyed by ID for the dep-terminality check.
	tasks := make([]*Task, 0, len(issues))
	statuses := make(map[string]TaskStatus, len(issues))
	depsByTask := make(map[string][]string, len(issues))
	for i := range issues {
		t := issues[i].toTask()
		tasks = append(tasks, t)
		statuses[t.ID] = t.Status
		depsByTask[t.ID] = make([]string, 0, len(issues[i].Dependencies))
		for _, d := range issues[i].Dependencies {
			depsByTask[t.ID] = append(depsByTask[t.ID], d.target())
		}
	}

	ready := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		if t.Status != StatusOpen || t.Assignee != "" {
			continue
		}
		if !labelsMatch(t, labels) {
			continue
		}
		// All deps must be terminal. A dep with no entry in statuses is a
		// dangling reference (the dep task hasn't been listed because it
		// lives outside the label filter scope) — treat as not-terminal so
		// we don't claim downstream readiness against unknown state.
		allDepsDone := true
		for _, depID := range depsByTask[t.ID] {
			st, ok := statuses[depID]
			if !ok || !st.Terminal() {
				allDepsDone = false
				break
			}
		}
		if allDepsDone {
			ready = append(ready, t)
		}
	}
	sortTasks(ready)
	return ready, nil
}

// Assign mirrors BeadsStore.Assign: in-process pre-checks under mu, single
// bd update for the task assignee, then the worker JSON write with rollback
// on failure. BVV-S-03 (no double-assignment) holds within one process; the
// per-branch lifecycle lock provides cross-process exclusion.
func (s *BDCLIStore) Assign(taskID, workerName string) error {
	if err := validateID(taskID); err != nil {
		return err
	}
	if err := validateID(workerName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()

	issue, err := s.fetchIssue(ctx, taskID)
	if err != nil {
		return err
	}
	task := issue.toTask()
	if task.Assignee != "" {
		return fmt.Errorf("assign %s: %w", taskID, ErrAlreadyAssigned)
	}
	if task.Status != StatusOpen {
		return fmt.Errorf("assign %s: %w", taskID, ErrTaskNotReady)
	}

	worker, err := s.getWorker(workerName)
	if err != nil {
		return err
	}
	if worker.Status != WorkerIdle {
		return fmt.Errorf("assign to %s: %w", workerName, ErrWorkerBusy)
	}

	if _, err := s.runBdMapped(ctx, "assign update task "+taskID, "update", taskID, "--assignee", workerName, "--status", "open"); err != nil {
		return err
	}

	worker.Status = WorkerActive
	worker.CurrentTaskID = taskID
	if err := atomicWriteJSON(s.workerPath(workerName), worker); err != nil {
		// Rollback the assignment so the task does not stay claimed by a
		// worker we never persisted as active. A failed rollback is rarer
		// than a failed forward write but worth surfacing to the operator.
		if _, _, rbErr := s.runBd(ctx, "update", taskID, "--assignee", ""); rbErr != nil {
			return fmt.Errorf("assign update worker: %w (rollback failed: %v)", err, rbErr)
		}
		return fmt.Errorf("assign update worker: %w", err)
	}
	return nil
}

// --- Store interface: workers (filesystem JSON, mirrors BeadsStore) ---

func (s *BDCLIStore) CreateWorker(w *Worker) error {
	if err := validateID(w.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.workerPath(w.Name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("worker %q: %w", w.Name, ErrWorkerExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worker %q stat: %w", w.Name, err)
	}
	return atomicWriteJSON(path, w)
}

func (s *BDCLIStore) GetWorker(name string) (*Worker, error) { return s.getWorker(name) }

func (s *BDCLIStore) getWorker(name string) (*Worker, error) {
	if err := validateID(name); err != nil {
		return nil, err
	}
	var w Worker
	if err := readJSON(s.workerPath(name), &w); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("get worker %s: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("get worker: %w", err)
	}
	return &w, nil
}

func (s *BDCLIStore) ListWorkers() ([]*Worker, error) {
	entries, err := os.ReadDir(s.workerDir)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	var workers []*Worker
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var w Worker
		if err := readJSON(filepath.Join(s.workerDir, e.Name()), &w); err != nil {
			return nil, fmt.Errorf("list workers: read %s: %w", e.Name(), err)
		}
		workers = append(workers, &w)
	}
	sortWorkers(workers)
	return workers, nil
}

func (s *BDCLIStore) UpdateWorker(w *Worker) error {
	if err := validateID(w.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.workerPath(w.Name)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("update worker %s: %w", w.Name, ErrNotFound)
		}
		return fmt.Errorf("update worker %q stat: %w", w.Name, err)
	}
	return atomicWriteJSON(path, w)
}

func (s *BDCLIStore) workerPath(name string) string {
	return filepath.Join(s.workerDir, name+".json")
}

// --- Store interface: dependencies ---

// AddDep delegates cycle detection and idempotency to bd: a self-cycle
// short-circuits in-process, otherwise `bd dep add` natively succeeds on
// duplicate edges and rejects new cycles with stderr text "would create a
// cycle" that mapBdError translates to ErrCycle. One subprocess per call,
// rather than the show-then-add probe an SDK port would need.
func (s *BDCLIStore) AddDep(taskID, dependsOn string) error {
	if err := validateID(taskID); err != nil {
		return err
	}
	if err := validateID(dependsOn); err != nil {
		return err
	}
	if taskID == dependsOn {
		return fmt.Errorf("add dep %s→%s: %w", taskID, dependsOn, ErrCycle)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op := fmt.Sprintf("add dep %s→%s", taskID, dependsOn)
	_, err := s.runBdMapped(context.Background(), op, "dep", "add", taskID, dependsOn)
	return err
}

func (s *BDCLIStore) GetDeps(taskID string) ([]string, error) {
	if err := validateID(taskID); err != nil {
		return nil, err
	}
	issue, err := s.fetchIssue(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	deps := make([]string, 0, len(issue.Dependencies))
	for _, d := range issue.Dependencies {
		deps = append(deps, d.target())
	}
	return deps, nil
}
