package orch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Progress   ProgressReporter // nil = no-op
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

	// Diagnostics surfaced via lifecycle_started detail. Populated during
	// init*; consumed in Run/Resume after the event log is open so corruption
	// is visible in the audit trail rather than only on stderr.
	storeFallbackFrom LedgerKind // non-empty if store fell back to a different backend
	storeFallbackTo   LedgerKind

	// started guards against double invocation of Run/Resume. A second
	// call would re-init, double-acquire the lock, and leak resources.
	started sync.Once

	testSpawnFunc SpawnFunc // test-only override; see SetTestSpawnFunc in testhooks_test.go
}

// ErrEngineAlreadyStarted is returned by Run or Resume if the Engine has
// already been started. Engines are single-use — construct a new one for
// each lifecycle.
var ErrEngineAlreadyStarted = errors.New("engine: already started")

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

// Run executes a fresh lifecycle (BVV §8, implements BVV-S-01, BVV-ERR-09,
// BVV-ERR-10a, BVV-SS-01).
// The ledger must be pre-populated (by planner or human); there is no Expand().
// Single-use: returns ErrEngineAlreadyStarted on subsequent calls.
func (e *Engine) Run(ctx context.Context) error {
	if !e.markStarted() {
		return ErrEngineAlreadyStarted
	}
	// 1. Initialise infrastructure.
	if err := e.init(); err != nil {
		return err
	}

	// 2. Acquire lifecycle lock (BVV-S-01).
	// Typically a fresh run creates its own tmux socket (random RunID),
	// but a caller-supplied RunID could collide with a live socket — in
	// which case StartServer joined rather than created. cleanupAfterFailedAcquire
	// consults OwnsServer so the collision case doesn't take down a live holder.
	if err := e.lock.Acquire(e.cfg.RunID, e.cfg.Lifecycle.Branch); err != nil {
		e.cleanupAfterFailedAcquire()
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

	// 5. Emit lifecycle_started — fatal on failure.
	summary := fmt.Sprintf("lifecycle started (run %s, branch %s)", e.cfg.RunID, e.cfg.Lifecycle.Branch)
	if err := e.emitLifecycleStarted(summary, nil); err != nil {
		return err
	}

	// 6. Run dispatch loop.
	return e.runLoop(ctx)
}

// Resume re-enters execution from persisted state (BVV-ERR-06..08).
// Reconciles lifecycle state (stale assignments, orphan sessions, recovered
// gap/retry/handoff counters, human re-opens) then resumes the dispatch
// loop. Reconciliation produces the recovered counters; counters do not
// flow in independently.
func (e *Engine) Resume(ctx context.Context) error {
	if !e.markStarted() {
		return ErrEngineAlreadyStarted
	}
	// 1. Initialise infrastructure (verifies ledger, recovers previous RunID).
	if err := e.initForResume(); err != nil {
		return err
	}

	// 2. Acquire lifecycle lock (BVV-ERR-06: staleness recovery).
	// BVV-ERR-08 hazard: Resume may have recovered a RunID pointing at a
	// still-live orchestrator's tmux socket. If Acquire fails here, the
	// previous holder is alive — cleanupAfterFailedAcquire consults
	// tmux.OwnsServer() and refuses to KillServer on a joined socket.
	if err := e.lock.Acquire(e.cfg.RunID, e.cfg.Lifecycle.Branch); err != nil {
		e.cleanupAfterFailedAcquire()
		return fmt.Errorf("engine: %w", err)
	}

	// 3. Reconcile state (BVV-ERR-07: must complete before dispatch).
	result, err := Reconcile(e.store, e.tmux, e.cfg.RunID, e.cfg.Lifecycle.Branch, e.log.Path())
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
	// Emit the audit-trail record BEFORE mutating in-memory counters. If
	// emit fails and we returned after the reset, the next Resume would
	// replay the (unchanged) event log, recompute pre-reopen counters,
	// and have no EscalationResolved marker to justify clearing them —
	// silently restoring the old limits. Emit-first preserves the
	// invariant: counters only change if the audit trail records why.
	//
	// Reopen detection is idempotent (step 6 keys off terminalHistory,
	// which EscalationResolved does not affect), so fail-fast here is
	// safe: the next Resume will detect the same reopens and retry.
	for _, taskID := range result.HumanReopens {
		if err := emitAndNotify(e.log, e.cfg.Progress, Event{
			Kind:    EventEscalationResolved,
			TaskID:  taskID,
			Summary: fmt.Sprintf("human re-open detected for task %s — counters reset", taskID),
		}); err != nil {
			Cleanup(e.tmux, e.lock, e.log, e.store)
			return fmt.Errorf("engine: emit escalation_resolved for %s: %w", taskID, err)
		}
		e.retries.ResetTask(taskID)
		e.handoffs.Reset(taskID)
	}

	// 5. Create watchdog + dispatcher.
	if err := e.buildDispatchAndWatchdog(); err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return err
	}

	// 6. Emit lifecycle_started with resume detail — fatal on failure.
	summary := fmt.Sprintf("lifecycle resumed (run %s, branch %s, reconciled=%d, orphans=%d, gaps=%d, reopens=%d)",
		e.cfg.RunID, e.cfg.Lifecycle.Branch,
		result.Reconciled, result.OrphanedSessions,
		len(result.GapsRecovered), len(result.HumanReopens))
	if err := e.emitLifecycleStarted(summary, result); err != nil {
		return err
	}

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
//
// Fails fast on:
//   - ledger missing: wraps ErrResumeNoLedger so callers can distinguish
//     "fresh start needed" from any other Stat error (perm-denied, EIO).
//   - lock corrupt: returns ErrCorruptLock. Silently fabricating a fresh
//     RunID would orphan any live tmux socket (BVV-ERR-08 violation) and
//     race the live orchestrator against the ledger (BVV-S-03 hazard).
//     Corrupt-lock recovery is operator-intervention territory, not
//     log-and-continue.
func (e *Engine) initForResume() error {
	ledgerDir := filepath.Join(e.cfg.RunDir, "ledger")
	if _, err := os.Stat(ledgerDir); err != nil {
		// Only IsNotExist qualifies as "no ledger found — fresh start OK".
		// Permission-denied, EIO, and friends must not be squashed into
		// ErrResumeNoLedger; callers that branch on errors.Is(…, ErrResumeNoLedger)
		// would otherwise treat an unreadable ledger dir as "create a new one"
		// and clobber the real ledger.
		if os.IsNotExist(err) {
			return fmt.Errorf("engine: %w: %v", ErrResumeNoLedger, err)
		}
		return fmt.Errorf("engine: stat ledger dir: %w", err)
	}

	// Recover previous RunID from stale lock file so we can reconnect to
	// the surviving tmux socket and detect live sessions (BVV-ERR-08).
	// Distinguish missing (genuine fresh resume) from corrupt (prior crash
	// mid-write).
	lockPath := e.lockPath()
	prev, err := ReadHolder(lockPath)
	if err != nil {
		return fmt.Errorf("engine: %w: %s: %v", ErrCorruptLock, lockPath, err)
	}
	if prev != "" {
		e.cfg.RunID = prev
	}

	return e.initCommon(ledgerDir)
}

// initCommon opens store, event log, tmux, lock, and pool. On any failure
// after a resource has been opened, this cascades closes so the caller does
// not need to (Run/Resume only call Cleanup after the lock has been acquired).
func (e *Engine) initCommon(ledgerDir string) error {
	// 1. Open store.
	store, kind, err := NewStore(e.cfg.LedgerKind, ledgerDir)
	if err != nil {
		return fmt.Errorf("engine: open store: %w", err)
	}
	e.store = store
	// Stash fallback for emission via lifecycle_started detail — stderr
	// alone is invisible in the audit trail, so an operator reading
	// events.jsonl post-incident cannot tell the run was degraded.
	//
	// NewStore treats empty LedgerKind as LedgerBeads and silently falls
	// back to LedgerFS if Beads is unreachable. Comparing against the
	// effective requested kind (not the raw config field) ensures the
	// common default-config path also surfaces the fallback — the prior
	// `!= ""` guard skipped exactly this case.
	requested := e.cfg.LedgerKind
	if requested == "" {
		requested = LedgerBeads
	}
	if kind != requested {
		e.storeFallbackFrom = requested
		e.storeFallbackTo = kind
		fmt.Fprintf(os.Stderr, "warning: store fallback: requested %s, using %s\n", requested, kind)
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
		// Cascade-close the resources opened above. Without this, the tmux
		// server, event log fd, and store all leak because Run/Resume only
		// invoke Cleanup once the lock has been acquired.
		Cleanup(e.tmux, nil, e.log, e.store)
		e.tmux, e.log, e.store = nil, nil, nil
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
	if e.testSpawnFunc != nil {
		disp.SetSpawnFunc(e.testSpawnFunc)
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

	// 6. Drain any outcomes that in-flight agents pushed after cancel but
	// before their goroutines returned (BVV-ERR-10a precondition). Without
	// this, the ctx.Done branch in runAgent writes an OutcomeFailure that
	// never reaches processOutcome, so Worker.Status stays Active in the
	// store and CheckReleaseDrained/AssertLifecycleReleaseDrained reports a
	// phantom violation. Drain must run AFTER Wait() — concurrent receives
	// on d.outcomes would race processOutcome.
	//
	// Uses a background context rather than the cancelled mergedCtx because
	// processOutcome's internal store writes should not be skipped just
	// because the user-facing ctx was cancelled; the goroutines have
	// already landed, and refusing to record their outcomes would re-create
	// the same phantom-busy condition.
	e.disp.Drain(context.Background())

	// 7. Classify the exit. Signal paths stay silent per BVV-ERR-09 —
	// a mid-run Ctrl-C must not modify status or leave a completion event
	// that would look like clean termination to the next Resume.
	// Operational errors need their own anchor so the event log isn't
	// truncated in a way indistinguishable from a crash.
	switch {
	case err == nil, errors.Is(err, ErrLifecycleAborted):
		e.emitLifecycleCompleted(err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Signal path — intentionally no event.
	default:
		e.emitLifecycleFailed(err)
	}

	// 8. Cleanup (releases lock, kills tmux, closes log+store).
	Cleanup(e.tmux, e.lock, e.log, e.store)

	return err
}

// emitLifecycleCompleted emits the clean-exit or gap-abort lifecycle anchor
// and runs the BVV-ERR-10a drain check. The Check runs in any build; the
// Assert panics only under -tags verify. Without the Check, a release build
// would hide BVV-ERR-10a violations entirely.
func (e *Engine) emitLifecycleCompleted(err error) {
	var summary, detail string
	if errors.Is(err, ErrLifecycleAborted) {
		summary = fmt.Sprintf("lifecycle aborted: gap tolerance reached (run %s, branch %s)",
			e.cfg.RunID, e.cfg.Lifecycle.Branch)
		detail = "outcome=aborted reason=gap_tolerance_exceeded"
	} else {
		summary = fmt.Sprintf("lifecycle completed (run %s, branch %s)",
			e.cfg.RunID, e.cfg.Lifecycle.Branch)
	}
	if busy := CheckReleaseDrained(e.store); len(busy) > 0 {
		summary = fmt.Sprintf("[BVV-ERR-10a] lifecycle release with active workers: %v", busy)
		detail = fmt.Sprintf("run=%s branch=%s busy=%v", e.cfg.RunID, e.cfg.Lifecycle.Branch, busy)
	}
	AssertLifecycleReleaseDrained(e.store)
	if emitErr := emitAndNotify(e.log, e.cfg.Progress, Event{
		Kind:    EventLifecycleCompleted,
		Summary: summary,
		Detail:  detail,
	}); emitErr != nil {
		// Lifecycle-completed is the terminal anchor a future Resume keys off.
		// If its write fails (disk full, fd gone), surface it on stderr so
		// the operator at least sees the gap. We cannot return an error here —
		// the caller is already committed to returning `err`, and the lock
		// still needs releasing.
		fmt.Fprintf(os.Stderr, "warning: emit lifecycle_completed failed: %v\n", emitErr)
	}
}

// emitLifecycleFailed emits a completion anchor for the operational-error
// exit (dispatcher Tick.Error, store corruption mid-run). Preserves the
// BVV-SS-01 / §10.3 guarantee that every lifecycle has a terminal anchor
// event, so a future Resume can tell the prior run ended rather than
// crashed. Also runs CheckReleaseDrained + AssertLifecycleReleaseDrained
// (BVV-ERR-10a) — operational errors do not legitimise leaked sessions.
func (e *Engine) emitLifecycleFailed(runErr error) {
	detail := fmt.Sprintf("outcome=failed reason=%s", runErr)
	if busy := CheckReleaseDrained(e.store); len(busy) > 0 {
		detail += fmt.Sprintf(" busy=%v", busy)
	}
	AssertLifecycleReleaseDrained(e.store)
	summary := fmt.Sprintf("lifecycle failed (run %s, branch %s): %v",
		e.cfg.RunID, e.cfg.Lifecycle.Branch, runErr)
	if emitErr := emitAndNotify(e.log, e.cfg.Progress, Event{
		Kind:    EventLifecycleCompleted,
		Summary: summary,
		Detail:  detail,
	}); emitErr != nil {
		fmt.Fprintf(os.Stderr, "warning: emit lifecycle_completed (failed) failed: %v\n", emitErr)
	}
}

// markStarted returns true on the first call (Run or Resume) and false on
// every subsequent call. Engines are single-use — re-running through the
// same Engine instance would re-init and double-acquire.
func (e *Engine) markStarted() bool {
	first := false
	e.started.Do(func() { first = true })
	return first
}

// emitLifecycleStarted writes the canonical §10.3 anchor event. Its absence
// from the audit trail leaves a future Resume blind to the lifecycle
// boundary (recoverFromEventLog keys off it), so a failed emit is fatal and
// must cascade-close the resources the caller otherwise expects to own.
func (e *Engine) emitLifecycleStarted(summary string, result *ResumeResult) error {
	err := emitAndNotify(e.log, e.cfg.Progress, Event{
		Kind:    EventLifecycleStarted,
		Summary: summary,
		Detail:  e.diagnosticsDetail(result),
	})
	if err != nil {
		Cleanup(e.tmux, e.lock, e.log, e.store)
		return fmt.Errorf("engine: emit lifecycle_started: %w", err)
	}
	return nil
}

// diagnosticsDetail formats stashed warnings (store fallback, event-log
// corruption, failed kills) for inclusion in the lifecycle_started event.
// Returns "" if there is nothing to report. The audit trail is the
// canonical surface for these conditions — stderr alone is invisible to
// post-incident review.
//
// Lock-file corruption is NOT surfaced here: corrupt locks fail Resume
// outright (ErrCorruptLock) rather than continuing into lifecycle_started,
// so the audit-trail anchor is never written under that condition.
func (e *Engine) diagnosticsDetail(resume *ResumeResult) string {
	var parts []string
	if e.storeFallbackFrom != "" {
		parts = append(parts, fmt.Sprintf("store_fallback=%s->%s", e.storeFallbackFrom, e.storeFallbackTo))
	}
	if resume != nil {
		if resume.EventLogCorruptLines > 0 {
			parts = append(parts, fmt.Sprintf("event_log_corrupt_lines=%d", resume.EventLogCorruptLines))
		}
		if len(resume.FailedKills) > 0 {
			parts = append(parts, fmt.Sprintf("failed_kills=%v", resume.FailedKills))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "warnings: " + strings.Join(parts, ", ")
}

// cleanupAfterFailedAcquire releases resources opened during init* when lock
// acquisition fails. Unlike the generic Cleanup, it refuses to KillServer on
// a tmux socket this engine joined rather than created: under Resume, a
// joined socket belongs to the still-live orchestrator whose contention
// blocked us, and killing it would destroy the live holder's sessions
// (BVV-ERR-08).
func (e *Engine) cleanupAfterFailedAcquire() {
	var tmuxForCleanup *TmuxClient
	if e.tmux != nil {
		if e.tmux.OwnsServer() {
			tmuxForCleanup = e.tmux
		} else {
			fmt.Fprintf(os.Stderr,
				"warning: leaving tmux server on socket %s intact — joined from prior run, KillServer would destroy the live holder's sessions (BVV-ERR-08)\n",
				e.tmux.Socket)
		}
	}
	Cleanup(tmuxForCleanup, nil, e.log, e.store)
}

// lockPath returns the default lock file path for the current branch.
// Branch names may contain path separators (e.g. "feat/x") or parent-dir
// references ("..") that would otherwise nest the lock under RunDir or
// escape it entirely. Sanitize to a flat, filename-safe fragment.
func (e *Engine) lockPath() string {
	if e.cfg.Lifecycle.Lock.Path != "" {
		return e.cfg.Lifecycle.Lock.Path
	}
	return filepath.Join(e.cfg.RunDir,
		fmt.Sprintf(".wonka-%s.lock", sanitizeBranchForLock(e.cfg.Lifecycle.Branch)))
}

// sanitizeBranchForLock flattens a branch name into a filename-safe fragment
// so the derived lock path cannot escape RunDir.
func sanitizeBranchForLock(branch string) string {
	safe := strings.NewReplacer("/", "-", "\\", "-").Replace(strings.TrimSpace(branch))
	if safe == "" || safe == "." || safe == ".." {
		return "default"
	}
	return safe
}

// generateRunID produces a short random hex string for run identification.
func generateRunID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
