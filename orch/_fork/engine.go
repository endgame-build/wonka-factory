package orch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// EngineConfig configures the engine.
type EngineConfig struct {
	MaxWorkers      int
	Preset          *Preset
	PluginDir       string
	RunDir          string     // output directory (logs, event log, ledger)
	RepoPath        string     // repository being analysed
	RunID           string     // unique run identifier (generated if empty)
	Branch          string     // git branch (informational)
	LedgerKind      LedgerKind // store backend; see NewStore for default and fallback semantics
	Dispatch        DispatchConfig
	Watchdog        WatchdogConfig
	Retry           RetryConfig
	Progress        ProgressReporter                  // nil = no progress reporting (default)
	PromptTransform func(agentID, body string) string // optional: rewrite agent prompt before injection; must not panic
}

// DefaultEngineConfig returns defaults with the given required parameters.
func DefaultEngineConfig(preset *Preset, pluginDir, runDir, repoPath string) EngineConfig {
	return EngineConfig{
		MaxWorkers: 4,
		Preset:     preset,
		PluginDir:  pluginDir,
		RunDir:     runDir,
		RepoPath:   repoPath,
		RunID:      uuid.NewString()[:8],
		Dispatch:   DefaultDispatchConfig(),
		Watchdog:   DefaultWatchdogConfig(),
		Retry:      RetryConfig{MaxRetries: 2, BaseTimeout: 30 * time.Second},
	}
}

// Engine wires dispatch, watchdog, recovery, and supporting components.
type Engine struct {
	cfg      EngineConfig
	store    Store
	pool     *WorkerPool
	tmux     *TmuxClient
	lock     *PipelineLock
	log      *EventLog
	watchdog *Watchdog
	dispatch *Dispatcher
	gaps     *GapTracker
	retries  *RetryState
	pipeline *Pipeline
	progress ProgressReporter
}

// NewEngine constructs the engine shell. Does NOT start execution — call Run or Resume.
func NewEngine(pipeline *Pipeline, cfg EngineConfig) (*Engine, error) {
	if cfg.RunID == "" {
		cfg.RunID = uuid.NewString()[:8]
	}

	return &Engine{
		cfg:      cfg,
		pipeline: pipeline,
		progress: cfg.Progress,
	}, nil
}

// Run executes the pipeline from scratch (OPS-01).
func (e *Engine) Run(ctx context.Context) error {
	// 1. Initialise infrastructure.
	if err := e.init(); err != nil {
		return err
	}

	// 2. Acquire pipeline lock (OPS-10).
	if err := e.lock.Acquire(e.cfg.RunID, e.pipeline.Phases[0].ID); err != nil {
		Cleanup(e.tmux, nil, e.log, e.store)
		return fmt.Errorf("engine: %w", err)
	}

	// 3. Expand pipeline → task graph.
	if err := Expand(e.pipeline, e.store); err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return fmt.Errorf("engine: expand: %w", err)
	}

	ev := Event{
		Kind:    EventPhaseStart,
		Phase:   e.pipeline.Phases[0].ID,
		Summary: fmt.Sprintf("pipeline %s started (run %s)", e.pipeline.ID, e.cfg.RunID),
	}
	emitAndNotify(e.log, e.progress, ev)

	// 4. Build dispatch + watchdog.
	e.gaps = NewGapTracker(e.pipeline.GapTolerance)
	e.retries = NewRetryState()
	e.buildDispatchAndWatchdog(0)

	// 5. Run.
	return e.runLoop(ctx)
}

// Resume re-enters execution from persisted state (OPS-08).
func (e *Engine) Resume(ctx context.Context) error {
	// 1. Initialise infrastructure (reopen existing store).
	if err := e.initForResume(); err != nil {
		return err
	}

	// 2. Acquire pipeline lock.
	if err := e.lock.Acquire(e.cfg.RunID, "resume"); err != nil {
		Cleanup(e.tmux, nil, e.log, e.store)
		return fmt.Errorf("engine: %w", err)
	}

	// 3. Resume: reconcile state.
	result, err := Resume(e.store, e.tmux, e.pipeline, e.cfg.RunDir, e.log.Path())
	if err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return fmt.Errorf("engine: resume: %w", err)
	}

	// 4. Initialise gap tracker with recovered state.
	e.gaps = NewGapTracker(e.pipeline.GapTolerance)
	e.gaps.SetGaps(result.GapsRecovered, result.GapAgents)
	e.retries = NewRetryState()

	ev := Event{
		Kind:    EventPhaseStart,
		Phase:   e.pipeline.Phases[result.ResumePhase].ID,
		Summary: fmt.Sprintf("pipeline resumed at phase %d (reconciled=%d, failures-reset=%d, retries-skipped=%d, gaps=%d)", result.ResumePhase, result.Reconciled, result.FailuresReset, result.RetryTasksSkipped, result.GapsRecovered),
	}
	emitAndNotify(e.log, e.progress, ev)

	// 5. Build dispatch + watchdog from resume point.
	e.buildDispatchAndWatchdog(result.ResumePhase)

	// 6. Run.
	return e.runLoop(ctx)
}

// init creates infrastructure for a fresh run.
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

// initForResume reopens existing infrastructure.
func (e *Engine) initForResume() error {
	ledgerDir := filepath.Join(e.cfg.RunDir, "ledger")
	if _, err := os.Stat(ledgerDir); err != nil {
		return fmt.Errorf("engine: %w: %v", ErrResumeNoLedger, err)
	}
	return e.initCommon(ledgerDir)
}

// initCommon opens store, event log, tmux, lock, and pool. Shared by init and initForResume.
func (e *Engine) initCommon(ledgerDir string) error {
	store, err := NewStore(e.cfg.LedgerKind, ledgerDir)
	if err != nil {
		return fmt.Errorf("engine: open store: %w", err)
	}
	e.store = store

	log, err := NewEventLog(filepath.Join(e.cfg.RunDir, "events.jsonl"))
	if err != nil {
		return fmt.Errorf("engine: open event log: %w", err)
	}
	e.log = log

	e.tmux = NewTmuxClient(e.cfg.RunID)
	if err := e.tmux.StartServer(); err != nil {
		return fmt.Errorf("engine: start tmux server: %w", err)
	}

	lockCfg := e.pipeline.Lock
	if lockCfg.Path == "" {
		lockCfg.Path = filepath.Join(e.cfg.RunDir, ".lock")
	} else if !filepath.IsAbs(lockCfg.Path) {
		lockCfg.Path = filepath.Join(e.cfg.RepoPath, lockCfg.Path)
	}
	if err := os.MkdirAll(filepath.Dir(lockCfg.Path), 0o755); err != nil {
		return fmt.Errorf("engine: create lock dir: %w", err)
	}
	e.lock = NewPipelineLock(lockCfg)

	e.pool = NewWorkerPool(e.store, e.tmux, e.cfg.MaxWorkers, e.cfg.RunID, e.cfg.RepoPath, e.cfg.RunDir)
	e.pool.promptTransform = e.cfg.PromptTransform
	return nil
}

// buildDispatchAndWatchdog creates the dispatcher and watchdog from the given start phase.
func (e *Engine) buildDispatchAndWatchdog(startPhase int) {
	e.watchdog = NewWatchdog(e.pool, e.store, e.log, e.pipeline, e.cfg.Preset, e.cfg.PluginDir, e.cfg.Watchdog, e.progress)

	e.dispatch = NewDispatcher(
		e.store, e.pool, e.lock, e.log, e.watchdog,
		e.gaps, e.retries, e.cfg.Retry,
		e.pipeline, e.cfg.Preset, e.cfg.PluginDir, e.cfg.RunDir,
		e.cfg.Dispatch, startPhase, e.pipeline.ID,
		e.progress,
	)
}

// runLoop starts watchdog + dispatch, blocks until terminal, then cleans up.
func (e *Engine) runLoop(ctx context.Context) error {
	// Set up signal handler.
	sigCtx, sigCancel := SetupSignalHandler()
	defer sigCancel()

	// Merge parent context with signal context.
	mergedCtx, mergedCancel := context.WithCancel(ctx)
	defer mergedCancel()
	go func() {
		select {
		case <-sigCtx.Done():
			mergedCancel()
		case <-mergedCtx.Done():
		}
	}()

	// Start watchdog goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.watchdog.Run(mergedCtx)
	}()

	// Run dispatch loop (blocks until terminal or cancelled).
	err := e.dispatch.Run(mergedCtx)

	// Stop watchdog and wait for agent goroutines.
	mergedCancel()
	e.dispatch.Wait()
	wg.Wait()

	// Finalise + cleanup.
	e.finalise(err == nil)
	Cleanup(e.tmux, e.lock, e.log, e.store)

	return err
}

// finalise performs OPS-17 finalisation steps.
func (e *Engine) finalise(success bool) {
	if success {
		e.cleanTransients()
	}
}

// cleanTransients removes transient artefacts (OPS-17a): crash markers, exitcode sidecars, tmp files.
func (e *Engine) cleanTransients() {
	logsDir := filepath.Join(e.cfg.RunDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) == ".exitcode" || filepath.Ext(name) == ".tmp" {
			_ = os.Remove(filepath.Join(logsDir, name))
		}
	}
}
