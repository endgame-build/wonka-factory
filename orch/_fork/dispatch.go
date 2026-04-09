package orch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DispatchConfig configures the dispatch loop.
type DispatchConfig struct {
	Interval          time.Duration // dispatch tick interval (default: 1s)
	AgentPollInterval time.Duration // agent session liveness poll interval (default: 500ms)
}

// DefaultDispatchConfig returns the default configuration.
func DefaultDispatchConfig() DispatchConfig {
	return DispatchConfig{
		Interval:          1 * time.Second,
		AgentPollInterval: 500 * time.Millisecond,
	}
}

// PhaseContext tracks the current phase for the dispatch loop.
type PhaseContext struct {
	Index       int    // current phase index (0-based)
	PhaseTaskID string // task ID for the phase task in the store
	PhaseDef    *Phase // pointer to Pipeline.Phases[Index]
}

// DispatchResult summarises what happened in a single dispatch tick.
type DispatchResult struct {
	Dispatched    int   // tasks dispatched this tick
	OrphansFailed int   // orphaned tasks failed via CB
	PhaseAdvanced bool  // phase was advanced
	PipelineDone  bool  // pipeline reached terminal state
	GateFailed    bool  // halting gate failed → pipeline terminal
	GapAbort      bool  // gap tolerance reached → pipeline abort
	Error         error // fatal error (retries exhausted on critical)
}

// SpawnFunc is called to monitor an agent after SpawnSession. The function should
// block until the agent completes and then update task status and release the worker.
// Tests inject a function that immediately completes; production uses runAgent.
type SpawnFunc func(ctx context.Context, task *Task, worker *Worker, agentDef AgentDef)

// Dispatcher implements the dispatch loop per TLA+ Orch1 process.
//
// Per tick (maps to TLA+ labels):
//  1. dispatch  — ReadyTasks() → Assign → spawn (TLA+ Dispatch label)
//  2. orphanCk  — CB tripped → fail orphaned task + return worker (TLA+ OrphanCk)
//  3. check     — all non-gate terminal → retry/gap → gate → advance (TLA+ Check..GateFail)
//  4. refreshLock — update lock timestamp (TLA+ AdvTick)
type Dispatcher struct {
	store          Store
	pool           *WorkerPool
	lock           *PipelineLock
	log            *EventLog
	watchdog       *Watchdog
	gaps           *GapTracker
	retries        *RetryState
	retryCfg       RetryConfig
	pipeline       *Pipeline
	preset         *Preset
	pluginDir      string
	outputDir      string
	cfg            DispatchConfig
	current        PhaseContext
	agentIndex     map[string]AgentDef
	pipelineID     string // root pipeline task ID
	spawnFunc      SpawnFunc
	processedFails map[string]bool // task IDs already processed for gap/retry (prevents double-counting across ticks)
	agentWg        sync.WaitGroup  // tracks spawned agent goroutines for clean shutdown
	progress       ProgressReporter
}

// NewDispatcher creates a dispatcher with all required dependencies.
func NewDispatcher(
	store Store,
	pool *WorkerPool,
	lock *PipelineLock,
	log *EventLog,
	watchdog *Watchdog,
	gaps *GapTracker,
	retries *RetryState,
	retryCfg RetryConfig,
	pipeline *Pipeline,
	preset *Preset,
	pluginDir string,
	outputDir string,
	cfg DispatchConfig,
	startPhase int,
	pipelineID string,
	progress ProgressReporter,
) *Dispatcher {
	d := &Dispatcher{
		store:          store,
		pool:           pool,
		lock:           lock,
		log:            log,
		watchdog:       watchdog,
		gaps:           gaps,
		retries:        retries,
		retryCfg:       retryCfg,
		pipeline:       pipeline,
		preset:         preset,
		pluginDir:      pluginDir,
		outputDir:      outputDir,
		cfg:            cfg,
		agentIndex:     BuildAgentIndex(pipeline),
		pipelineID:     pipelineID,
		processedFails: make(map[string]bool),
		progress:       progress,
	}
	d.spawnFunc = d.runAgent
	d.initPhase(startPhase)
	return d
}

// SetSpawnFunc sets a custom spawn function. Used by tests to bypass tmux.
func (d *Dispatcher) SetSpawnFunc(fn SpawnFunc) {
	d.spawnFunc = fn
}

// Wait blocks until all spawned agent goroutines have completed.
// Callers should cancel the context before calling Wait to ensure goroutines exit promptly.
func (d *Dispatcher) Wait() {
	d.agentWg.Wait()
}

// Run starts the dispatch loop. Blocks until pipeline terminal or ctx cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result := d.Tick(ctx)

		if result.PipelineDone {
			return nil
		}
		if result.GateFailed {
			return ErrGateHalt
		}
		if result.GapAbort {
			return ErrPipelineAborted
		}
		if result.Error != nil {
			return result.Error
		}

		ticker := time.NewTicker(d.cfg.Interval)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return ctx.Err()
		case <-ticker.C:
			ticker.Stop()
		}
	}
}

// Tick performs a single dispatch tick. Exported for testing.
func (d *Dispatcher) Tick(ctx context.Context) DispatchResult {
	var result DispatchResult

	// Step 1: Dispatch — find ready tasks, assign, spawn.
	dispatched, err := d.dispatch(ctx)
	if err != nil {
		result.Error = err
		return result
	}
	result.Dispatched = dispatched

	// Step 2: OrphanCk — handle CB-tripped orphans.
	orphans, err := d.orphanCk()
	if err != nil {
		result.Error = err
		return result
	}
	result.OrphansFailed = orphans

	// Step 3: Check — phase completion, retry/gap, gate, advance.
	phaseAdvanced, pipelineDone, gateFailed, gapAbort, err := d.check()
	result.PhaseAdvanced = phaseAdvanced
	result.PipelineDone = pipelineDone
	result.GateFailed = gateFailed
	result.GapAbort = gapAbort
	if err != nil {
		result.Error = err
		return result
	}

	// Step 4: Lock refresh — must be last step of every tick (TLA+ AdvTick).
	if d.lock != nil {
		if err := d.lock.Refresh(d.current.PhaseDef.ID); err != nil {
			result.Error = fmt.Errorf("dispatch: lock refresh: %w", err)
			return result
		}
	}

	return result
}

// initPhase sets the current phase context.
func (d *Dispatcher) initPhase(index int) {
	phase := &d.pipeline.Phases[index]
	d.current = PhaseContext{
		Index:       index,
		PhaseTaskID: d.pipeline.ID + ":" + phase.ID,
		PhaseDef:    phase,
	}
}

// failTaskAndRelease marks a task as failed, emits a dispatch-failure event
// with the reason, and returns its worker to the pool.
//
// The reason is surfaced via EventCrashDetected so dispatch-time failures
// (ValidateInputs, MkdirAll, WriteCrashMarker, SpawnSession) are traceable in
// the event log — without this, the task would show as failed with no
// explanation, and the root cause would be lost.
//
// Store/pool errors during the mark-failed path are suppressed — the dispatch
// loop detects stale state via watchdog/orphanCk.
func (d *Dispatcher) failTaskAndRelease(task *Task, workerName string, reason error) {
	task.Status = StatusFailed
	task.UpdatedAt = time.Now()
	_ = d.store.UpdateTask(task)   //nolint:errcheck // best-effort; watchdog detects stale state
	_ = d.pool.Release(workerName) //nolint:errcheck // best-effort; watchdog detects stale state

	var phase string
	if d.current.PhaseDef != nil {
		phase = d.current.PhaseDef.ID
	}
	ev := Event{
		Kind:    EventCrashDetected,
		Phase:   phase,
		Agent:   task.AgentID,
		TaskID:  task.ID,
		Worker:  workerName,
		Summary: fmt.Sprintf("dispatch failed: %v", reason),
	}
	emitAndNotify(d.log, d.progress, ev)
}

// dispatch finds ready tasks, assigns them to idle workers, and spawns agents. (TLA+ Dispatch)
func (d *Dispatcher) dispatch(ctx context.Context) (int, error) {
	ready, err := d.store.ReadyTasks()
	if err != nil {
		return 0, fmt.Errorf("dispatch: ready tasks: %w", err)
	}

	// Auto-advance the pipeline root and current phase task so their children
	// become unblocked. Only advance if not already completed (avoid no-op writes).
	// CRITICAL: Only advance the current phase — future phases must stay open.
	advanced := false
	for _, task := range ready {
		if task.Status == StatusCompleted {
			continue
		}
		if (task.Type == TypePipeline && task.ID == d.pipelineID) ||
			(task.Type == TypePhase && task.ID == d.current.PhaseTaskID) {
			task.Status = StatusCompleted
			task.UpdatedAt = time.Now()
			if err := d.store.UpdateTask(task); err != nil {
				return 0, fmt.Errorf("dispatch: advance structural task %s: %w", task.ID, err)
			}
			advanced = true
		}
	}

	// Re-fetch only if structural tasks were advanced (new tasks may now be ready).
	if advanced {
		ready, err = d.store.ReadyTasks()
		if err != nil {
			return 0, fmt.Errorf("dispatch: ready tasks (post-advance): %w", err)
		}
	}

	dispatched := 0
	for _, task := range ready {
		// Only dispatch agent-type tasks.
		if task.AgentID == "" {
			continue
		}
		agentDef, ok := d.agentIndex[task.AgentID]
		if !ok {
			// Unknown agent ID — data consistency violation. Fail the task.
			task.Status = StatusFailed
			task.UpdatedAt = time.Now()
			_ = d.store.UpdateTask(task) //nolint:errcheck // best-effort; task stays open → retried next tick
			ev := Event{
				Kind:    EventCrashDetected,
				TaskID:  task.ID,
				Summary: fmt.Sprintf("unknown agent ID %q in pipeline index", task.AgentID),
			}
			emitAndNotify(d.log, d.progress, ev)
			continue
		}

		worker, err := d.pool.Allocate()
		if err != nil {
			break // pool exhausted, try next tick
		}

		if err := d.store.Assign(task.ID, worker.Name); err != nil {
			// Task may have been claimed by another path or store write failed.
			// Both are rare but worth logging so operators can distinguish races
			// from persistent store failures.
			emitAndNotify(d.log, d.progress, Event{
				Kind:    EventCrashDetected,
				Phase:   d.current.PhaseDef.ID,
				Agent:   task.AgentID,
				TaskID:  task.ID,
				Worker:  worker.Name,
				Summary: fmt.Sprintf("assign task failed: %v", err),
			})
			continue
		}

		// Re-read task after Assign to get updated status.
		task, err = d.store.GetTask(task.ID)
		if err != nil {
			emitAndNotify(d.log, d.progress, Event{
				Kind:    EventCrashDetected,
				Phase:   d.current.PhaseDef.ID,
				Agent:   task.AgentID,
				TaskID:  task.ID,
				Worker:  worker.Name,
				Summary: fmt.Sprintf("re-read task after assign failed: %v", err),
			})
			_ = d.pool.Release(worker.Name) //nolint:errcheck // best-effort; worker stays idle
			continue
		}

		// Validate inputs before spawn (OPS-03).
		if err := ValidateInputs(agentDef.Inputs, d.outputDir); err != nil {
			d.failTaskAndRelease(task, worker.Name, fmt.Errorf("validate inputs: %w", err))
			continue
		}

		// Ensure output directory exists and write crash marker (CHK-04).
		// Skip crash marker for consensus verify tasks — they validate the
		// merge output at the same path. Overwriting it would destroy the
		// valid merged content. Resume handles verify tasks via ledger state
		// (in_progress → open reset), so crash markers are not needed.
		outputPath := filepath.Join(d.outputDir, task.Output)
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			d.failTaskAndRelease(task, worker.Name, fmt.Errorf("create output dir %s: %w", filepath.Dir(outputPath), err))
			continue
		}
		if task.Type != TypeConsensusVerify {
			if err := WriteCrashMarker(outputPath); err != nil {
				d.failTaskAndRelease(task, worker.Name, fmt.Errorf("write crash marker: %w", err))
				continue
			}
		}

		// Spawn tmux session.
		if err := d.pool.SpawnSession(worker.Name, task, agentDef, d.preset, d.pluginDir); err != nil {
			d.failTaskAndRelease(task, worker.Name, fmt.Errorf("spawn session: %w", err))
			continue
		}

		// Re-read worker and task: SpawnSession updates SessionStartedAt and task status
		// in the store, but our local pointers are stale from Allocate/GetTask.
		worker, _ = d.store.GetWorker(worker.Name) //nolint:errcheck // SpawnSession succeeded; store read is best-effort
		task, _ = d.store.GetTask(task.ID)         //nolint:errcheck // SpawnSession succeeded; store read is best-effort

		ev := Event{
			Kind:    EventAgentStart,
			Phase:   d.current.PhaseDef.ID,
			Agent:   task.AgentID,
			TaskID:  task.ID,
			Worker:  worker.Name,
			Summary: "agent dispatched",
		}
		emitAndNotify(d.log, d.progress, ev)

		d.agentWg.Add(1)
		go func(t *Task, w *Worker, ad AgentDef) {
			defer d.agentWg.Done()
			d.spawnFunc(ctx, t, w, ad)
		}(task, worker, agentDef)

		dispatched++
	}

	return dispatched, nil
}

// orphanCk handles circuit-breaker-tripped orphans. (TLA+ OrphanCk)
// If CB is tripped, fails one in_progress task and returns the worker to idle.
// Handles both task failure AND worker return atomically to prevent double-return.
func (d *Dispatcher) orphanCk() (int, error) {
	if d.watchdog == nil || !d.watchdog.CBTripped() {
		return 0, nil
	}

	workers, err := d.store.ListWorkers()
	if err != nil {
		return 0, fmt.Errorf("orphanCk: list workers: %w", err)
	}

	failed := 0
	for _, worker := range workers {
		if worker.Status != WorkerActive || worker.CurrentTaskID == "" {
			continue
		}

		task, err := d.store.GetTask(worker.CurrentTaskID)
		if err != nil {
			continue
		}
		if task.Status != StatusInProgress {
			continue
		}

		// Fail the orphaned task.
		task.Status = StatusFailed
		task.UpdatedAt = time.Now()
		if err := d.store.UpdateTask(task); err != nil {
			continue
		}

		// Return worker to idle.
		_ = d.pool.Release(worker.Name) //nolint:errcheck // best-effort; worker leaked → pool exhaustion triggers investigation

		ev := Event{
			Kind:    EventCircuitBreaker,
			Phase:   d.current.PhaseDef.ID,
			TaskID:  task.ID,
			Worker:  worker.Name,
			Summary: "orphaned task failed by OrphanCk",
		}
		emitAndNotify(d.log, d.progress, ev)

		failed++
		break // process one orphan per tick
	}

	d.watchdog.ResetCB()
	return failed, nil
}

// check evaluates phase completion: retry/gap, gate, advance. (TLA+ Check..GateFail) //nolint:cyclop,funlen // maps to multiple TLA+ labels; splitting breaks verified structure
func (d *Dispatcher) check() (phaseAdvanced, pipelineDone, gateFailed, gapAbort bool, err error) {
	children, err := d.store.GetChildren(d.current.PhaseTaskID)
	if err != nil {
		return false, false, false, false, fmt.Errorf("check: get children of %s: %w", d.current.PhaseTaskID, err)
	}

	// Partition children into: gate tasks, output tasks (retry/gap candidates), and all tasks.
	// Output tasks are the final deliverables per agent:
	//   - TypeAgent for sequential/parallel topologies
	//   - TypeConsensusVerify for consensus topologies
	// Instance and merge failures propagate through the consensus protocol naturally.
	var gateAgent string
	if d.current.PhaseDef.Gate != nil {
		gateAgent = d.current.PhaseDef.Gate.Agent
	}

	var nonGateTasks []*Task // all non-gate children (for terminal check)
	var outputTasks []*Task  // only output-bearing tasks (for retry/gap)
	for _, child := range children {
		if child.AgentID == gateAgent && gateAgent != "" {
			continue // skip gate agent
		}
		nonGateTasks = append(nonGateTasks, child)
		if child.Type == TypeAgent || child.Type == TypeConsensusVerify {
			outputTasks = append(outputTasks, child)
		}
	}

	// Are all non-gate tasks terminal?
	for _, t := range nonGateTasks {
		if !t.Status.Terminal() {
			return false, false, false, false, nil // still running
		}
	}

	// All non-gate tasks are terminal. Process failures on output tasks only.
	// Skip tasks already processed on a prior tick (prevents double-counting
	// when gate evaluation returns GatePending across multiple ticks).
	for _, t := range outputTasks {
		if t.Status != StatusFailed {
			continue
		}
		if d.processedFails[t.ID] {
			continue
		}

		agentDef, ok := d.agentIndex[t.AgentID]
		if !ok {
			continue
		}

		if agentDef.Criticality == Critical {
			if d.retries.CanRetry(t.AgentID, d.retryCfg) {
				if err := d.createRetryTask(t, t.AgentID); err != nil {
					return false, false, false, false, err
				}
				d.processedFails[t.ID] = true
				return false, false, false, false, nil // retry created, wait for next tick
			}
			// Critical agent exhausted retries → pipeline terminates (ERR-03).
			return false, false, false, false, fmt.Errorf("check: %w: %s", ErrRetriesExhausted, t.AgentID)
		}

		// Non-critical → record gap (ERR-04). Atomic increment+check (ERR-08, S5).
		d.processedFails[t.ID] = true
		abort := d.gaps.IncrementAndCheck(t.AgentID)

		ev := Event{
			Kind:    EventGapRecorded,
			Phase:   d.current.PhaseDef.ID,
			Agent:   t.AgentID,
			TaskID:  t.ID,
			Summary: fmt.Sprintf("gap %d/%d", d.gaps.Count(), d.pipeline.GapTolerance),
		}
		emitAndNotify(d.log, d.progress, ev)

		if abort {
			return false, false, false, true, nil // pipeline abort (ERR-08)
		}
	}

	// Evaluate gate (EXP-10, S6).
	verdict := EvaluateGate(children, d.current.PhaseDef.Gate)
	if verdict != GateNone {
		ev := Event{
			Kind:    EventGateResult,
			Phase:   d.current.PhaseDef.ID,
			Agent:   gateAgent,
			Summary: fmt.Sprintf("gate verdict: %s", verdict),
		}
		emitAndNotify(d.log, d.progress, ev)
	}
	switch verdict {
	case GatePending:
		return false, false, false, false, nil // gate not dispatched yet
	case GateFail:
		// Find the latest unprocessed failed gate task to use as retry template.
		var failedGateTask *Task
		for _, child := range children {
			if child.AgentID == gateAgent && child.Status == StatusFailed && !d.processedFails[child.ID] {
				failedGateTask = child
			}
		}
		if failedGateTask == nil {
			// All gate failures already processed and no retry pending — terminal.
			return false, false, true, false, nil // pipeline terminal (S6)
		}
		// Check retries for gate agent.
		if d.retries.CanRetry(gateAgent, d.retryCfg) {
			if err := d.createRetryTask(failedGateTask, gateAgent); err != nil {
				return false, false, false, false, err
			}
			d.processedFails[failedGateTask.ID] = true
			return false, false, false, false, nil
		}
		d.processedFails[failedGateTask.ID] = true
		return false, false, true, false, nil // pipeline terminal (S6)
	case GateNone, GatePass:
		// continue to phase advancement
	}

	// Phase complete. Store health check via DeriveParentStatus (LDG-16..18).
	// Status is NOT persisted — the phase task was already auto-completed in dispatch()
	// to unblock children. Overwriting with StatusFailed would break ReadyTasks deps.
	_, err = DeriveParentStatus(d.store, d.current.PhaseTaskID)
	if err != nil {
		return false, false, false, false, fmt.Errorf("check: derive phase status: %w", err)
	}

	ev := Event{
		Kind:    EventPhaseComplete,
		Phase:   d.current.PhaseDef.ID,
		Summary: fmt.Sprintf("phase %d/%d complete", d.current.Index+1, len(d.pipeline.Phases)),
	}
	emitAndNotify(d.log, d.progress, ev)

	// Last phase → pipeline done.
	if d.current.Index >= len(d.pipeline.Phases)-1 {
		_, _ = DeriveParentStatus(d.store, d.pipelineID) //nolint:errcheck // store health check; pipeline is done
		ev = Event{
			Kind:    EventPipelineComplete,
			Summary: fmt.Sprintf("pipeline complete (%d gaps)", d.gaps.Count()),
		}
		emitAndNotify(d.log, d.progress, ev)
		return true, true, false, false, nil
	}

	// Advance to next phase (S1 monotonic progress).
	d.initPhase(d.current.Index + 1)
	d.processedFails = make(map[string]bool) // clear — prior phase IDs are unreachable

	ev = Event{
		Kind:    EventPhaseStart,
		Phase:   d.current.PhaseDef.ID,
		Summary: fmt.Sprintf("phase %d/%d started", d.current.Index+1, len(d.pipeline.Phases)),
	}
	emitAndNotify(d.log, d.progress, ev)

	return true, false, false, false, nil
}

// createRetryTask creates a retry task from a failed template task, copies its dependencies,
// and emits an EventRetryScheduled log event. Shared by output-task and gate retry paths.
func (d *Dispatcher) createRetryTask(failedTask *Task, agentID string) error {
	// Compute the next attempt number without mutating state. RecordAttempt is
	// deferred until a retry is actually dispatched (new or reset), so completed
	// retries from prior runs don't consume the retry budget.
	attempt := d.retries.AttemptCount(agentID) + 1
	retryID := RetryTaskID(agentID, attempt)

	retryTask := &Task{
		ID:       retryID,
		ParentID: d.current.PhaseTaskID,
		Type:     failedTask.Type,
		Status:   StatusOpen,
		AgentID:  agentID,
		Output:   failedTask.Output,
		Priority: failedTask.Priority,
	}
	if err := d.store.CreateTask(retryTask); errors.Is(err, ErrTaskExists) {
		// On resume, the retry task may already exist from a prior run.
		existing, getErr := d.store.GetTask(retryID)
		if getErr != nil {
			return fmt.Errorf("check: get existing retry task %s: %w", retryID, getErr)
		}
		if existing.Status == StatusCompleted {
			return nil // prior retry succeeded — no re-dispatch needed
		}
		if resetErr := resetToOpen(d.store, existing); resetErr != nil {
			return fmt.Errorf("check: reset existing retry task %s: %w", retryID, resetErr)
		}
	} else if err != nil {
		return fmt.Errorf("check: create retry task %s: %w", retryID, err)
	}

	// Record attempt only after confirming a retry will be dispatched.
	d.retries.RecordAttempt(agentID)

	// Copy dependencies from the original task (best-effort — retry runs without deps if copy fails).
	deps, _ := d.store.GetDeps(failedTask.ID) //nolint:errcheck // best-effort dep copy
	for _, dep := range deps {
		_ = d.store.AddDep(retryID, dep) //nolint:errcheck // best-effort dep copy
	}

	ev := Event{
		Kind:    EventRetryScheduled,
		Phase:   d.current.PhaseDef.ID,
		Agent:   agentID,
		TaskID:  retryID,
		Summary: fmt.Sprintf("retry %d/%d", attempt, d.retryCfg.MaxRetries),
	}
	emitAndNotify(d.log, d.progress, ev)
	return nil
}

// runAgent monitors a tmux session until it exits, validates output, updates task status,
// and releases the worker. This is the default SpawnFunc used in production.
func (d *Dispatcher) runAgent(ctx context.Context, task *Task, worker *Worker, agentDef AgentDef) {
	// Poll until session dies. Default 500ms — must be significantly faster
	// than the watchdog interval (30s default) so the agent goroutine processes
	// session death before the watchdog classifies it as a rapid failure.
	pollInterval := d.cfg.AgentPollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return // shutdown — ledger state intentionally preserved for resume (RCV-14)
		case <-ticker.C:
		}
		alive, err := d.pool.IsAlive(worker.Name)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= 20 {
				// tmux infrastructure permanently broken after 10s of failures.
				break
			}
			continue
		}
		consecutiveErrors = 0
		if !alive {
			break
		}
	}

	// Read exit code from sidecar file. -1 means the sidecar was never written
	// (agent killed before bash could write it).
	logPath := LogPath(d.outputDir, task.ID)
	exitCode, err := ReadExitCode(logPath)
	if err != nil {
		exitCode = -1
	}

	// Validate output (OPS-04).
	outputPath := filepath.Join(d.outputDir, task.Output)
	outputErr := ValidateOutput(outputPath, agentDef.Format)

	// Check crash marker.
	if outputErr != nil {
		if isCrash, _ := IsCrashMarker(outputPath); isCrash { //nolint:errcheck // crash detection is best-effort
			exitCode = 1
		}
	}

	// If exit code is unknown (-1, sidecar not yet fsynced) but output is valid,
	// the agent succeeded — the sidecar race is benign. For killed agents, the
	// output will be invalid (crash marker or missing), so they still fail.
	if exitCode < 0 && outputErr == nil {
		exitCode = 0
	}

	// Determine outcome (OPS-05).
	outcome := DetermineOutcome(exitCode, outputErr, agentDef.Criticality)

	switch outcome {
	case OutcomeCompleted:
		task.Status = StatusCompleted
	case OutcomeRetry, OutcomeGap, OutcomeFailed:
		task.Status = StatusFailed
	}
	task.UpdatedAt = time.Now()

	// Record rapid failure with circuit breaker (SUP-05). The agent goroutine
	// detects session death faster than the watchdog (500ms poll vs 30s default),
	// so it must contribute to CB counting to catch rapid crash loops.
	if outcome != OutcomeCompleted && d.watchdog != nil {
		if d.watchdog.RecordAgentFailure(worker.Name, worker.SessionStartedAt) {
			ev := Event{
				Kind:    EventCircuitBreaker,
				Worker:  worker.Name,
				TaskID:  task.ID,
				Summary: "circuit breaker tripped (rapid agent failure)",
			}
			emitAndNotify(d.log, d.progress, ev)
		}
	}

	if err := d.store.UpdateTask(task); err != nil {
		// Task status not persisted — leave worker active so watchdog can detect
		// the dead session and trigger recovery.
		ev := Event{
			Kind:    EventCrashDetected,
			TaskID:  task.ID,
			Worker:  worker.Name,
			Summary: fmt.Sprintf("failed to persist task outcome: %v", err),
		}
		emitAndNotify(d.log, d.progress, ev)
		return
	}

	// Release worker.
	if err := d.pool.Release(worker.Name); err != nil {
		ev := Event{
			Kind:    EventCrashDetected,
			TaskID:  task.ID,
			Worker:  worker.Name,
			Summary: fmt.Sprintf("failed to release worker: %v", err),
		}
		emitAndNotify(d.log, d.progress, ev)
	}

	// Derive phase ID from task parent (format: "{pipelineID}:{phaseID}") instead of
	// reading d.current which may have advanced to the next phase concurrently.
	phaseID := strings.TrimPrefix(task.ParentID, d.pipelineID+":")

	ev := Event{
		Kind:    EventAgentComplete,
		Phase:   phaseID,
		Agent:   task.AgentID,
		TaskID:  task.ID,
		Worker:  worker.Name,
		Summary: fmt.Sprintf("outcome=%s exit=%d", outcome, exitCode),
		Detail:  task.Output,
		Outcome: outcome,
	}
	emitAndNotify(d.log, d.progress, ev)
}
