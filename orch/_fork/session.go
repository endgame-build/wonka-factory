package orch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WorkerPool manages a bounded pool of workers that execute agent tasks
// via tmux sessions.
type WorkerPool struct {
	store           Store
	tmux            *TmuxClient
	maxWorkers      int
	runID           string
	repoPath        string
	outputDir       string
	promptTransform func(agentID, body string) string // optional: rewrite agent prompt before injection
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

// Allocate returns an idle worker, or creates a new one if capacity allows.
// Worker names are w-01, w-02, etc. Returns ErrPoolExhausted if all workers are active.
func (wp *WorkerPool) Allocate() (*Worker, error) {
	// Try to find an existing idle worker.
	workers, err := wp.store.ListWorkers()
	if err != nil {
		return nil, fmt.Errorf("allocate: list workers: %w", err)
	}
	for _, w := range workers {
		if w.Status == WorkerIdle {
			return w, nil
		}
	}

	// Create a new worker if under capacity.
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

// SpawnSession starts a tmux session for a worker executing an agent task.
//
// Steps (per implementation plan):
//  1. Construct session name ({runID}-{workerName})
//  2. Ensure log directory exists
//  3. Build env map (ITF-01)
//  4. Build prompt and command
//  5. Wrap with log redirection
//  6. Create tmux session
//  7. Update worker → active (WKR-05)
//  8. Update task → in_progress (LDG-14a)
func (wp *WorkerPool) SpawnSession(workerName string, task *Task, agentDef AgentDef, preset *Preset, pluginDir string) error {
	sessionName := SessionName(wp.runID, workerName)

	if err := os.MkdirAll(filepath.Join(wp.outputDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("spawn: create log dir: %w", err)
	}

	env := BuildEnv(workerName, wp.runID, wp.outputDir, wp.repoPath, task.AgentID, "", preset.Env) // ITF-01, ITF-02
	// Set ORCH_OUTPUT so agents (especially mock scripts) know where to write.
	if task.Output != "" {
		env["ORCH_OUTPUT"] = filepath.Join(wp.outputDir, task.Output)
	}
	prompt := BuildPrompt(task.Output, agentDef.Inputs, wp.outputDir)
	cmd := BuildCommand(preset, agentDef, pluginDir, prompt)

	// Inject agent system prompt from plugin definition.
	extra, err := wp.agentPromptArgs(preset, pluginDir, task.AgentID)
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	cmd = append(cmd, extra...)

	shellCmd, err := BuildShellCommand(cmd, env, LogPath(wp.outputDir, task.ID), preset.TextFilter)
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// Start the tmux session in the target repo directory so the agent
	// analyses the correct codebase and picks up its CLAUDE.md.
	if err := wp.tmux.CreateSession(sessionName, shellCmd, wp.repoPath); err != nil {
		return fmt.Errorf("spawn: tmux: %w", err)
	}

	// If any subsequent step fails, kill the tmux session to prevent orphans.
	cleanupSession := true
	defer func() {
		if cleanupSession {
			_ = wp.tmux.KillSession(sessionName) // best-effort cleanup
		}
	}()

	// WKR-05: worker → active.
	worker, err := wp.store.GetWorker(workerName)
	if err != nil {
		return fmt.Errorf("spawn: get worker: %w", err)
	}
	worker.Status = WorkerActive
	worker.CurrentTaskID = task.ID
	worker.SessionStartedAt = time.Now()
	if err := wp.store.UpdateWorker(worker); err != nil {
		return fmt.Errorf("spawn: update worker: %w", err)
	}

	// LDG-14a: task → in_progress.
	task.Status = StatusInProgress
	task.UpdatedAt = time.Now()
	if err := wp.store.UpdateTask(task); err != nil {
		return fmt.Errorf("spawn: update task: %w", err)
	}

	cleanupSession = false // all steps succeeded — keep the session
	return nil
}

// agentPromptArgs reads the agent prompt from the plugin directory, applies
// any prompt transform, and returns the extra CLI flags to inject. The agent
// .md file contains full instructions (role, execution protocol, output format).
// BuildCommand only provides the output path — the system prompt carries the
// "what to do" instructions.
func (wp *WorkerPool) agentPromptArgs(preset *Preset, pluginDir, agentID string) ([]string, error) {
	if preset.SystemPromptFlag == "" || pluginDir == "" {
		return nil, nil
	}
	agentPrompt, model, err := ReadAgentPrompt(pluginDir, agentID)
	if err != nil {
		return nil, err
	}
	var args []string
	if agentPrompt != "" {
		if wp.promptTransform != nil {
			agentPrompt = wp.promptTransform(agentID, agentPrompt)
		}
		args = append(args, preset.SystemPromptFlag, agentPrompt)
	}
	if model != "" && preset.ModelFlag != "" {
		args = append(args, preset.ModelFlag, model)
	}
	return args, nil
}

// IsAlive reports whether the worker's tmux session is running (SUP-02 prerequisite).
// Returns a non-nil error if tmux infrastructure is broken (not the same as "session dead").
func (wp *WorkerPool) IsAlive(workerName string) (bool, error) {
	return wp.tmux.HasSession(SessionName(wp.runID, workerName))
}

// Release transitions a worker from active to idle (WKR-04). Kills the
// worker's tmux session to prevent orphans, clears session state and
// CurrentTaskID. The task's Assignee field is NOT modified — it remains
// pointing to this worker for ledger recovery.
func (wp *WorkerPool) Release(workerName string) error {
	worker, err := wp.store.GetWorker(workerName)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}

	if err := wp.tmux.KillSessionIfExists(SessionName(wp.runID, workerName)); err != nil {
		return fmt.Errorf("release: kill session: %w", err)
	}

	worker.Status = WorkerIdle
	worker.CurrentTaskID = ""
	worker.SessionPID = 0
	worker.SessionStartedAt = time.Time{}

	return wp.store.UpdateWorker(worker)
}

// Deallocate releases a worker from the pool. Returns ErrWorkerBusy if the
// worker has an active assignment (WKR-11). Kills any tmux session (WKR-12).
// Note: the Store interface does not yet have DeleteWorker; full removal is
// deferred to Phase 3 (event log recording + worker store deletion).
func (wp *WorkerPool) Deallocate(workerName string) error {
	worker, err := wp.store.GetWorker(workerName)
	if err != nil {
		return fmt.Errorf("deallocate: %w", err)
	}
	if worker.CurrentTaskID != "" {
		return ErrWorkerBusy
	}

	// Kill session — suppress "not found" but propagate real errors (avoids TOCTOU).
	if err := wp.tmux.KillSessionIfExists(SessionName(wp.runID, workerName)); err != nil {
		return fmt.Errorf("deallocate: %w", err)
	}
	return nil
}

// ResetWorkspace clears prior output artefacts when reassigning a worker to
// a new task (WKR-08). Does NOT reset on same-task restart (CTY-06).
func (wp *WorkerPool) ResetWorkspace(previousOutput string) error {
	if previousOutput == "" {
		return nil
	}
	path := filepath.Join(wp.outputDir, previousOutput)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reset workspace: %w", err)
	}
	return nil
}

// RestartSession kills an existing session and spawns a new one for the same
// task. The task assignment is unchanged (CTY-06). Supports unlimited restarts
// per task (CTY-07). Does NOT reset workspace (same-task restart).
func (wp *WorkerPool) RestartSession(workerName string, task *Task, agentDef AgentDef, preset *Preset, pluginDir string) error {
	// Kill existing session — suppress "not found" but propagate real errors (avoids TOCTOU).
	if err := wp.tmux.KillSessionIfExists(SessionName(wp.runID, workerName)); err != nil {
		return fmt.Errorf("restart: kill: %w", err)
	}

	// Spawn new session for same task (CTY-06: assignment unchanged).
	return wp.SpawnSession(workerName, task, agentDef, preset, pluginDir)
}
