package orch

import (
	"context"
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
// Sent on the outcomes channel; processed single-threaded by drainOutcomes.
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
// directly to the channel.
type SpawnFunc func(ctx context.Context, task *Task, worker *Worker, roleCfg RoleConfig, outcomes chan<- TaskOutcome)

// DispatchResult reports the outcome of a single Tick.
type DispatchResult struct {
	Dispatched    int  // tasks dispatched this tick
	OrphansFailed int  // tasks failed by CB orphan handling
	LifecycleDone bool // all tasks terminal + no active workers
	GapAbort      bool // gap tolerance reached → lifecycle aborted
	Error         error
}

// Dispatcher implements the DAG-driven dispatch loop (BVV-DSP-01..15).
// It queries ReadyTasks, assigns them to idle workers, spawns agent sessions,
// and processes outcomes via a channel to keep state machines single-threaded.
type Dispatcher struct {
	store    Store
	pool     *WorkerPool
	lock     *LifecycleLock
	log      *EventLog
	watchdog *Watchdog
	gaps     *GapTracker
	retries  *RetryState
	handoffs *HandoffState
	retryCfg RetryConfig
	lifecycle *LifecycleConfig
	cfg      DispatchConfig

	branchLabel string // "branch:<name>" for ReadyTasks filter
	aborted     bool   // lifecycle abort flag
	testMode    bool   // skip SpawnSession (set by SetSpawnFunc)

	spawnFunc SpawnFunc
	outcomes  chan TaskOutcome
	agentWg   sync.WaitGroup
	progress  ProgressReporter
}

// NewDispatcher creates a dispatcher wired to all subsystems. The spawnFunc
// defaults to d.runAgent; call SetSpawnFunc to override for tests.
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
) *Dispatcher {
	bufSize := 4 // minimum buffer
	if pool != nil {
		bufSize = pool.MaxWorkers()
		if bufSize < 1 {
			bufSize = 4
		}
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
	return d
}

// SetSpawnFunc overrides the agent launch function and enables test mode.
// In test mode, the dispatcher skips SpawnSession (no tmux needed) and
// transitions the task to in_progress directly via the store.
func (d *Dispatcher) SetSpawnFunc(fn SpawnFunc) {
	d.spawnFunc = fn
	d.testMode = true
}

// Wait blocks until all runAgent goroutines have completed.
func (d *Dispatcher) Wait() {
	d.agentWg.Wait()
}

// emit forwards an event to both the EventLog and the ProgressReporter.
// Best-effort: dispatch decisions are never blocked by event logging failures.
func (d *Dispatcher) emit(e Event) {
	_ = emitAndNotify(d.log, d.progress, e)
}

// warnf writes a diagnostic warning to stderr.
func (d *Dispatcher) warnf(format string, args ...any) {
	fmt.Fprintf(defaultStderr, "warning: "+format+"\n", args...)
}

// --- Outcome processing (runs on dispatch goroutine, single-threaded) ---

// processOutcome routes a completed agent's exit code to the appropriate
// state transition. BVV-DSP-09: the orchestrator is authoritative for status.
func (d *Dispatcher) processOutcome(o TaskOutcome) {
	switch o.Outcome {
	case OutcomeSuccess:
		d.handleSuccess(o)
	case OutcomeFailure:
		d.handleFailure(o)
	case OutcomeBlocked:
		d.handleBlocked(o)
	case OutcomeHandoff:
		d.handleHandoff(o)
	}
}

// terminateAndRelease is the shared path for all terminal outcomes: set status,
// assert irreversibility, persist, release worker, emit event.
func (d *Dispatcher) terminateAndRelease(task *Task, workerName string, newStatus TaskStatus, event Event) {
	prev := task.Status
	task.Status = newStatus
	task.UpdatedAt = time.Now()
	AssertTerminalIrreversibility(prev, task.Status)
	if err := d.store.UpdateTask(task); err != nil {
		d.warnf("update task %s: %v", task.ID, err)
	}
	if err := d.pool.Release(workerName); err != nil {
		d.warnf("release worker %s: %v", workerName, err)
	}
	d.emit(event)
}

func (d *Dispatcher) handleSuccess(o TaskOutcome) {
	d.terminateAndRelease(o.Task, o.Worker.Name, StatusCompleted, Event{
		Kind: EventTaskCompleted, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Outcome: OutcomeSuccess, Summary: fmt.Sprintf("task %s completed", o.Task.ID),
	})
}

func (d *Dispatcher) handleFailure(o TaskOutcome) {
	if d.retries.CanRetry(o.Task.ID, d.retryCfg) {
		d.retries.RecordAttempt(o.Task.ID)
		o.Task.Status = StatusOpen
		o.Task.Assignee = ""
		o.Task.UpdatedAt = time.Now()
		if err := d.store.UpdateTask(o.Task); err != nil {
			d.warnf("update task %s for retry: %v", o.Task.ID, err)
		}
		if err := d.pool.Release(o.Worker.Name); err != nil {
			d.warnf("release worker %s: %v", o.Worker.Name, err)
		}
		d.emit(Event{Kind: EventTaskRetried, TaskID: o.Task.ID, Worker: o.Worker.Name,
			Summary: fmt.Sprintf("task %s retried (attempt %d)", o.Task.ID, d.retries.AttemptCount(o.Task.ID))})
		return
	}

	d.terminateAndRelease(o.Task, o.Worker.Name, StatusFailed, Event{
		Kind: EventTaskFailed, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Outcome: OutcomeFailure, Summary: fmt.Sprintf("task %s failed (exit %d)", o.Task.ID, o.ExitCode),
	})
	d.handleTerminalFailure(o.Task)
}

func (d *Dispatcher) handleBlocked(o TaskOutcome) {
	d.terminateAndRelease(o.Task, o.Worker.Name, StatusBlocked, Event{
		Kind: EventTaskBlocked, TaskID: o.Task.ID, Worker: o.Worker.Name,
		Outcome: OutcomeBlocked, Summary: fmt.Sprintf("task %s blocked (exit 2)", o.Task.ID),
	})
	d.handleTerminalFailure(o.Task)
}

func (d *Dispatcher) handleHandoff(o TaskOutcome) {
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
			d.agentWg.Add(1)
			go func() {
				defer d.agentWg.Done()
				d.spawnFunc(context.Background(), o.Task, o.Worker, o.RoleCfg, d.outcomes)
			}()
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
		d.aborted = true
		d.abortCleanup()
		d.emit(Event{Kind: EventEscalationCreated, TaskID: task.ID,
			Summary: fmt.Sprintf("critical task %s failed — lifecycle aborted", task.ID)})
		return
	}

	abort := d.gaps.IncrementAndCheck(task.ID)
	// Note: AssertBoundedDegradation is NOT called here because gap count
	// can overshoot tolerance by up to MaxWorkers-1 (concurrent in-flight
	// outcomes processed after threshold). Property test verifies the bound.
	d.emit(Event{Kind: EventGapRecorded, TaskID: task.ID,
		Summary: fmt.Sprintf("gap %d/%d recorded for task %s", d.gaps.Count(), d.lifecycle.GapTolerance, task.ID)})
	if abort {
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

// drainOutcomes processes all completed agent outcomes from the channel.
// Non-blocking: returns when the channel is empty.
func (d *Dispatcher) drainOutcomes() {
	for {
		select {
		case o := <-d.outcomes:
			d.processOutcome(o)
		default:
			return
		}
	}
}

// orphanCk handles circuit-breaker-tripped workers (SUP-05/06). When the CB
// trips, one in-progress task per tick is failed to prevent thundering herd.
func (d *Dispatcher) orphanCk() int {
	if d.watchdog == nil || !d.watchdog.CBTripped() {
		return 0
	}
	workers, err := d.store.ListWorkers()
	if err != nil {
		return 0
	}
	failed := 0
	for _, w := range workers {
		if w.Status != WorkerActive || w.CurrentTaskID == "" {
			continue
		}
		task, err := d.store.GetTask(w.CurrentTaskID)
		if err != nil || task.Status.Terminal() {
			continue
		}
		// Fail one task per tick.
		task.Status = StatusFailed
		task.UpdatedAt = time.Now()
		if err := d.store.UpdateTask(task); err != nil {
			continue
		}
		if err := d.pool.Release(w.Name); err != nil {
			d.warnf("orphanCk release %s: %v", w.Name, err)
		}
		d.emit(Event{Kind: EventTaskFailed, TaskID: task.ID, Worker: w.Name,
			Summary: "circuit breaker tripped"})
		d.handleTerminalFailure(task)
		failed++
		break // one per tick
	}
	if failed > 0 {
		d.watchdog.ResetCB()
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
		roleCfg, ok := d.lifecycle.Roles[role]
		if !ok {
			// BVV-DSP-03a: unknown role → create escalation, block original.
			d.createEscalation(task, role)
			continue
		}

		// Allocate worker — ErrPoolExhausted means all slots busy, try next tick.
		worker, err := d.pool.Allocate()
		if err != nil {
			break // pool exhausted
		}

		// BVV-S-03: atomic assignment (at most one worker per task).
		if err := d.store.Assign(task.ID, worker.Name); err != nil {
			// Assignment failed (race, already assigned, etc.) — skip this task.
			continue
		}
		AssertSingleAssignment(d.store, task.ID)
		AssertDependencyOrdering(d.store, task.ID)

		// Re-read task after Assign (status now assigned).
		task, err = d.store.GetTask(task.ID)
		if err != nil {
			continue
		}

		if d.testMode {
			// Test mode: transition task to in_progress and worker to active
			// without tmux. The test SpawnFunc handles the "session".
			task.Status = StatusInProgress
			task.UpdatedAt = time.Now()
			_ = d.store.UpdateTask(task)
			worker.Status = WorkerActive
			worker.CurrentTaskID = task.ID
			_ = d.store.UpdateWorker(worker)
		} else {
			// Production: spawn tmux session.
			if err := d.pool.SpawnSession(worker.Name, task, roleCfg, d.lifecycle.Branch); err != nil {
				d.failTaskAndRelease(task, worker.Name, err)
				continue
			}
			// Re-read worker for SessionStartedAt (set by SpawnSession).
			worker, err = d.store.GetWorker(worker.Name)
			if err != nil {
				d.warnf("re-read worker %s: %v", worker.Name, err)
			}
		}

		d.emit(Event{Kind: EventTaskDispatched, TaskID: task.ID, Worker: worker.Name,
			Summary: fmt.Sprintf("task %s dispatched to %s (role: %s)", task.ID, worker.Name, role)})

		// Launch agent monitoring goroutine.
		d.agentWg.Add(1)
		go func(t *Task, w *Worker, rc RoleConfig) {
			defer d.agentWg.Done()
			d.spawnFunc(ctx, t, w, rc, d.outcomes)
		}(task, worker, roleCfg)

		dispatched++
	}
	return dispatched, nil
}

// createEscalation creates an escalation task for an unknown role and blocks
// the original task (BVV-DSP-03a).
func (d *Dispatcher) createEscalation(task *Task, role string) {
	escID := "escalation-" + task.ID
	escTask := &Task{
		ID:     escID,
		Title:  fmt.Sprintf("Unknown role '%s' for task %s", role, task.ID),
		Body:   fmt.Sprintf("Task %s has role '%s' which is not configured in lifecycle.Roles. Human intervention required.", task.ID, role),
		Status: StatusOpen,
		Labels: map[string]string{
			LabelBranch:      task.Branch(),
			LabelRole:        "escalation",
			LabelCriticality: string(Critical),
		},
		Priority: 0,
	}
	// Best-effort: escalation task may already exist from a prior tick.
	_ = d.store.CreateTask(escTask)

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
	_ = d.store.UpdateTask(task)
	_ = d.pool.Release(workerName)
	d.emit(Event{Kind: EventTaskFailed, TaskID: task.ID, Worker: workerName,
		Summary: fmt.Sprintf("spawn failed: %v", reason)})
}

// checkTermination reports whether all tasks are terminal and no workers active.
// BVV-L-01: the lifecycle terminates when all tasks reach a terminal state.
func (d *Dispatcher) checkTermination() bool {
	tasks, err := d.store.ListTasks(d.branchLabel)
	if err != nil {
		return false
	}
	for _, t := range tasks {
		if !t.Status.Terminal() {
			return false
		}
	}
	workers, err := d.store.ListWorkers()
	if err != nil {
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

// Tick executes one dispatch cycle: drain outcomes, handle orphans, dispatch
// ready tasks, check termination, refresh lock.
func (d *Dispatcher) Tick(ctx context.Context) DispatchResult {
	// 1. Process completed agent outcomes.
	d.drainOutcomes()

	// 2. Handle CB-tripped orphans.
	orphans := d.orphanCk()

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
// channel. BVV-ERR-02/02a: timer-based session timeout with escalation.
func (d *Dispatcher) runAgent(ctx context.Context, task *Task, worker *Worker, roleCfg RoleConfig, outcomes chan<- TaskOutcome) {
	timeout := ScaledTimeout(d.retryCfg.BaseTimeout, d.retries.AttemptCount(task.ID))
	timeout += RetryJitter(timeout)
	deadline := time.After(timeout)

	ticker := time.NewTicker(d.cfg.AgentPollInterval)
	defer ticker.Stop()

	timedOut := false
	for {
		select {
		case <-ctx.Done():
			return // shutdown — don't send outcome
		case <-deadline:
			// BVV-ERR-02a: session timeout.
			sessionName := SessionName(d.pool.RunID(), worker.Name)
			d.pool.tmux.KillSessionIfExists(sessionName) //nolint:errcheck
			timedOut = true
			goto done
		case <-ticker.C:
			alive, err := d.pool.IsAlive(worker.Name)
			if err != nil {
				continue // tmux infra error — retry next tick
			}
			if !alive {
				goto done
			}
		}
	}

done:
	exitCode, _ := ReadExitCode(LogPath(d.pool.OutputDir(), task.ID))

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
