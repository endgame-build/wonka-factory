package orch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// DispatchConfig controls the timing of the dispatch loop.
type DispatchConfig struct {
	Interval          time.Duration // tick interval (default 1s)
	AgentPollInterval time.Duration // runAgent liveness poll (default 500ms)
}

// DefaultDispatchConfig returns sensible defaults for production use.
func DefaultDispatchConfig() DispatchConfig {
	return DispatchConfig{
		Interval:          1 * time.Second,
		AgentPollInterval: 500 * time.Millisecond,
	}
}

// TaskOutcome carries the result from a runAgent goroutine to the dispatch tick.
// Sent on the outcomes channel; processed single-threaded by Drain.
// Exported so test SpawnFunc implementations can construct outcomes.
type TaskOutcome struct {
	Task     *Task
	Worker   *Worker
	Outcome  AgentOutcome
	ExitCode int
	RoleCfg  RoleConfig
}

// NewTaskOutcome constructs a TaskOutcome. Convenience for test SpawnFuncs.
func NewTaskOutcome(task *Task, worker *Worker, outcome AgentOutcome, exitCode int, roleCfg RoleConfig) TaskOutcome {
	return TaskOutcome{Task: task, Worker: worker, Outcome: outcome, ExitCode: exitCode, RoleCfg: roleCfg}
}

// SpawnFunc is the function signature for launching an agent. Production uses
// Dispatcher.runAgent; tests inject mock implementations that send outcomes
// directly to the channel. The attemptCount parameter carries the retry
// attempt count captured on the dispatch goroutine (single-threaded), avoiding
// a data race on RetryState from the spawned goroutine.
type SpawnFunc func(ctx context.Context, task *Task, worker *Worker, roleCfg RoleConfig, attemptCount int, outcomes chan<- TaskOutcome)

// DispatchResult reports the outcome of a single Tick.
// Invariant: LifecycleDone, GapAbort, and Error are mutually exclusive —
// at most one of these three signal fields is true/non-nil per Tick return.
// Dispatched and OrphansFailed are always populated alongside them.
type DispatchResult struct {
	Dispatched    int  // tasks dispatched this tick
	OrphansFailed int  // tasks failed by CB orphan handling
	LifecycleDone bool // all tasks terminal + no active workers
	GapAbort      bool // gap tolerance reached → lifecycle aborted
	Error         error
}

// Dispatcher implements the DAG-driven dispatch loop (BVV-DSP-01..14).
// It queries ReadyTasks, assigns them to idle workers, spawns agent sessions,
// and processes outcomes via a channel to keep state machines single-threaded.
type Dispatcher struct {
	store     Store
	pool      *WorkerPool
	lock      *LifecycleLock
	log       *EventLog
	watchdog  *Watchdog
	gaps      *GapTracker
	retries   *RetryState
	handoffs  *HandoffState
	retryCfg  RetryConfig
	lifecycle *LifecycleConfig
	cfg       DispatchConfig

	branchLabel string // "branch:<name>" for ReadyTasks filter
	aborted     bool   // lifecycle abort flag
	abortReason string // machine-readable reason for the terminal anchor (e.g. "graph_invalid:BVV-TG-09")
	testMode    bool   // skip SpawnSession (set by SetSpawnFunc)

	spawnFunc SpawnFunc
	outcomes  chan TaskOutcome
	agentWg   sync.WaitGroup
	progress  ProgressReporter

	// postSuccessHook, if non-nil, is called after each task transitions to
	// completed AND the store write persisted. Used by the engine for role-
	// specific post-completion work (e.g. graph validation after the planner
	// finishes). The dispatcher itself remains role-agnostic (BVV-DSN-04):
	// any semantic inspection happens in the hook, not in the dispatcher.
	postSuccessHook func(*Task)
}

// NewDispatcher creates a dispatcher wired to all subsystems. The spawnFunc
// defaults to d.runAgent; call SetSpawnFunc to override for tests.
//
// Required dependencies: store, pool, lifecycle, gaps, retries, handoffs.
// Optional (nil-safe): lock, log, watchdog, progress. Zero-value
// DispatchConfig fields are replaced with DefaultDispatchConfig() values.
func NewDispatcher(
	store Store,
	pool *WorkerPool,
	lock *LifecycleLock,
	log *EventLog,
	watchdog *Watchdog,
	gaps *GapTracker,
	retries *RetryState,
	handoffs *HandoffState,
	retryCfg RetryConfig,
	lifecycle *LifecycleConfig,
	cfg DispatchConfig,
	progress ProgressReporter,
) (*Dispatcher, error) {
	switch {
	case store == nil:
		return nil, fmt.Errorf("dispatcher: store is required")
	case pool == nil:
		return nil, fmt.Errorf("dispatcher: pool is required")
	case lifecycle == nil:
		return nil, fmt.Errorf("dispatcher: lifecycle is required")
	case gaps == nil:
		return nil, fmt.Errorf("dispatcher: gaps is required")
	case retries == nil:
		return nil, fmt.Errorf("dispatcher: retries is required")
	case handoffs == nil:
		return nil, fmt.Errorf("dispatcher: handoffs is required")
	}

	// Apply defaults for zero-value config fields.
	defaults := DefaultDispatchConfig()
	if cfg.Interval == 0 {
		cfg.Interval = defaults.Interval
	}
	if cfg.AgentPollInterval == 0 {
		cfg.AgentPollInterval = defaults.AgentPollInterval
	}

	bufSize := pool.MaxWorkers()
	if bufSize < 4 {
		bufSize = 4
	}
	d := &Dispatcher{
		store:       store,
		pool:        pool,
		lock:        lock,
		log:         log,
		watchdog:    watchdog,
		gaps:        gaps,
		retries:     retries,
		handoffs:    handoffs,
		retryCfg:    retryCfg,
		lifecycle:   lifecycle,
		cfg:         cfg,
		branchLabel: "branch:" + lifecycle.Branch,
		outcomes:    make(chan TaskOutcome, bufSize),
		progress:    progress,
	}
	d.spawnFunc = d.runAgent
	return d, nil
}

// SetSpawnFunc overrides the agent launch function and enables test mode.
// In test mode, the dispatcher skips SpawnSession (no tmux needed) and
// transitions the task to in_progress directly via the store.
func (d *Dispatcher) SetSpawnFunc(fn SpawnFunc) {
	d.spawnFunc = fn
	d.testMode = true
}

// SetPostSuccessHook installs a callback fired from processOutcome after a
// task transitions to completed AND the store write persists. Single-
// threaded (runs on the dispatch goroutine) so the hook body does not need
// synchronization against other outcome processing.
//
// The hook may call d.AbortLifecycle(reason) to stop further dispatch —
// useful for post-completion validators (e.g. BVV-TG-07..10 graph
// well-formedness check that runs after the planner completes).
func (d *Dispatcher) SetPostSuccessHook(fn func(*Task)) {
	d.postSuccessHook = fn
}

// AbortLifecycle sets the abort flag and runs cleanup (blocks all remaining
// open tasks so they stop appearing in ReadyTasks). Safe to call from
// postSuccessHook on the dispatch goroutine. For caller-driven aborts
// that don't come from a terminal failure path (BVV-ERR-03/04), the
// caller is responsible for emitting the appropriate audit-trail event
// before invoking this method.
//
// The reason is stamped on the terminal lifecycle_completed anchor. Use a
// short, machine-parseable token (e.g. "graph_invalid:BVV-TG-09") so
// operator tooling can classify aborts without parsing free-form prose.
//
// First-wins: once a reason is set (by this method or by handleTerminalFailure),
// subsequent calls do not overwrite it. This gives operators the earliest
// and typically most-specific cause when multiple abort paths race within
// a single dispatch Drain. An empty reason is always a no-op.
func (d *Dispatcher) AbortLifecycle(reason string) {
	d.setAbortReason(reason)
	d.aborted = true
	d.abortCleanup()
}

// setAbortReason applies first-wins semantics to d.abortReason. Centralises
// the guard so AbortLifecycle and handleTerminalFailure cannot drift.
func (d *Dispatcher) setAbortReason(reason string) {
	if reason != "" && d.abortReason == "" {
		d.abortReason = reason
	}
}

// AbortReason returns the stored abort reason when one was recorded by
// AbortLifecycle or handleTerminalFailure, or empty if no reason was
// captured. Emitted on the terminal lifecycle_completed anchor so an
// operator can distinguish abort causes without timestamp correlation.
// emitLifecycleCompleted treats an empty string as "no specific reason
// recorded" and falls back to the historical "gap_tolerance_exceeded"
// default, so the empty-string semantics are load-bearing.
func (d *Dispatcher) AbortReason() string {
	return d.abortReason
}

// Wait blocks until all runAgent goroutines have completed.
func (d *Dispatcher) Wait() {
	d.agentWg.Wait()
}

// emit forwards an event to both the EventLog and the ProgressReporter.
// Best-effort: dispatch decisions are never blocked by event logging failures.
func (d *Dispatcher) emit(e Event) {
	if err := emitAndNotify(d.log, d.progress, e); err != nil {
		d.warnf("emit event %s (task %s): %v", e.Kind, e.TaskID, err)
	}
}

// warnf writes a diagnostic warning to stderr.
func (d *Dispatcher) warnf(format string, args ...any) {
	fmt.Fprintf(defaultStderr, "warning: "+format+"\n", args...)
}

// --- Outcome processing (runs on dispatch goroutine, single-threaded) ---

// processOutcome routes a completed agent's exit code to the appropriate
// state transition. BVV-DSP-09: the orchestrator is authoritative for status.
func (d *Dispatcher) processOutcome(ctx context.Context, o TaskOutcome) {
	switch o.Outcome {
	case OutcomeSuccess:
		d.handleSuccess(o)
	case OutcomeFailure:
		d.handleFailure(o)
	case OutcomeBlocked:
		d.handleBlocked(o)
	case OutcomeHandoff:
		d.handleHandoff(ctx, o)
	default:
		d.warnf("unknown outcome %q for task %s — treating as failure", o.Outcome, o.Task.ID)
		d.handleFailure(o)
	}
}

// terminateAndRelease is the shared path for all terminal outcomes: set status,
// assert irreversibility, persist to store. On success, releases the worker and
// emits the event. On store failure, neither release nor emit occurs — the
// task/worker pairing stays consistent for watchdog recovery. Returns true if
// the store write succeeded; callers should skip downstream logic (gap/abort)
// on false to avoid acting on state the store doesn't reflect.
func (d *Dispatcher) terminateAndRelease(task *Task, workerName string, newStatus TaskStatus, event Event) bool {
	prev := task.Status
	task.Status = newStatus
	task.UpdatedAt = time.Now()
	AssertTerminalIrreversibility(prev, task.Status)
	if err := d.store.UpdateTask(task); err != nil {
		d.warnf("update task %s: %v", task.ID, err)
		// Do NOT release worker or emit event — the store still shows the task
		// as in_progress with this worker assigned. Releasing the worker would
		// create inconsistency (idle worker, orphaned task). The watchdog or a
		// subsequent tick can retry once the store recovers.
		return false
	}
	if err := d.pool.Release(workerName); err != nil {
		d.warnf("release worker %s: %v", workerName, err)
	}
	d.emit(event)
	return true
}

func (d *Dispatcher) handleSuccess(o TaskOutcome) {
	persisted := d.terminateAndRelease(o.Task, o.Worker.Name, StatusCompleted, Event{
		Kind: EventTaskCompleted, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Outcome: OutcomeSuccess, Summary: fmt.Sprintf("task %s completed", o.Task.ID),
	})
	// Fire the post-success hook only after the completion is durably
	// persisted. Otherwise a store-write retry would trigger the hook
	// twice — unsafe for hooks with side effects (graph validation emits
	// an event, may create an escalation task).
	if persisted && d.postSuccessHook != nil {
		d.postSuccessHook(o.Task)
	}
}

func (d *Dispatcher) handleFailure(o TaskOutcome) {
	if d.retries.CanRetry(o.Task.ID, d.retryCfg) {
		// Work on a copy so the original task is untouched if the store write
		// fails. Without this, falling through to terminateAndRelease would
		// receive a task with Status=Open and Assignee="" — corrupting the
		// terminal-fail store record and losing the worker attribution.
		retryTask := *o.Task
		retryTask.Status = StatusOpen
		retryTask.Assignee = ""
		retryTask.UpdatedAt = time.Now()
		if err := d.store.UpdateTask(&retryTask); err != nil {
			// Store write failed — retry cannot be persisted. Fall through to
			// terminal failure so the task reaches a terminal state rather than
			// being permanently orphaned as in_progress.
			d.warnf("update task %s for retry: %v — failing task instead", o.Task.ID, err)
		} else {
			*o.Task = retryTask
			d.retries.RecordAttempt(o.Task.ID)
			if err := d.pool.Release(o.Worker.Name); err != nil {
				d.warnf("release worker %s: %v", o.Worker.Name, err)
			}
			d.emit(Event{Kind: EventTaskRetried, TaskID: o.Task.ID, Worker: o.Worker.Name,
				Summary: fmt.Sprintf("task %s retried (attempt %d)", o.Task.ID, d.retries.AttemptCount(o.Task.ID))})
			return
		}
	}

	persisted := d.terminateAndRelease(o.Task, o.Worker.Name, StatusFailed, Event{
		Kind: EventTaskFailed, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Outcome: OutcomeFailure, Summary: fmt.Sprintf("task %s failed (exit %d)", o.Task.ID, o.ExitCode),
	})
	if persisted {
		d.handleTerminalFailure(o.Task)
	}
}

func (d *Dispatcher) handleBlocked(o TaskOutcome) {
	persisted := d.terminateAndRelease(o.Task, o.Worker.Name, StatusBlocked, Event{
		Kind: EventTaskBlocked, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Outcome: OutcomeBlocked, Summary: fmt.Sprintf("task %s blocked (exit 2)", o.Task.ID),
	})
	if persisted {
		d.handleTerminalFailure(o.Task)
	}
}

func (d *Dispatcher) handleHandoff(ctx context.Context, o TaskOutcome) {
	count, ok := d.handoffs.TryRecord(o.Task.ID)
	if ok {
		// Handoff within budget — restart session (BVV-DSP-14: status stays in_progress).
		if !d.testMode {
			if err := d.pool.RestartSession(o.Worker.Name, o.Task, o.RoleCfg, d.lifecycle.Branch); err != nil {
				d.warnf("restart session for %s: %v", o.Task.ID, err)
				d.handleFailure(TaskOutcome{Task: o.Task, Worker: o.Worker, Outcome: OutcomeFailure, ExitCode: 1, RoleCfg: o.RoleCfg})
				return
			}
		}
		d.emit(Event{Kind: EventTaskHandoff, TaskID: o.Task.ID, Worker: o.Worker.Name,
			Summary: fmt.Sprintf("task %s handoff %d", o.Task.ID, count)})

		// In test mode, re-launch the SpawnFunc goroutine for the restarted session.
		if d.testMode {
			attempts := d.retries.AttemptCount(o.Task.ID)
			d.agentWg.Add(1)
			go func(t *Task, w *Worker, rc RoleConfig, ac int) {
				defer d.agentWg.Done()
				d.spawnFunc(ctx, t, w, rc, ac, d.outcomes)
			}(o.Task, o.Worker, o.RoleCfg, attempts)
		}
		return
	}

	// Handoff budget exhausted — treat as failure (BVV-L-04).
	d.emit(Event{Kind: EventHandoffLimitReached, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Summary: fmt.Sprintf("task %s handoff limit reached (%d)", o.Task.ID, count)})
	d.handleFailure(TaskOutcome{Task: o.Task, Worker: o.Worker, Outcome: OutcomeFailure, ExitCode: 1, RoleCfg: o.RoleCfg})
}

// handleTerminalFailure routes a terminal failure through gap/abort logic.
// BVV-ERR-03: critical task → immediate abort.
// BVV-ERR-04: non-critical task → gap counter → abort at tolerance.
func (d *Dispatcher) handleTerminalFailure(task *Task) {
	if task.IsCritical() {
		d.setAbortReason("critical_task_failure:" + task.ID)
		d.aborted = true
		d.abortCleanup()
		d.emit(Event{Kind: EventEscalationCreated, TaskID: task.ID,
			Summary: fmt.Sprintf("critical task %s failed — lifecycle aborted", task.ID)})
		return
	}

	abort := d.gaps.IncrementAndCheck(task.ID)
	// Note: AssertBoundedDegradation is NOT called here because gap count
	// can overshoot tolerance by up to MaxWorkers-1 (concurrent in-flight
	// outcomes processed after threshold). TestProp_GapBoundedOvershoot
	// verifies the bound.
	d.emit(Event{Kind: EventGapRecorded, TaskID: task.ID,
		Summary: fmt.Sprintf("gap %d/%d recorded for task %s", d.gaps.Count(), d.lifecycle.GapTolerance, task.ID)})
	if abort {
		d.setAbortReason("gap_tolerance_exceeded")
		d.aborted = true
		d.abortCleanup()
		d.emit(Event{Kind: EventEscalationCreated, TaskID: task.ID,
			Summary: fmt.Sprintf("gap tolerance %d reached — lifecycle aborted", d.lifecycle.GapTolerance)})
	}
}

// abortCleanup blocks all remaining open tasks (BVV-ERR-04a).
func (d *Dispatcher) abortCleanup() {
	tasks, err := d.store.ListTasks(d.branchLabel)
	if err != nil {
		d.warnf("abort cleanup list tasks: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Status == StatusOpen {
			t.Status = StatusBlocked
			t.UpdatedAt = time.Now()
			if err := d.store.UpdateTask(t); err != nil {
				d.warnf("abort cleanup task %s: %v", t.ID, err)
			}
		}
	}
}

// --- Dispatch step ---

// Drain processes all completed agent outcomes from the channel,
// non-blocking; returns when the channel is empty. Called inside Tick and
// by Engine.runLoop after ctx cancellation + Wait(). Without the latter
// call, BVV-ERR-10a's "all sessions drained before release" check trips
// on workers whose agent goroutines exited but whose outcomes never
// reached processOutcome — phantom-busy workers.
//
// Only safe to invoke when no other goroutine is receiving on outcomes;
// concurrent receives would race processOutcome.
func (d *Dispatcher) Drain(ctx context.Context) {
	for {
		select {
		case o := <-d.outcomes:
			d.processOutcome(ctx, o)
		default:
			return
		}
	}
}

// failOrphanedTask fails a non-terminal in-progress task and releases its
// worker. Shared by orphanCk (CB-tripped) and failStuckTasks (watchdog
// budget-exhausted). Returns true if the task was successfully failed.
func (d *Dispatcher) failOrphanedTask(taskID, workerName, summary string) bool {
	task, err := d.store.GetTask(taskID)
	if err != nil {
		d.warnf("failOrphanedTask get task %s: %v", taskID, err)
		return false
	}
	if task.Status.Terminal() {
		return false
	}
	persisted := d.terminateAndRelease(task, workerName, StatusFailed, Event{
		Kind: EventTaskFailed, TaskID: taskID, Worker: workerName,
		Summary: summary,
	})
	if persisted {
		d.handleTerminalFailure(task)
	}
	return persisted
}

// orphanCk handles circuit-breaker-tripped workers (SUP-05/06). When the CB
// trips, one in-progress task per tick is failed to prevent thundering herd.
func (d *Dispatcher) orphanCk() int {
	if d.watchdog == nil || !d.watchdog.CBTripped() {
		return 0
	}
	workers, err := d.store.ListWorkers()
	if err != nil {
		d.warnf("orphanCk list workers: %v", err)
		return 0
	}
	failed := 0
	for _, w := range workers {
		if w.Status != WorkerActive || w.CurrentTaskID == "" {
			continue
		}
		if d.failOrphanedTask(w.CurrentTaskID, w.Name, "circuit breaker tripped") {
			failed++
			break // one per tick
		}
	}
	if failed > 0 {
		d.watchdog.ResetCB()
	}
	return failed
}

// failStuckTasks drains tasks that the watchdog identified as stuck (dead
// session + exhausted handoff budget) and fails them. This closes the
// liveness gap where such tasks would stay in_progress forever.
func (d *Dispatcher) failStuckTasks() int {
	if d.watchdog == nil {
		return 0
	}
	stuck := d.watchdog.drainStuckTasks()
	failed := 0
	for _, s := range stuck {
		if d.failOrphanedTask(s.taskID, s.workerName, "watchdog: session dead, handoff budget exhausted") {
			failed++
		}
	}
	return failed
}

// dispatch assigns ready tasks to idle workers and spawns agent sessions.
// BVV-DSP-01: dispatch all ready tasks up to available workers.
// BVV-DSP-02: no holding ready tasks.
func (d *Dispatcher) dispatch(ctx context.Context) (int, error) {
	if d.aborted {
		return 0, nil
	}

	ready, err := d.store.ReadyTasks(d.branchLabel)
	if err != nil {
		return 0, fmt.Errorf("dispatch: ready tasks: %w", err)
	}

	dispatched := 0
	for _, task := range ready {
		// BVV-DSP-03: role-based routing from task metadata.
		role := task.Role()
		// Escalation tasks are human-facing artifacts — not dispatchable work.
		// Without this skip, each tick would route the escalation through the
		// role map, miss (no RoleEscalation RoleConfig by design), and call
		// createEscalation on it, producing escalation-escalation-<orig> and
		// blocking the previous escalation. The cycle repeats every tick,
		// flooding the ledger. They stay `open` until a human resolves them.
		if role == RoleEscalation {
			continue
		}
		// Role map is keyed by string (CLI-configured) — convert the typed
		// Role to string at the lookup boundary. The CLI builds this map
		// with string keys derived from our untyped Role constants, so the
		// round-trip is lossless.
		roleCfg, ok := d.lifecycle.Roles[string(role)]
		if !ok {
			// BVV-DSP-03a: unknown role → create escalation, block original.
			d.createEscalation(task, role)
			continue
		}

		// Allocate worker — ErrPoolExhausted means all slots busy, try next tick.
		worker, err := d.pool.Allocate()
		if err != nil {
			if !errors.Is(err, ErrPoolExhausted) {
				d.warnf("allocate worker: %v", err)
			}
			break
		}

		// BVV-S-03: atomic assignment (at most one worker per task).
		if err := d.store.Assign(task.ID, worker.Name); err != nil {
			d.warnf("assign task %s to %s: %v", task.ID, worker.Name, err)
			if releaseErr := d.pool.Release(worker.Name); releaseErr != nil {
				d.warnf("release worker %s after assign failure: %v", worker.Name, releaseErr)
			}
			continue
		}
		AssertSingleAssignment(d.store, task.ID)
		AssertDependencyOrdering(d.store, task.ID)

		// Re-read task after Assign (status now assigned).
		taskID := task.ID
		task, err = d.store.GetTask(taskID)
		if err != nil {
			// task may be nil — use a synthetic task with the known ID for cleanup.
			d.failTaskAndRelease(&Task{ID: taskID}, worker.Name, fmt.Errorf("re-read after assign: %w", err))
			continue
		}

		// BVV-S-05: verify routing was metadata-derived. Runs for both
		// test-mode and production paths so regressions can't sneak in via
		// a test-only bypass.
		AssertZeroContentInspection(task, role)

		if d.testMode {
			// Test mode: transition task to in_progress and worker to active
			// without tmux. The test SpawnFunc handles the "session".
			task.Status = StatusInProgress
			task.UpdatedAt = time.Now()
			if err := d.store.UpdateTask(task); err != nil {
				d.failTaskAndRelease(task, worker.Name, fmt.Errorf("test mode update task: %w", err))
				continue
			}
			worker.Status = WorkerActive
			worker.CurrentTaskID = task.ID
			if err := d.store.UpdateWorker(worker); err != nil {
				d.failTaskAndRelease(task, worker.Name, fmt.Errorf("test mode update worker: %w", err))
				continue
			}
		} else {
			// Production: spawn tmux session.
			if err := d.pool.SpawnSession(worker.Name, task, roleCfg, d.lifecycle.Branch); err != nil {
				d.failTaskAndRelease(task, worker.Name, err)
				continue
			}
			// Re-read worker for SessionStartedAt (set by SpawnSession).
			// Failure here means circuit breaker would use a zero SessionStartedAt,
			// silently disabling rapid-failure detection — treat as spawn failure.
			workerName := worker.Name
			worker, err = d.store.GetWorker(workerName)
			if err != nil {
				d.failTaskAndRelease(task, workerName, fmt.Errorf("re-read worker after spawn: %w", err))
				continue
			}
		}

		d.emit(Event{Kind: EventTaskDispatched, TaskID: task.ID, Worker: worker.Name,
			Summary: fmt.Sprintf("task %s dispatched to %s (role: %s)", task.ID, worker.Name, role)})

		// Launch agent monitoring goroutine. Capture attemptCount here on the
		// dispatch goroutine to avoid a data race on RetryState.
		attempts := d.retries.AttemptCount(task.ID)
		d.agentWg.Add(1)
		go func(t *Task, w *Worker, rc RoleConfig, ac int) {
			defer d.agentWg.Done()
			d.spawnFunc(ctx, t, w, rc, ac, d.outcomes)
		}(task, worker, roleCfg, attempts)

		dispatched++
	}
	return dispatched, nil
}

// createEscalation creates an escalation task for an unknown role and blocks
// the original task (BVV-DSP-03a).
func (d *Dispatcher) createEscalation(task *Task, role Role) {
	escID := "escalation-" + task.ID
	escTask := &Task{
		ID:     escID,
		Title:  fmt.Sprintf("Unknown role '%s' for task %s", role, task.ID),
		Body:   fmt.Sprintf("Task %s has role '%s' which is not configured in lifecycle.Roles. Human intervention required.", task.ID, role),
		Status: StatusOpen,
		Labels: map[string]string{
			LabelBranch:      task.Branch(),
			LabelRole:        RoleEscalation,
			LabelCriticality: string(Critical),
		},
		Priority: 0,
	}
	// Best-effort: escalation task may already exist from a prior tick.
	if err := d.store.CreateTask(escTask); err != nil && !errors.Is(err, ErrTaskExists) {
		d.warnf("create escalation task for %s: %v", task.ID, err)
	}

	// Block the original task so it stops appearing in ReadyTasks.
	task.Status = StatusBlocked
	task.UpdatedAt = time.Now()
	if err := d.store.UpdateTask(task); err != nil {
		d.warnf("block task %s for escalation: %v", task.ID, err)
	}
	d.emit(Event{Kind: EventEscalationCreated, TaskID: task.ID,
		Summary: fmt.Sprintf("unknown role '%s' — escalation task %s created", role, escID)})
}

// failTaskAndRelease is a best-effort cleanup when spawn fails.
func (d *Dispatcher) failTaskAndRelease(task *Task, workerName string, reason error) {
	task.Status = StatusFailed
	task.UpdatedAt = time.Now()
	if err := d.store.UpdateTask(task); err != nil {
		d.warnf("failTaskAndRelease update task %s: %v", task.ID, err)
	}
	if err := d.pool.Release(workerName); err != nil {
		d.warnf("failTaskAndRelease release worker %s: %v", workerName, err)
	}
	d.emit(Event{Kind: EventTaskFailed, TaskID: task.ID, Worker: workerName,
		Summary: fmt.Sprintf("spawn failed: %v", reason)})
}

// checkTermination reports whether all tasks are terminal and no workers active.
// BVV-L-01: the lifecycle terminates when all tasks reach a terminal state.
func (d *Dispatcher) checkTermination() bool {
	tasks, err := d.store.ListTasks(d.branchLabel)
	if err != nil {
		d.warnf("checkTermination list tasks: %v", err)
		return false
	}
	for _, t := range tasks {
		if !t.Status.Terminal() {
			return false
		}
	}
	workers, err := d.store.ListWorkers()
	if err != nil {
		d.warnf("checkTermination list workers: %v", err)
		return false
	}
	for _, w := range workers {
		if w.Status == WorkerActive {
			return false
		}
	}
	return len(tasks) > 0 // empty ledger is not "done"
}

// --- Tick and Run ---

// Tick executes one dispatch cycle in 6 numbered steps:
//  1. PROCESS OUTCOMES   — drain completed agent outcomes from the channel.
//  2. HANDLE ORPHANS     — recover CB-tripped + stuck tasks (BVV-ERR-11a).
//  3. CHECK ABORT        — if the lifecycle aborted, exit with GapAbort.
//  4. DISPATCH           — assign ready tasks to idle workers (BVV-DSP-01/02).
//  5. CHECK TERMINATION  — if all tasks terminal and no workers active, done.
//  6. LOCK REFRESH       — refresh the per-branch lifecycle lock (BVV-S-01).
func (d *Dispatcher) Tick(ctx context.Context) DispatchResult {
	// 1. Process completed agent outcomes.
	d.Drain(ctx)

	// WC invariant — tick-boundary check complements the pool-mutation
	// guards in WorkerPool.Allocate/Release.
	guardWorkerConservation(d.store, d.pool.MaxWorkers())

	// 2. Handle CB-tripped orphans.
	orphans := d.orphanCk()

	// 2a. Fail tasks that the watchdog identified as stuck (dead session,
	// handoff budget exhausted). Without this, such tasks stay in_progress
	// forever since the watchdog skips them and the dispatcher never sees
	// an outcome. BVV-S-02/S-10: the dispatcher owns the status transition.
	orphans += d.failStuckTasks()

	// 3. Check abort state.
	if d.aborted {
		return DispatchResult{OrphansFailed: orphans, GapAbort: true}
	}

	// 4. Dispatch ready tasks.
	dispatched, err := d.dispatch(ctx)
	if err != nil {
		return DispatchResult{Dispatched: dispatched, OrphansFailed: orphans, Error: err}
	}

	// 5. Check termination.
	if d.checkTermination() {
		return DispatchResult{Dispatched: dispatched, OrphansFailed: orphans, LifecycleDone: true}
	}

	// 6. Refresh lock.
	if d.lock != nil {
		if err := d.lock.Refresh(d.lifecycle.Branch); err != nil {
			return DispatchResult{Dispatched: dispatched, OrphansFailed: orphans, Error: err}
		}
	}

	return DispatchResult{Dispatched: dispatched, OrphansFailed: orphans}
}

// Run executes the dispatch loop until the lifecycle terminates, aborts, or
// the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result := d.Tick(ctx)
		if result.LifecycleDone {
			return nil
		}
		if result.GapAbort {
			return ErrLifecycleAborted
		}
		if result.Error != nil {
			return result.Error
		}

		timer := time.NewTimer(d.cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// --- Production SpawnFunc ---

// runAgent is the default SpawnFunc. It polls the tmux session for liveness,
// reads the exit code from the sidecar file, and sends the outcome on the
// channel. BVV-ERR-02/02a: scaled session timeout (base * attempt multiplier),
// treated as exit-code-1 on expiry.
func (d *Dispatcher) runAgent(ctx context.Context, task *Task, worker *Worker, roleCfg RoleConfig, attemptCount int, outcomes chan<- TaskOutcome) {
	if d.pool.tmux == nil {
		d.warnf("runAgent: tmux client is nil, cannot monitor session for task %s", task.ID)
		outcomes <- TaskOutcome{
			Task: task, Worker: worker, Outcome: OutcomeBlocked,
			ExitCode: 2, RoleCfg: roleCfg,
		}
		return
	}

	timeout := ScaledTimeout(d.retryCfg.BaseTimeout, attemptCount)
	timeout = RetryJitter(timeout)
	deadline := time.After(timeout)

	ticker := time.NewTicker(d.cfg.AgentPollInterval)
	defer ticker.Stop()

	timedOut := false
	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown — kill the tmux session to prevent zombies,
			// then send a failure outcome so the dispatcher can release the
			// worker cleanly. Without this, the worker stays Active in the
			// store with no monitoring goroutine.
			sessionName := SessionName(d.pool.RunID(), worker.Name)
			if err := d.pool.tmux.KillSessionIfExists(sessionName); err != nil {
				d.warnf("shutdown kill session %s: %v", sessionName, err)
			}
			outcomes <- TaskOutcome{
				Task: task, Worker: worker, Outcome: OutcomeFailure,
				ExitCode: 1, RoleCfg: roleCfg,
			}
			return
		case <-deadline:
			// BVV-ERR-02a: session timeout.
			sessionName := SessionName(d.pool.RunID(), worker.Name)
			if err := d.pool.tmux.KillSessionIfExists(sessionName); err != nil {
				d.warnf("timeout kill session %s: %v", sessionName, err)
			}
			timedOut = true
			goto done
		case <-ticker.C:
			alive, err := d.pool.IsAlive(worker.Name)
			if err != nil {
				d.warnf("IsAlive check for %s (task %s): %v", worker.Name, task.ID, err)
				continue // tmux infra error — retry next poll
			}
			if !alive {
				goto done
			}
		}
	}

done:
	exitCode, exitErr := ReadExitCode(LogPath(d.pool.OutputDir(), task.ID))
	if exitErr != nil {
		d.warnf("read exit code for task %s (worker %s): %v", task.ID, worker.Name, exitErr)
	}

	if timedOut || exitCode < 0 {
		exitCode = 1
	}

	// Record with circuit breaker for non-success exits (SUP-05).
	if exitCode != 0 && d.watchdog != nil {
		d.watchdog.RecordAgentFailure(worker.Name, worker.SessionStartedAt)
	}

	outcome := DetermineOutcome(exitCode)
	outcomes <- TaskOutcome{
		Task:     task,
		Worker:   worker,
		Outcome:  outcome,
		ExitCode: exitCode,
		RoleCfg:  roleCfg,
	}
}

// defaultStderr is the writer used for diagnostic warnings. Tests can
// override this to suppress or capture output.
var defaultStderr interface{ Write([]byte) (int, error) } = os.Stderr
