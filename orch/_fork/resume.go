package orch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ResumeResult describes the outcome of state reconstruction.
type ResumeResult struct {
	ResumePhase       int      // phase index to resume from (0-based)
	Reconciled        int      // number of tasks whose status was corrected
	FailuresReset     int      // failed non-retry tasks reset to open (output missing or invalid)
	RetryTasksSkipped int      // failed retry tasks left as-is (dispatch handles them)
	CrashMarkers      int      // crash markers detected and cleared
	OrphanedPIDs      int      // orphaned tmux sessions killed
	GapsRecovered     int      // gaps recovered from prior session
	GapAgents         []string // agent IDs that contributed recovered gaps
}

// Resume reconstructs pipeline state from the ledger and filesystem (OPS-08, OPS-09).
//
// Algorithm:
//  1. Verify ledger exists and is readable (OPS-09 prerequisite).
//  2. Reconcile ledger ↔ filesystem (OPS-09):
//     a. For each task with status != completed: if output exists + valid → mark completed.
//     b. For each task with status == in_progress: if crash marker → delete marker, reset to open.
//     c. For each task with status == in_progress: reset to open (session is gone).
//  3. Detect and kill orphaned tmux sessions (RCV-05). Session-name matching only;
//     PID/identity verification (RCV-06) not implemented for tmux-isolated sockets.
//  4. Reset all workers to idle (clear stale session state).
//  5. Scan phases forward to find resume point (OPS-08).
//  6. Recover gap count from event log (ERR-07 monotonic).
func Resume(
	store Store,
	tmux *TmuxClient,
	pipeline *Pipeline,
	outputDir string,
	logPath string,
) (*ResumeResult, error) {
	result := &ResumeResult{}
	agentIndex := BuildAgentIndex(pipeline)

	// Step 1: Verify we can read the store.
	if _, err := store.GetTask(pipeline.ID); err != nil {
		return nil, fmt.Errorf("resume: %w: %v", ErrResumeNoLedger, err)
	}

	// Step 2: Reconcile task statuses.
	for i := range pipeline.Phases {
		phase := &pipeline.Phases[i]
		phaseID := pipeline.ID + ":" + phase.ID
		children, err := store.GetChildren(phaseID)
		if err != nil {
			return nil, fmt.Errorf("resume: get children for phase %s: %w", phaseID, err)
		}

		for _, task := range children {
			wasFailed := task.Status == StatusFailed
			if wasFailed && isRetryTask(task.ID) {
				result.RetryTasksSkipped++
			}
			corrected, err := reconcileTask(store, task, outputDir, agentIndex)
			if err != nil {
				return nil, fmt.Errorf("resume: reconcile task %s: %w", task.ID, err)
			}
			if corrected {
				result.Reconciled++
				if wasFailed {
					result.FailuresReset++
				}
			}

			// Check crash markers (CHK-05).
			if task.Output != "" {
				outputPath := filepath.Join(outputDir, task.Output)
				if isCrash, _ := IsCrashMarker(outputPath); isCrash { //nolint:errcheck // best-effort crash detection
					_ = RemoveCrashMarker(outputPath) //nolint:errcheck // best-effort cleanup
					result.CrashMarkers++
				}
			}

			// Recursively reconcile children (consensus instance/merge/verify).
			grandchildren, err := store.GetChildren(task.ID)
			if err != nil {
				return nil, fmt.Errorf("resume: get children for task %s: %w", task.ID, err)
			}
			for _, gc := range grandchildren {
				gcWasFailed := gc.Status == StatusFailed
				if gcWasFailed && isRetryTask(gc.ID) {
					result.RetryTasksSkipped++
				}
				corrected, err := reconcileTask(store, gc, outputDir, agentIndex)
				if err != nil {
					return nil, fmt.Errorf("resume: reconcile task %s: %w", gc.ID, err)
				}
				if corrected {
					result.Reconciled++
					if gcWasFailed {
						result.FailuresReset++
					}
				}
				if gc.Output != "" {
					outputPath := filepath.Join(outputDir, gc.Output)
					if isCrash, _ := IsCrashMarker(outputPath); isCrash { //nolint:errcheck // best-effort crash detection
						_ = RemoveCrashMarker(outputPath) //nolint:errcheck // best-effort cleanup
						result.CrashMarkers++
					}
				}
			}
		}
	}

	// Step 3: Kill orphaned tmux sessions (RCV-05).
	orphaned, err := killOrphanSessions(store, tmux, tmux.RunID())
	if err != nil {
		return nil, fmt.Errorf("resume: kill orphan sessions: %w", err)
	}
	result.OrphanedPIDs = orphaned

	// Step 4: Reset all workers to idle (WKR-06, RCV-03).
	// CurrentTaskID is preserved so the dispatch loop can recover the assignment.
	// Only session-specific fields (PID, timestamp) are cleared since sessions
	// were killed in Step 3.
	workers, err := store.ListWorkers()
	if err != nil {
		return nil, fmt.Errorf("resume: list workers: %w", err)
	}
	for _, w := range workers {
		if w.Status != WorkerActive {
			continue
		}
		w.Status = WorkerIdle
		w.SessionPID = 0
		w.SessionStartedAt = time.Time{}
		if err := store.UpdateWorker(w); err != nil {
			return nil, fmt.Errorf("resume: reset worker %s: %w", w.Name, err)
		}
	}

	// Step 5: Find resume phase (OPS-08).
	result.ResumePhase = findResumePhase(store, pipeline)

	// Step 6: Recover gap count from event log (ERR-07).
	if logPath != "" {
		count, agents, err := recoverGaps(logPath)
		if err != nil {
			return nil, fmt.Errorf("resume: recover gaps: %w", err)
		}
		result.GapsRecovered = count
		result.GapAgents = agents
	}

	return result, nil
}

// resetToOpen clears a task's status and assignee so dispatch can re-run it.
func resetToOpen(store Store, task *Task) error {
	task.Status = StatusOpen
	task.Assignee = ""
	task.UpdatedAt = time.Now()
	return store.UpdateTask(task)
}

// reconcileTask checks a single task's ledger state against filesystem state.
// Returns true if the task status was corrected.
func reconcileTask(store Store, task *Task, outputDir string, agentIndex map[string]AgentDef) (bool, error) {
	if task.Output == "" {
		return false, nil // phase/pipeline tasks — no output to check
	}

	agentDef, ok := agentIndex[task.AgentID]
	if !ok {
		return false, nil
	}

	outputPath := filepath.Join(outputDir, task.Output)

	switch {
	case task.Status == StatusInProgress || task.Status == StatusAssigned:
		// Session is gone (we're resuming). If output is valid, mark completed.
		if err := ValidateOutput(outputPath, agentDef.Format); err == nil {
			task.Status = StatusCompleted
			task.UpdatedAt = time.Now()
			if err := store.UpdateTask(task); err != nil {
				return false, err
			}
			return true, nil
		}
		if err := resetToOpen(store, task); err != nil {
			return false, err
		}
		return true, nil

	case task.Status == StatusCompleted:
		// Verify output still exists and is valid. If not, reset to open.
		if err := ValidateOutput(outputPath, agentDef.Format); err != nil {
			if err := resetToOpen(store, task); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil

	case task.Status == StatusFailed:
		// Skip retry tasks — the dispatch loop's retry mechanism handles them.
		// Resetting retry tasks here would create duplicate open tasks for the
		// same agent. The original task gets reset; if it fails again, the
		// dispatch loop reuses existing retry task IDs via createRetryTask.
		if isRetryTask(task.ID) {
			return false, nil
		}
		// If output is missing or invalid, reset to open for re-dispatch.
		// If output is valid (agent exited non-zero but left usable output), leave it.
		if err := ValidateOutput(outputPath, agentDef.Format); err != nil {
			if err := resetToOpen(store, task); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	return false, nil
}

// findResumePhase scans phases forward to find the first non-complete phase (OPS-08).
//
// OPS-08 describes a reverse scan then forward pick, but for well-formed pipelines
// the forward scan produces an identical result. In corruption cases (phase N looks
// complete but phase N-1 doesn't), the forward scan is more conservative — it resumes
// at the earlier incomplete phase rather than skipping it.
func findResumePhase(store Store, pipeline *Pipeline) int {
	for i := range pipeline.Phases {
		phaseID := pipeline.ID + ":" + pipeline.Phases[i].ID
		children, err := store.GetChildren(phaseID)
		if err != nil {
			return i // can't read → start here
		}

		allTerminal := true
		for _, child := range children {
			if !child.Status.Terminal() {
				allTerminal = false
				break
			}
		}
		if !allTerminal {
			return i
		}
	}
	// All phases terminal — pipeline is done.
	return len(pipeline.Phases) - 1
}

// killOrphanSessions lists all tmux sessions and kills any not referenced
// by an active worker in the store (RCV-05).
func killOrphanSessions(store Store, tmux *TmuxClient, runID string) (int, error) {
	sessions, err := tmux.ListSessions()
	if err != nil {
		return 0, err
	}

	workers, err := store.ListWorkers()
	if err != nil {
		return 0, err
	}

	// Build set of expected session names.
	expected := make(map[string]bool)
	for _, w := range workers {
		if w.Status == WorkerActive {
			expected[SessionName(runID, w.Name)] = true
		}
	}

	killed := 0
	for _, session := range sessions {
		if !expected[session] {
			_ = tmux.KillSessionIfExists(session)
			killed++
		}
	}
	return killed, nil
}

// recoverGaps reads the event log and counts gap_recorded events (ERR-07).
func recoverGaps(logPath string) (count int, agents []string, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e Event
		if jsonErr := json.Unmarshal(scanner.Bytes(), &e); jsonErr != nil {
			continue
		}
		if e.Kind == EventGapRecorded && e.Agent != "" {
			count++
			agents = append(agents, e.Agent)
		}
	}
	return count, agents, scanner.Err()
}
