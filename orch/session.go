package orch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WorkerPool manages a bounded pool of workers that execute agent tasks
// via tmux sessions. BVV-DSP-05: one task per session — each SpawnSession
// call creates a fresh session for a specific task, and the session ends
// when the agent exits.
type WorkerPool struct {
	store      Store
	tmux       *TmuxClient
	maxWorkers int
	runID      string
	repoPath   string
	outputDir  string // run directory; holds logs/{taskID}.stdout and sidecar files

	// logDirOnce guarantees the per-run logs directory is created at most
	// once per pool — without this, every SpawnSession would issue a stat
	// syscall against an existing directory.
	logDirOnce sync.Once
	logDirErr  error
}

// NewWorkerPool creates a worker pool backed by the given store and tmux client.
func NewWorkerPool(store Store, tmux *TmuxClient, maxWorkers int, runID, repoPath, outputDir string) *WorkerPool {
	return &WorkerPool{
		store:      store,
		tmux:       tmux,
		maxWorkers: maxWorkers,
		runID:      runID,
		repoPath:   repoPath,
		outputDir:  outputDir,
	}
}

// OutputDir returns the run directory that holds logs and sidecar files.
// Used by the dispatcher's runAgent to compute LogPath for exit-code reading.
func (wp *WorkerPool) OutputDir() string { return wp.outputDir }

// RunID returns the run identifier for session naming.
func (wp *WorkerPool) RunID() string { return wp.runID }

// MaxWorkers returns the pool capacity. Used by the dispatcher to size the
// outcomes channel buffer.
func (wp *WorkerPool) MaxWorkers() int { return wp.maxWorkers }

// Allocate returns an idle worker, or creates a new one if capacity allows.
// Worker names are w-01, w-02, etc. Returns ErrPoolExhausted if all workers
// are active (BVV WKR-04..07).
func (wp *WorkerPool) Allocate() (*Worker, error) {
	workers, err := wp.store.ListWorkers()
	if err != nil {
		return nil, fmt.Errorf("allocate: list workers: %w", err)
	}
	for _, w := range workers {
		if w.Status == WorkerIdle {
			return w, nil
		}
	}

	if len(workers) >= wp.maxWorkers {
		return nil, ErrPoolExhausted
	}

	w := &Worker{
		Name:   fmt.Sprintf("w-%02d", len(workers)+1),
		Status: WorkerIdle,
	}
	if err := wp.store.CreateWorker(w); err != nil {
		return nil, fmt.Errorf("allocate: create worker: %w", err)
	}
	return w, nil
}

// SpawnSession starts a tmux session for a worker executing an agent task
// (BVV WKR-05, BVV-DSP-05).
//
// Steps:
//  1. Resolve instruction + model from roleCfg.InstructionFile.
//  2. Build env with ORCH_TASK_ID / ORCH_BRANCH (BVV-ITF-01, BVV-DSP-06).
//  3. Build command from preset + instruction + model + maxTurns.
//  4. Wrap with sidecar exit-code capture (BVV Appendix A).
//  5. Create tmux session in repoPath (so CLAUDE.md auto-discovers).
//  6. Transition worker → active (WKR-05).
//  7. Transition task → in_progress (LDG-14a).
//
// On any failure after step 5, a deferred rollback kills the tmux session
// and (if step 6 landed) restores the worker's prior store record. The
// task ID comes from the passed *Task (task.ID) — no separate taskID
// parameter.
func (wp *WorkerPool) SpawnSession(workerName string, task *Task, roleCfg RoleConfig, branch string) error {
	if roleCfg.Preset == nil {
		return fmt.Errorf("spawn: role %q has nil preset", task.Role())
	}
	if err := wp.ensureLogDir(); err != nil {
		return fmt.Errorf("spawn: create log dir: %w", err)
	}
	sessionName := SessionName(wp.runID, workerName)

	// 1. Resolve instruction body + model from the role's .md file. The body
	// is the frontmatter-stripped content; the model comes from frontmatter.
	body, model, err := ReadAgentPrompt(roleCfg.InstructionFile)
	if err != nil {
		return fmt.Errorf("spawn: read instruction: %w", err)
	}

	// 2. Build env — BVV-ITF-01 env-only identity, BVV-DSP-06 ORCH_TASK_ID.
	env := BuildEnv(workerName, wp.runID, wp.repoPath, task.ID, branch, roleCfg.Preset.Env)

	// 3. Build command — preset + instruction body + model override + maxTurns.
	// The body is passed as the literal --append-system-prompt argument so the
	// CLI uses it as the prompt string (not as a file path).
	cmd := BuildCommand(roleCfg.Preset, body, model, roleCfg.MaxTurns)

	// 4. Wrap with sidecar exit-code capture (BVV Appendix A).
	shellCmd, err := BuildShellCommand(cmd, env, LogPath(wp.outputDir, task.ID), roleCfg.Preset.TextFilter)
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// 5. Start the tmux session in the target repo directory so the agent
	// analyses the correct codebase and picks up its CLAUDE.md.
	if err := wp.tmux.CreateSession(sessionName, shellCmd, wp.repoPath); err != nil {
		return fmt.Errorf("spawn: tmux: %w", err)
	}

	var priorWorker *Worker // set after step 6 lands; drives the rollback
	success := false
	defer func() {
		if success {
			return
		}
		// Rollback is best-effort; we're already returning the step error.
		// Surface rollback failures to stderr so operators can see what
		// leaked — a silent UpdateWorker failure strands the worker record
		// as Active until Resume reconciles it. KillSessionIfExists
		// swallows "session already gone" so a fast-exiting mock agent
		// doesn't spam warnings; a genuine tmux infra failure still
		// surfaces here.
		if err := wp.tmux.KillSessionIfExists(sessionName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: spawn rollback: kill session %s failed: %v\n", sessionName, err)
		}
		if priorWorker != nil {
			if err := wp.store.UpdateWorker(priorWorker); err != nil {
				fmt.Fprintf(os.Stderr, "warning: spawn rollback: restore worker %s failed: %v\n", workerName, err)
			}
		}
	}()

	// 6. WKR-05: worker → active.
	worker, err := wp.store.GetWorker(workerName)
	if err != nil {
		return fmt.Errorf("spawn: get worker: %w", err)
	}
	snap := *worker
	worker.Status = WorkerActive
	worker.CurrentTaskID = task.ID
	worker.SessionStartedAt = time.Now()
	if err := wp.store.UpdateWorker(worker); err != nil {
		return fmt.Errorf("spawn: update worker: %w", err)
	}
	priorWorker = &snap

	// 7. LDG-14a: task → in_progress. Stage the transition on a copy so
	// the caller's *task stays consistent with the store if UpdateTask
	// fails and the deferred rollback fires.
	updatedTask := *task
	updatedTask.Status = StatusInProgress
	updatedTask.UpdatedAt = time.Now()
	if err := wp.store.UpdateTask(&updatedTask); err != nil {
		return fmt.Errorf("spawn: update task: %w", err)
	}
	*task = updatedTask

	success = true
	return nil
}

// IsAlive reports whether the worker's tmux session is running (BVV-ERR-11
// prerequisite). Returns a non-nil error if tmux infrastructure is broken
// (distinct from "session dead" — callers of Watchdog.CheckOnce treat
// (false, nil) as "restart candidate" and error as "skip this tick").
func (wp *WorkerPool) IsAlive(workerName string) (bool, error) {
	return wp.tmux.HasSession(SessionName(wp.runID, workerName))
}

// Release transitions a worker from active to idle (WKR-04). Kills the
// worker's tmux session to prevent orphans, clears session state and
// CurrentTaskID. The task's Assignee field is NOT modified here — the
// dispatcher owns all task status transitions (BVV-DSP-15). BVV-S-02
// governs terminal-state irreversibility, which is a separate concern.
func (wp *WorkerPool) Release(workerName string) error {
	worker, err := wp.store.GetWorker(workerName)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}

	if wp.tmux != nil {
		if err := wp.tmux.KillSessionIfExists(SessionName(wp.runID, workerName)); err != nil {
			return fmt.Errorf("release: kill session: %w", err)
		}
	}

	worker.Status = WorkerIdle
	worker.CurrentTaskID = ""
	worker.SessionPID = 0
	worker.SessionStartedAt = time.Time{}

	return wp.store.UpdateWorker(worker)
}

// Deallocate removes a worker from the pool. Returns ErrWorkerBusy if the
// worker has an active assignment (WKR-11). Kills any tmux session (WKR-12).
// Note: the Store interface does not yet have DeleteWorker; full removal is
// deferred — Deallocate today is only responsible for tmux cleanup.
func (wp *WorkerPool) Deallocate(workerName string) error {
	worker, err := wp.store.GetWorker(workerName)
	if err != nil {
		return fmt.Errorf("deallocate: %w", err)
	}
	if worker.CurrentTaskID != "" {
		return ErrWorkerBusy
	}

	if wp.tmux != nil {
		if err := wp.tmux.KillSessionIfExists(SessionName(wp.runID, workerName)); err != nil {
			return fmt.Errorf("deallocate: %w", err)
		}
	}
	return nil
}

// ensureLogDir creates the per-run logs directory exactly once per pool
// lifetime, regardless of how many times SpawnSession is called. The first
// call does the MkdirAll; subsequent calls return the cached result.
func (wp *WorkerPool) ensureLogDir() error {
	wp.logDirOnce.Do(func() {
		wp.logDirErr = os.MkdirAll(filepath.Join(wp.outputDir, "logs"), 0o755)
	})
	return wp.logDirErr
}

// RestartSession kills an existing session and spawns a new one for the same
// task. Used by Watchdog on dead-session detection (BVV-ERR-11) and by the
// dispatcher on exit-code-3 handoff processing. Task assignment is unchanged.
// The HandoffState counter is NOT modified here — the caller (watchdog or
// dispatcher) owns that accounting per the BVV-L-04 budget contract.
func (wp *WorkerPool) RestartSession(workerName string, task *Task, roleCfg RoleConfig, branch string) error {
	if wp.tmux != nil {
		if err := wp.tmux.KillSessionIfExists(SessionName(wp.runID, workerName)); err != nil {
			return fmt.Errorf("restart: kill: %w", err)
		}
	}
	return wp.SpawnSession(workerName, task, roleCfg, branch)
}
