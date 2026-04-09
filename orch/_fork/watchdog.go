package orch

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CircuitBreaker tracks rapid failures and trips when the threshold is reached (SUP-05, SUP-06).
// Thread-safe — written by the watchdog goroutine, read by the dispatch loop's OrphanCk step.
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
// the session lived less than the window duration (SUP-06). Returns true if the
// circuit breaker has tripped (SUP-05).
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

// Reset clears the tripped state. Called by the dispatch loop's OrphanCk after processing.
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

// Watchdog supervises agent session liveness on a fixed cycle (SUP-01).
//
// Responsibilities (per TLA+ Watchdog process):
//   - Check all active workers via tmux has-session (OBS-02a)
//   - Restart dead sessions unless CB tripped (SUP-02, SUP-03)
//   - Track rapid failures and trip CB (SUP-05, SUP-06)
//   - Set CB flag only — does NOT change task status (SUP-04, S11)
type Watchdog struct {
	pool       *WorkerPool
	store      Store
	log        *EventLog
	cb         *CircuitBreaker
	cfg        WatchdogConfig
	preset     *Preset
	pluginDir  string
	agentIndex map[string]AgentDef
	progress   ProgressReporter
}

// NewWatchdog creates a watchdog with the given dependencies.
func NewWatchdog(
	pool *WorkerPool,
	store Store,
	log *EventLog,
	pipeline *Pipeline,
	preset *Preset,
	pluginDir string,
	cfg WatchdogConfig,
	progress ProgressReporter,
) *Watchdog {
	return &Watchdog{
		pool:       pool,
		store:      store,
		log:        log,
		cb:         NewCircuitBreaker(cfg.CBThreshold, cfg.CBWindow),
		cfg:        cfg,
		preset:     preset,
		pluginDir:  pluginDir,
		agentIndex: BuildAgentIndex(pipeline),
		progress:   progress,
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

		// Check if the agent goroutine already handled this death (task is terminal).
		// This avoids counting normal completions as rapid failures.
		if worker.CurrentTaskID != "" {
			if task, taskErr := w.store.GetTask(worker.CurrentTaskID); taskErr == nil && task.Status.Terminal() {
				continue // agent goroutine handled it — not a watchdog concern
			}
		}

		// Session is dead and task is not terminal (SUP-02, SUP-03).
		ev := Event{
			Kind:    EventCrashDetected,
			Worker:  worker.Name,
			TaskID:  worker.CurrentTaskID,
			Summary: "session dead, task not terminal",
		}
		emitAndNotify(w.log, w.progress, ev)

		tripped := w.cb.RecordFailure(worker.Name, worker.SessionStartedAt)

		if tripped {
			// CB tripped — do NOT restart, do NOT change task status (SUP-04).
			// The dispatch loop's OrphanCk will handle the orphaned task.
			ev = Event{
				Kind:    EventCircuitBreaker,
				Worker:  worker.Name,
				TaskID:  worker.CurrentTaskID,
				Summary: "circuit breaker tripped",
			}
			emitAndNotify(w.log, w.progress, ev)
			continue
		}

		// Restart session (SUP-03). Requires task → agentDef lookup.
		task, err := w.store.GetTask(worker.CurrentTaskID)
		if err != nil {
			continue
		}
		agentDef, ok := w.agentIndex[task.AgentID]
		if !ok {
			continue
		}

		ev = Event{
			Kind:    EventSessionRestart,
			Worker:  worker.Name,
			TaskID:  task.ID,
			Agent:   task.AgentID,
			Summary: "restarting dead session",
		}
		emitAndNotify(w.log, w.progress, ev)

		_ = w.pool.RestartSession(worker.Name, task, agentDef, w.preset, w.pluginDir)
	}

	return nil
}

// CBTripped returns whether the circuit breaker is tripped.
// Called by the dispatch loop's OrphanCk step.
func (w *Watchdog) CBTripped() bool {
	return w.cb.Tripped()
}

// RecordAgentFailure records a failure detected by the agent goroutine with the
// circuit breaker. Returns true if the CB has tripped. This allows rapid failures
// to trip the CB even when the agent goroutine handles them before the watchdog.
func (w *Watchdog) RecordAgentFailure(workerName string, sessionStart time.Time) bool {
	return w.cb.RecordFailure(workerName, sessionStart)
}

// ResetCB clears the circuit breaker. Called by the dispatch loop after OrphanCk.
func (w *Watchdog) ResetCB() {
	w.cb.Reset()
}
