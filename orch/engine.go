package orch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// EngineConfig configures the Engine for a lifecycle run.
// All role/branch configuration lives in Lifecycle — no Pipeline type.
type EngineConfig struct {
	MaxWorkers int
	Lifecycle  *LifecycleConfig // branch, roles, gap tolerance, retries, handoffs, timeout, lock
	RunDir     string           // output directory (logs, event log, ledger)
	RepoPath   string           // repository being orchestrated
	RunID      string           // unique run identifier (generated if empty)
	LedgerKind LedgerKind       // store backend; see NewStore for default and fallback
	Dispatch   DispatchConfig
	Watchdog   WatchdogConfig
	Progress      ProgressReporter // nil = no-op
	TestSpawnFunc SpawnFunc        // test override: bypasses tmux (nil = production runAgent)
}

// DefaultEngineConfig returns sensible defaults with the given required parameters.
func DefaultEngineConfig(lifecycle *LifecycleConfig, runDir, repoPath string) EngineConfig {
	return EngineConfig{
		MaxWorkers: 4,
		Lifecycle:  lifecycle,
		RunDir:     runDir,
		RepoPath:   repoPath,
		Dispatch:   DefaultDispatchConfig(),
		Watchdog:   DefaultWatchdogConfig(),
	}
}

// Engine wires dispatch, watchdog, recovery, and supporting components for a
// single lifecycle run. Created by NewEngine; executed by Run (fresh) or
// Resume (interrupted).
type Engine struct {
	cfg      EngineConfig
	store    Store
	pool     *WorkerPool
	tmux     *TmuxClient
	lock     *LifecycleLock
	log      *EventLog
	watchdog *Watchdog
	disp     *Dispatcher
	gaps     *GapTracker
	retries  *RetryState
	handoffs *HandoffState
}

// NewEngine validates the config and returns an uninitialised Engine shell.
// Call Run or Resume to start execution.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	switch {
	case cfg.Lifecycle == nil:
		return nil, fmt.Errorf("engine: lifecycle config is required")
	case cfg.Lifecycle.Branch == "":
		return nil, fmt.Errorf("engine: lifecycle.Branch must be non-empty")
	case cfg.RunDir == "":
		return nil, fmt.Errorf("engine: RunDir is required")
	case cfg.RepoPath == "":
		return nil, fmt.Errorf("engine: RepoPath is required")
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.RunID == "" {
		cfg.RunID = generateRunID()
	}
	return &Engine{cfg: cfg}, nil
}

// Store returns the lifecycle store. Nil before Run/Resume initialises it.
func (e *Engine) Store() Store { return e.store }

// EventLogPath returns the event log file path. Empty before initialisation.
func (e *Engine) EventLogPath() string {
	if e.log == nil {
		return ""
	}
	return e.log.Path()
}

// RunID returns the engine's run identifier.
func (e *Engine) RunID() string { return e.cfg.RunID }

// retryConfig derives a RetryConfig from the LifecycleConfig.
// LifecycleConfig is the single source of truth for retry parameters.
func (e *Engine) retryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:  e.cfg.Lifecycle.MaxRetries,
		BaseTimeout: e.cfg.Lifecycle.BaseTimeout,
	}
}

// Run executes a fresh lifecycle (BVV §8).
// The ledger must be pre-populated (by planner or human); there is no Expand().
func (e *Engine) Run(ctx context.Context) error {
	// 1. Initialise infrastructure.
	if err := e.init(); err != nil {
		return err
	}

	// 2. Acquire lifecycle lock (BVV-S-01).
	if err := e.lock.Acquire(e.cfg.RunID, e.cfg.Lifecycle.Branch); err != nil {
		Cleanup(e.tmux, nil, e.log, e.store)
		return fmt.Errorf("engine: %w", err)
	}

	// 3. Create state machines.
	e.gaps = NewGapTracker(e.cfg.Lifecycle.GapTolerance)
	e.retries = NewRetryState()
	e.handoffs = NewHandoffState(e.cfg.Lifecycle.MaxHandoffs)

	// 4. Create watchdog + dispatcher.
	if err := e.buildDispatchAndWatchdog(); err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return err
	}

	// 5. Emit lifecycle_started.
	_ = emitAndNotify(e.log, e.cfg.Progress, Event{
		Kind:    EventLifecycleStarted,
		Summary: fmt.Sprintf("lifecycle started (run %s, branch %s)", e.cfg.RunID, e.cfg.Lifecycle.Branch),
	})

	// 6. Run dispatch loop.
	return e.runLoop(ctx)
}

// Resume re-enters execution from persisted state (BVV-ERR-06..08).
// Recovers gap/retry/handoff counters from the event log, reconciles stale
// assignments, and resumes the dispatch loop.
func (e *Engine) Resume(ctx context.Context) error {
	// 1. Initialise infrastructure (verifies ledger, recovers previous RunID).
	if err := e.initForResume(); err != nil {
		return err
	}

	// 2. Acquire lifecycle lock (BVV-ERR-06: staleness recovery).
	if err := e.lock.Acquire(e.cfg.RunID, e.cfg.Lifecycle.Branch); err != nil {
		Cleanup(e.tmux, nil, e.log, e.store)
		return fmt.Errorf("engine: %w", err)
	}

	// 3. Reconcile state (BVV-ERR-07: must complete before dispatch).
	result, err := Reconcile(e.store, e.tmux, e.cfg.Lifecycle.Branch, e.log.Path())
	if err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return fmt.Errorf("engine: reconcile: %w", err)
	}

	// 4. Create state machines with recovered state.
	e.gaps = NewGapTracker(e.cfg.Lifecycle.GapTolerance)
	e.gaps.SetGaps(result.GapsRecovered)

	e.retries = NewRetryState()
	e.retries.SetCounts(result.RetriesRecovered)

	e.handoffs = NewHandoffState(e.cfg.Lifecycle.MaxHandoffs)
	e.handoffs.SetCounts(result.HandoffsRecovered)

	// BVV-S-02a: human re-opens reset retry+handoff counters.
	for _, taskID := range result.HumanReopens {
		e.retries.ResetTask(taskID)
		e.handoffs.Reset(taskID)
		_ = emitAndNotify(e.log, e.cfg.Progress, Event{
			Kind:    EventEscalationResolved,
			TaskID:  taskID,
			Summary: fmt.Sprintf("human re-open detected for task %s — counters reset", taskID),
		})
	}

	// 5. Create watchdog + dispatcher.
	if err := e.buildDispatchAndWatchdog(); err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return err
	}

	// 6. Emit lifecycle_started with resume detail.
	_ = emitAndNotify(e.log, e.cfg.Progress, Event{
		Kind: EventLifecycleStarted,
		Summary: fmt.Sprintf("lifecycle resumed (run %s, branch %s, reconciled=%d, orphans=%d, gaps=%d, reopens=%d)",
			e.cfg.RunID, e.cfg.Lifecycle.Branch,
			result.Reconciled, result.OrphanedSessions,
			len(result.GapsRecovered), len(result.HumanReopens)),
	})

	// 7. Run dispatch loop.
	return e.runLoop(ctx)
}

// --- Internal infrastructure ---

// init creates directories and shared infrastructure for a fresh run.
func (e *Engine) init() error {
	ledgerDir := filepath.Join(e.cfg.RunDir, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		return fmt.Errorf("engine: create ledger dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(e.cfg.RunDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("engine: create logs dir: %w", err)
	}
	return e.initCommon(ledgerDir)
}

// initForResume verifies the ledger exists, recovers the previous RunID from
// the stale lock file (BVV-ERR-08: tmux socket reconnection), and opens
// shared infrastructure.
func (e *Engine) initForResume() error {
	ledgerDir := filepath.Join(e.cfg.RunDir, "ledger")
	if _, err := os.Stat(ledgerDir); err != nil {
		return fmt.Errorf("engine: %w: %v", ErrResumeNoLedger, err)
	}

	// Recover previous RunID from stale lock file so we can reconnect to
	// the surviving tmux socket and detect live sessions (BVV-ERR-08).
	lockPath := e.lockPath()
	if prev := ReadHolder(lockPath); prev != "" {
		e.cfg.RunID = prev
	}

	return e.initCommon(ledgerDir)
}

// initCommon opens store, event log, tmux, lock, and pool.
func (e *Engine) initCommon(ledgerDir string) error {
	// 1. Open store.
	store, kind, err := NewStore(e.cfg.LedgerKind, ledgerDir)
	if err != nil {
		return fmt.Errorf("engine: open store: %w", err)
	}
	e.store = store
	if e.cfg.LedgerKind != "" && kind != e.cfg.LedgerKind {
		fmt.Fprintf(os.Stderr, "warning: store fallback: requested %s, using %s\n", e.cfg.LedgerKind, kind)
	}

	// 2. Open event log.
	logPath := filepath.Join(e.cfg.RunDir, "events.jsonl")
	log, err := NewEventLog(logPath)
	if err != nil {
		e.store.Close()
		return fmt.Errorf("engine: open event log: %w", err)
	}
	e.log = log

	// 3. Create and start tmux.
	e.tmux = NewTmuxClient(e.cfg.RunID)
	if err := e.tmux.StartServer(); err != nil {
		e.log.Close()
		e.store.Close()
		return fmt.Errorf("engine: start tmux server: %w", err)
	}

	// 4. Create lifecycle lock — path resolved via lockPath() (single source).
	lockCfg := e.cfg.Lifecycle.Lock
	lockCfg.Path = e.lockPath()
	if err := os.MkdirAll(filepath.Dir(lockCfg.Path), 0o755); err != nil {
		return fmt.Errorf("engine: create lock dir: %w", err)
	}
	e.lock = NewLifecycleLock(lockCfg)

	// 5. Create worker pool.
	e.pool = NewWorkerPool(e.store, e.tmux, e.cfg.MaxWorkers,
		e.cfg.RunID, e.cfg.RepoPath, e.cfg.RunDir)

	return nil
}

// buildDispatchAndWatchdog creates the watchdog and dispatcher from current state.
func (e *Engine) buildDispatchAndWatchdog() error {
	watchdog, err := NewWatchdog(
		e.pool, e.store, e.log,
		e.cfg.Lifecycle.Roles, e.handoffs,
		e.cfg.Lifecycle.Branch, e.cfg.Watchdog, e.cfg.Progress,
	)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	e.watchdog = watchdog

	disp, err := NewDispatcher(
		e.store, e.pool, e.lock, e.log, e.watchdog,
		e.gaps, e.retries, e.handoffs,
		e.retryConfig(), e.cfg.Lifecycle, e.cfg.Dispatch, e.cfg.Progress,
	)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if e.cfg.TestSpawnFunc != nil {
		disp.SetSpawnFunc(e.cfg.TestSpawnFunc)
	}
	e.disp = disp

	return nil
}

// runLoop starts watchdog + dispatch, blocks until terminal, then cleans up.
func (e *Engine) runLoop(ctx context.Context) error {
	// 1. Setup signal handler (BVV-ERR-09).
	sigCtx, sigCancel := SetupSignalHandler()
	defer sigCancel()

	// 2. Merge parent context with signal context.
	mergedCtx, mergedCancel := context.WithCancel(ctx)
	defer mergedCancel()
	go func() {
		select {
		case <-sigCtx.Done():
			mergedCancel()
		case <-mergedCtx.Done():
		}
	}()

	// 3. Start watchdog goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.watchdog.Run(mergedCtx)
	}()

	// 4. Run dispatch loop (blocks until LifecycleDone, GapAbort, or ctx cancel).
	err := e.disp.Run(mergedCtx)

	// 5. Cancel watchdog + wait for all goroutines.
	mergedCancel()
	e.disp.Wait()
	wg.Wait()

	// 6. Voluntary exit: assert drain precondition (BVV-ERR-10a) and emit event.
	// BVV-ERR-09: signal path (ctx.Err()) — no status modifications, no event.
	if err == nil || errors.Is(err, ErrLifecycleAborted) {
		AssertLifecycleReleaseDrained(e.store)
		_ = emitAndNotify(e.log, e.cfg.Progress, Event{
			Kind:    EventLifecycleCompleted,
			Summary: fmt.Sprintf("lifecycle completed (run %s, branch %s)", e.cfg.RunID, e.cfg.Lifecycle.Branch),
		})
	}

	// 7. Cleanup (releases lock, kills tmux, closes log+store).
	Cleanup(e.tmux, e.lock, e.log, e.store)

	return err
}

// lockPath returns the default lock file path for the current branch.
func (e *Engine) lockPath() string {
	if e.cfg.Lifecycle.Lock.Path != "" {
		return e.cfg.Lifecycle.Lock.Path
	}
	return filepath.Join(e.cfg.RunDir,
		fmt.Sprintf(".wonka-%s.lock", e.cfg.Lifecycle.Branch))
}

// generateRunID produces a short random hex string for run identification.
func generateRunID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
