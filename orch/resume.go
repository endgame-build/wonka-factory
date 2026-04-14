package orch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// SessionPresence abstracts tmux session operations needed by Reconcile.
// *TmuxClient satisfies this interface. Tests provide a mock.
type SessionPresence interface {
	RunID() string
	HasSession(name string) (bool, error)
	ListSessions() ([]string, error)
	KillSessionIfExists(name string) error
}

// ResumeResult describes the outcome of DAG state reconciliation (BVV §11a.2).
// Returned by Reconcile for the Engine to restore in-memory state machines.
type ResumeResult struct {
	Reconciled        int              // stale assignments reset to open
	OrphanedSessions  int              // orphan tmux sessions killed
	GapsRecovered     []string         // task IDs from gap_recorded events
	RetriesRecovered  map[string]int   // taskID → retry count from event log
	HandoffsRecovered map[string]int   // taskID → handoff count from event log
	HumanReopens      []string         // task IDs where terminal→open detected (BVV-S-02a)
}

// Reconcile reconstructs lifecycle state from the ledger, tmux, and event log
// (BVV-ERR-07: reconciliation MUST complete before dispatch resumes).
//
// The 7-step algorithm follows BVV spec §11a.2:
//  1. Reset stale assignments (dead sessions → task open).
//  2. Kill orphaned tmux sessions.
//  3. Recover gap count from event log (BVV-ERR-05 monotonic).
//  4. Recover retry counts from event log (BVV-ERR-01 monotonic).
//  5. Recover handoff counts from event log (BVV-L-04 monotonic).
//  6. Detect human re-opens (BVV-S-02a).
//  7. Reset workers to idle (preserving live-session workers).
//
// All steps are idempotent — a crash during Reconcile allows the next Resume
// to re-run from scratch without state corruption.
func Reconcile(
	store Store,
	tmux SessionPresence,
	branch string,
	logPath string,
) (*ResumeResult, error) {
	result := &ResumeResult{
		RetriesRecovered:  make(map[string]int),
		HandoffsRecovered: make(map[string]int),
	}
	branchLabel := LabelBranch + ":" + branch

	// Step 1: Stale assignment detection (BVV-ERR-08).
	// Tasks with status assigned/in_progress but no live tmux session are
	// reset to open. BVV-ERR-08: in_progress tasks WITH a live session are
	// preserved — the watchdog picks up monitoring after reconciliation.
	tasks, err := store.ListTasks(branchLabel)
	if err != nil {
		return nil, fmt.Errorf("reconcile: list tasks: %w", err)
	}
	// Track which tasks still have live sessions (used in steps 2 and 7).
	liveTasks := make(map[string]bool)
	for i := range tasks {
		task := tasks[i]
		if task.Status != StatusAssigned && task.Status != StatusInProgress {
			continue
		}
		if task.Assignee == "" {
			continue
		}
		sessionName := SessionName(tmux.RunID(), task.Assignee)
		alive, hasErr := tmux.HasSession(sessionName)
		if hasErr != nil {
			continue // tmux infra error — skip, don't corrupt state
		}
		if alive && task.Status == StatusInProgress {
			liveTasks[task.ID] = true
			continue // BVV-ERR-08: live session continues
		}
		// Dead session or assigned-but-never-started → reset.
		task.Status = StatusOpen
		task.Assignee = ""
		task.UpdatedAt = time.Now()
		if err := store.UpdateTask(task); err != nil {
			return nil, fmt.Errorf("reconcile: reset task %s: %w", task.ID, err)
		}
		tasks[i] = task // update slice in-place for step 2
		result.Reconciled++
	}

	// Step 2: Orphaned session cleanup.
	// Kill tmux sessions that have no corresponding in_progress task.
	// Reuses the tasks slice from step 1 (mutated in-place above).
	sessions, err := tmux.ListSessions()
	if err != nil {
		sessions = nil // tmux server may be dead — not fatal
	}
	if len(sessions) > 0 {
		expected := make(map[string]bool)
		for _, t := range tasks {
			if t.Status == StatusInProgress && t.Assignee != "" {
				expected[SessionName(tmux.RunID(), t.Assignee)] = true
			}
		}
		for _, session := range sessions {
			if !expected[session] {
				_ = tmux.KillSessionIfExists(session)
				result.OrphanedSessions++
			}
		}
	}

	// Steps 3-6: Recover counters and detect human re-opens from event log.
	// Single-pass scan populates all four recovery outputs.
	if logPath != "" {
		rec, recErr := recoverFromEventLog(logPath)
		if recErr != nil {
			return nil, fmt.Errorf("reconcile: recover from event log: %w", recErr)
		}
		result.GapsRecovered = rec.gaps
		result.RetriesRecovered = rec.retries
		result.HandoffsRecovered = rec.handoffs

		// Step 6: Human re-open detection (BVV-S-02a).
		// A task that was terminal in the event log but is now open was
		// re-opened by a human.
		for taskID, lastTerminal := range rec.terminalHistory {
			task, getErr := store.GetTask(taskID)
			if getErr != nil {
				continue // task may have been deleted
			}
			if task.Status == StatusOpen && lastTerminal.Terminal() {
				result.HumanReopens = append(result.HumanReopens, taskID)
			}
		}
	}

	// Step 7: Worker reset.
	// All workers → idle, except those monitoring a live session (step 1).
	workers, err := store.ListWorkers()
	if err != nil {
		return nil, fmt.Errorf("reconcile: list workers: %w", err)
	}
	for _, w := range workers {
		if w.Status == WorkerIdle {
			continue
		}
		// Preserve workers whose assigned task has a live session.
		if w.CurrentTaskID != "" && liveTasks[w.CurrentTaskID] {
			continue
		}
		w.Status = WorkerIdle
		w.CurrentTaskID = ""
		w.SessionPID = 0
		w.SessionStartedAt = time.Time{}
		if err := store.UpdateWorker(w); err != nil {
			return nil, fmt.Errorf("reconcile: reset worker %s: %w", w.Name, err)
		}
	}

	return result, nil
}

// --- Event log recovery ---

// eventLogRecovery holds all counters extracted from a single event log scan.
type eventLogRecovery struct {
	gaps            []string            // task IDs from gap_recorded events
	retries         map[string]int      // taskID → retry count
	handoffs        map[string]int      // taskID → handoff count
	terminalHistory map[string]TaskStatus // taskID → last terminal status
}

// recoverFromEventLog performs a single-pass scan of the event log and
// extracts gap task IDs, per-task retry counts, per-task handoff counts,
// and the last terminal status per task. Missing log file → zero values, nil.
// Corrupt JSON lines are silently skipped.
func recoverFromEventLog(logPath string) (*eventLogRecovery, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &eventLogRecovery{
				retries:         make(map[string]int),
				handoffs:        make(map[string]int),
				terminalHistory: make(map[string]TaskStatus),
			}, nil
		}
		return nil, err
	}
	defer f.Close()

	rec := &eventLogRecovery{
		retries:         make(map[string]int),
		handoffs:        make(map[string]int),
		terminalHistory: make(map[string]TaskStatus),
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.TaskID == "" {
			continue
		}
		switch e.Kind {
		case EventGapRecorded:
			rec.gaps = append(rec.gaps, e.TaskID)
		case EventTaskRetried:
			rec.retries[e.TaskID]++
		case EventTaskHandoff:
			rec.handoffs[e.TaskID]++
		case EventTaskCompleted:
			rec.terminalHistory[e.TaskID] = StatusCompleted
		case EventTaskFailed:
			rec.terminalHistory[e.TaskID] = StatusFailed
		case EventTaskBlocked:
			rec.terminalHistory[e.TaskID] = StatusBlocked
		}
	}
	return rec, scanner.Err()
}
