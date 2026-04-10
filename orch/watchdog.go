package orch

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CircuitBreaker tracks rapid failures and trips when the threshold is
// reached (BVV SUP-05, SUP-06). Thread-safe — written by the watchdog
// goroutine, read by the dispatch loop when it checks CBTripped.
type CircuitBreaker struct {
	mu        sync.Mutex
	threshold int                    // consecutive rapid failures before trip (SUP-05: 3)
	window    time.Duration          // rapid failure window (SUP-06: 60s)
	failures  map[string][]time.Time // workerName → failure timestamps
	tripped   bool
}

// NewCircuitBreaker creates a circuit breaker with the given threshold and window.
func NewCircuitBreaker(threshold int, window time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: threshold,
		window:    window,
		failures:  make(map[string][]time.Time),
	}
}

// RecordFailure records a rapid failure for a worker. A failure is "rapid" if
// the session lived less than the window duration (SUP-06). Returns true if
// the circuit breaker has tripped (SUP-05).
func (cb *CircuitBreaker) RecordFailure(workerName string, sessionStart time.Time) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	if now.Sub(sessionStart) >= cb.window {
		return cb.tripped // not a rapid failure
	}

	cb.failures[workerName] = append(cb.failures[workerName], now)

	// Count recent rapid failures across all workers.
	total := 0
	cutoff := now.Add(-cb.window)
	for name, times := range cb.failures {
		var recent []time.Time
		for _, t := range times {
			if t.After(cutoff) {
				recent = append(recent, t)
			}
		}
		cb.failures[name] = recent
		total += len(recent)
	}

	if total >= cb.threshold {
		cb.tripped = true
	}
	return cb.tripped
}

// Tripped returns whether the circuit breaker is currently tripped.
func (cb *CircuitBreaker) Tripped() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.tripped
}

// Reset clears the tripped state. Called by the dispatch loop after
// processing orphaned tasks.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.tripped = false
	cb.failures = make(map[string][]time.Time)
}

// WatchdogConfig configures the watchdog process.
type WatchdogConfig struct {
	Interval    time.Duration // check interval (SUP-01: 30s)
	CBThreshold int           // circuit breaker threshold (SUP-05: 3)
	CBWindow    time.Duration // rapid failure window (SUP-06: 60s)
}

// DefaultWatchdogConfig returns the recommended configuration.
func DefaultWatchdogConfig() WatchdogConfig {
	return WatchdogConfig{
		Interval:    30 * time.Second,
		CBThreshold: 3,
		CBWindow:    60 * time.Second,
	}
}

// Watchdog supervises agent session liveness on a fixed cycle (SUP-01,
// BVV-ERR-11).
//
// Responsibilities:
//   - Check all active workers via tmux has-session (BVV-ERR-11)
//   - On dead sessions: check HandoffState, restart if budget remains
//     (BVV-ERR-11a, BVV-L-04), or emit EventHandoffLimitReached and skip
//     restart if the handoff budget is exhausted.
//   - Track rapid failures via CircuitBreaker (SUP-05, SUP-06)
//   - **Never change task status** (BVV-S-10). The dispatcher is the sole
//     owner of task status transitions. The watchdog signals via events
//     and HandoffState mutations; the dispatcher observes those and
//     transitions tasks on its next tick.
type Watchdog struct {
	pool     *WorkerPool
	store    Store
	log      *EventLog
	cb       *CircuitBreaker
	cfg      WatchdogConfig
	roles    map[string]RoleConfig // role tag → (instruction file, preset)
	handoffs *HandoffState         // BVV-ERR-11a / BVV-L-04
	branch   string                // lifecycle scoping for session naming
	progress ProgressReporter
}

// NewWatchdog creates a watchdog with the given dependencies. The roles map
// replaces the fork's pipeline + agentIndex pair — BVV routes dead sessions
// via the task's role label, not a pipeline-derived agent index. HandoffState
// tracks per-task handoff budget; the watchdog is one of its two writers
// (the dispatcher is the other, on exit-code-3 processing).
func NewWatchdog(
	pool *WorkerPool,
	store Store,
	log *EventLog,
	roles map[string]RoleConfig,
	handoffs *HandoffState,
	branch string,
	cfg WatchdogConfig,
	progress ProgressReporter,
) *Watchdog {
	return &Watchdog{
		pool:     pool,
		store:    store,
		log:      log,
		cb:       NewCircuitBreaker(cfg.CBThreshold, cfg.CBWindow),
		cfg:      cfg,
		roles:    roles,
		handoffs: handoffs,
		branch:   branch,
		progress: progress,
	}
}

// Run starts the watchdog loop. Blocks until ctx is cancelled.
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.CheckOnce()
		}
	}
}

// CheckOnce performs a single watchdog check cycle. Exported for testing.
//
// For each active worker with a dead tmux session:
//  1. Skip if the task is already terminal (dispatcher handled it).
//  2. If handoff budget is exhausted → emit EventHandoffLimitReached and
//     leave the task in place. The dispatcher will observe the event on
//     its next tick, fetch the task, and transition it to Failed.
//  3. Otherwise increment HandoffState, emit EventTaskHandoff, and call
//     RestartSession. BVV-S-10: the watchdog never mutates task status
//     directly; only events and counters change.
func (w *Watchdog) CheckOnce() error {
	workers, err := w.store.ListWorkers()
	if err != nil {
		return fmt.Errorf("watchdog: list workers: %w", err)
	}

	for _, worker := range workers {
		if worker.Status != WorkerActive {
			continue
		}

		alive, err := w.pool.IsAlive(worker.Name)
		if err != nil {
			continue // tmux infra error, skip this worker
		}
		if alive {
			continue
		}

		// Skip if the agent goroutine (dispatcher) already handled the death
		// and the task is terminal — avoids double-counting a normal completion
		// as a watchdog-triggered handoff.
		if worker.CurrentTaskID == "" {
			continue
		}
		task, err := w.store.GetTask(worker.CurrentTaskID)
		if err != nil {
			continue
		}
		if task.Status.Terminal() {
			continue // BVV-S-10: never touch terminal tasks
		}

		// Resolve role → RoleConfig. Unknown role is the dispatcher's problem
		// (it creates escalation tasks per BVV-DSP-03a); the watchdog skips.
		role := task.Role()
		roleCfg, ok := w.roles[role]
		if !ok {
			continue
		}

		// BVV-L-04: check handoff budget BEFORE any mutation or event.
		if !w.handoffs.CanHandoff(task.ID) {
			// Budget exhausted. Emit the signal event and leave the task in
			// place. The dispatcher will observe EventHandoffLimitReached on
			// its next tick and convert the task to a failure (BVV-ERR-11a).
			//
			// Emit errors are best-effort here: a failed write means disk
			// full or log closed, which the dispatcher's next tick will
			// surface through other code paths. The BVV-S-10 invariant is
			// still respected — no task status mutation happens in this arm.
			_ = emitAndNotify(w.log, w.progress, Event{
				Kind:    EventHandoffLimitReached,
				TaskID:  task.ID,
				Worker:  worker.Name,
				Summary: "watchdog handoff limit reached",
				Detail:  fmt.Sprintf("branch=%s role=%s", w.branch, role),
			})
			continue
		}

		// Record the handoff FIRST so the emitted count reflects the new
		// state. The dispatcher's exit-3 path uses the same order for
		// consistency.
		w.handoffs.RecordHandoff(task.ID)
		count := w.handoffs.Count(task.ID)

		_ = emitAndNotify(w.log, w.progress, Event{
			Kind:    EventTaskHandoff,
			TaskID:  task.ID,
			Worker:  worker.Name,
			Summary: fmt.Sprintf("watchdog restart (handoff %d)", count),
			Detail:  fmt.Sprintf("reason=session_dead branch=%s role=%s count=%d", w.branch, role, count),
		})

		// Track rapid failures for circuit-breaker purposes. A tripped CB
		// does NOT skip the restart — the watchdog still attempts to recover.
		// CBTripped signals the dispatcher (via CheckOnce's caller) that the
		// system is unhealthy; the dispatcher decides whether to halt.
		_ = w.cb.RecordFailure(worker.Name, worker.SessionStartedAt)

		// Best-effort restart. A restart failure is tracked by the circuit
		// breaker on the next tick when IsAlive returns false again. We do
		// NOT touch task status here — BVV-S-10 forbids it.
		_ = w.pool.RestartSession(worker.Name, task, roleCfg, w.branch)
	}

	return nil
}

// CBTripped returns whether the circuit breaker is tripped.
// The dispatch loop polls this to decide whether to halt.
func (w *Watchdog) CBTripped() bool {
	return w.cb.Tripped()
}

// RecordAgentFailure records a failure detected by the dispatcher's outcome
// processor with the circuit breaker. Returns true if the CB has tripped.
// This allows rapid dispatcher-observed failures to trip the CB even when
// the watchdog tick hasn't seen them yet.
func (w *Watchdog) RecordAgentFailure(workerName string, sessionStart time.Time) bool {
	return w.cb.RecordFailure(workerName, sessionStart)
}

// ResetCB clears the circuit breaker. Called by the dispatch loop after
// successfully processing orphaned tasks.
func (w *Watchdog) ResetCB() {
	w.cb.Reset()
}
