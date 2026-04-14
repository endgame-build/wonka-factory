package orch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// SessionPresence abstracts tmux session operations needed by Reconcile.
// *TmuxClient satisfies this interface. Tests provide a mock.
type SessionPresence interface {
	HasSession(name string) (bool, error)
	ListSessions() ([]string, error)
	KillSessionIfExists(name string) error
}

// ResumeResult describes the outcome of DAG state reconciliation (BVV §11a.2).
// Returned by Reconcile for the Engine to restore in-memory state machines.
type ResumeResult struct {
	Reconciled        int            // stale assignments reset to open
	OrphanedSessions  int            // orphan tmux sessions successfully killed
	FailedKills       []string       // session names whose kill returned an error
	GapsRecovered     []string       // task IDs from gap_recorded events
	RetriesRecovered  map[string]int // taskID → retry count from event log
	HandoffsRecovered map[string]int // taskID → handoff count from event log
	HumanReopens      []string       // task IDs where terminal→open detected (BVV-S-02a)

	// EventLogCorruptLines counts JSONL lines that failed to parse during
	// recovery. A corrupt mid-record line truncates downstream counter
	// recovery (BVV-ERR-01 / BVV-L-04 monotonic guarantees), and the
	// single-pass scan makes Reconcile the only chance to notice.
	EventLogCorruptLines int
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
	runID string,
	branch string,
	logPath string,
) (*ResumeResult, error) {
	result := &ResumeResult{
		RetriesRecovered:  make(map[string]int),
		HandoffsRecovered: make(map[string]int),
	}
	branchLabel := LabelBranch + ":" + branch

	// Step 1: Stale assignment reset (§11a.2 step 1; BVV-ERR-08 defines the
	// preservation exception for live sessions).
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
		sessionName := SessionName(runID, task.Assignee)
		alive, hasErr := tmux.HasSession(sessionName)
		if hasErr != nil {
			// Surface the error rather than silently skipping. Leaving the
			// task in_progress with an unverifiable session corrupts state:
			// the dispatcher will not re-queue (status != open) and the
			// watchdog has no worker to monitor.
			return nil, fmt.Errorf("reconcile: probe session %s for task %s: %w",
				sessionName, task.ID, hasErr)
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
		tasks[i] = task // keep slice in sync — step 2 reads task.Status below
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
				expected[SessionName(runID, t.Assignee)] = true
			}
		}
		for _, session := range sessions {
			if !expected[session] {
				// Only count successful kills. Reporting a kill that errored
				// as "cleaned up" lies in the audit trail and lets stale
				// sessions accumulate while the operator believes state is
				// clean.
				if killErr := tmux.KillSessionIfExists(session); killErr != nil {
					result.FailedKills = append(result.FailedKills, session)
					continue
				}
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
		result.EventLogCorruptLines = rec.corruptLines

		// Step 6: Human re-open detection (BVV-S-02a).
		// A task that was terminal in the event log but is now open was
		// re-opened by a human. terminalHistory only contains terminal
		// statuses by construction, so the store-side check is sufficient.
		for taskID := range rec.terminalHistory {
			task, getErr := store.GetTask(taskID)
			if getErr != nil {
				if errors.Is(getErr, ErrNotFound) {
					continue // task deleted by operator — not a re-open
				}
				return nil, fmt.Errorf("reconcile: get task %s: %w", taskID, getErr)
			}
			if task.Status == StatusOpen {
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
	gaps            []string              // task IDs from gap_recorded events
	retries         map[string]int        // taskID → retry count
	handoffs        map[string]int        // taskID → handoff count
	terminalHistory map[string]TaskStatus // taskID → last terminal status
	corruptLines    int                   // unparseable JSONL lines
}

// maxEventLogLine bounds the per-line buffer for the event-log scanner.
// Defaults of bufio.Scanner truncate at 64 KiB, which silently drops large
// Detail/Summary payloads and skips every subsequent line. 16 MiB matches
// the practical ceiling of a single Event after JSON encoding.
const maxEventLogLine = 16 * 1024 * 1024

// recoverFromEventLog performs a single-pass scan of the event log and
// extracts gap task IDs, per-task retry counts, per-task handoff counts,
// and the last terminal status per task. Missing log file → zero values, nil.
//
// Corrupt JSON lines are counted, not silently dropped. The single-pass
// optimisation makes this the only chance to notice corruption that would
// otherwise under-count BVV-ERR-01 / BVV-L-04 monotonic counters; callers
// must surface eventLogRecovery.corruptLines.
func recoverFromEventLog(logPath string) (*eventLogRecovery, error) {
	rec := &eventLogRecovery{
		retries:         make(map[string]int),
		handoffs:        make(map[string]int),
		terminalHistory: make(map[string]TaskStatus),
	}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return rec, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxEventLogLine)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			rec.corruptLines++
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
	if err := scanner.Err(); err != nil {
		// Partial recovery is worse than none — counters would be
		// silently truncated. Return the error so Reconcile fails.
		return nil, fmt.Errorf("event log scan: %w", err)
	}
	return rec, nil
}
