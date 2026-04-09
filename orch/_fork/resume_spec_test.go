package orch_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/endgame/facet-scan/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Resume Tests (OPS-08, OPS-09, RCV-03..06) ---

// TestOPS08_ResumeFindsCorrectPhase verifies [OPS-08]: resume scans phases to find restart point.
func TestOPS08_ResumeFindsCorrectPhase(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-08")

	// Complete all tasks in phase 0 and write valid output files.
	phaseID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phaseID)
	require.NoError(t, err)
	for _, child := range children {
		child.Status = orch.StatusCompleted
		child.UpdatedAt = time.Now()
		require.NoError(t, store.UpdateTask(child))
		if child.Output != "" {
			outPath := filepath.Join(dir, child.Output)
			require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755))
			writeFile(t, outPath, "# Valid output\n"+largeBody)
		}
	}

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)
	assert.Equal(t, 1, result.ResumePhase, "should resume from phase 1 (second phase)")
}

// TestOPS09_ReconcileCompletedWithoutMarker verifies [OPS-09]:
// task marked in_progress but output exists → mark completed.
func TestOPS09_ReconcileCompletedWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-09a")

	// Find a task with output.
	phaseID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phaseID)
	require.NoError(t, err)
	require.NotEmpty(t, children)

	task := children[0]
	task.Status = orch.StatusInProgress
	task.Assignee = "w-01"
	task.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateTask(task))

	// Write valid output file.
	if task.Output != "" {
		outPath := filepath.Join(dir, task.Output)
		require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755))
		writeFile(t, outPath, "# Valid output\n"+largeBody)
	}

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)
	assert.Positive(t, result.Reconciled, "should reconcile at least one task")

	// Verify task is now completed.
	updated, err := store.GetTask(task.ID)
	require.NoError(t, err)
	if task.Output != "" {
		assert.Equal(t, orch.StatusCompleted, updated.Status, "task with valid output should be completed")
	}
}

// TestOPS09_ReconcileResetMissing verifies [OPS-09]:
// task marked completed but output missing → reset to open.
func TestOPS09_ReconcileResetMissing(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-09b")

	// Find a task with output and mark it completed.
	phaseID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phaseID)
	require.NoError(t, err)

	var taskWithOutput *orch.Task
	for _, child := range children {
		if child.Output != "" {
			taskWithOutput = child
			break
		}
	}
	if taskWithOutput == nil {
		t.Skip("no task with output in phase 0")
	}

	taskWithOutput.Status = orch.StatusCompleted
	taskWithOutput.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateTask(taskWithOutput))

	// Do NOT write the output file — it's "missing".

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)
	assert.Positive(t, result.Reconciled)

	updated, err := store.GetTask(taskWithOutput.ID)
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, updated.Status, "completed task with missing output should reset to open")
}

// TestCHK05_ResumeDetectsCrashMarkers verifies [CHK-05]: crash markers detected and cleared on resume.
func TestCHK05_ResumeDetectsCrashMarkers(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-chk05")

	// Find task with output and write crash marker.
	phaseID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phaseID)
	require.NoError(t, err)

	for _, child := range children {
		if child.Output != "" {
			outPath := filepath.Join(dir, child.Output)
			require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755))
			require.NoError(t, orch.WriteCrashMarker(outPath))
			break
		}
	}

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)
	assert.Positive(t, result.CrashMarkers, "should detect crash markers")
}

// TestERR07_ResumeRecoversGaps verifies [ERR-07]: gap count recovered from event log.
func TestERR07_ResumeRecoversGaps(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-gaps")

	// Write event log with gap events.
	logPath := filepath.Join(dir, "events.jsonl")
	f, err := os.Create(logPath)
	require.NoError(t, err)

	events := []orch.Event{
		{Kind: orch.EventGapRecorded, Agent: "agent-01", Summary: "gap 1/5"},
		{Kind: orch.EventGapRecorded, Agent: "agent-02", Summary: "gap 2/5"},
		{Kind: orch.EventAgentComplete, Agent: "agent-03", Summary: "done"}, // not a gap
	}
	for _, e := range events {
		e.Timestamp = time.Now()
		data, _ := json.Marshal(e)
		_, _ = f.Write(append(data, '\n'))
	}
	f.Close()

	result, err := orch.Resume(store, tmuxClient, p, dir, logPath)
	require.NoError(t, err)
	assert.Equal(t, 2, result.GapsRecovered, "should recover 2 gaps from event log")
	assert.Equal(t, []string{"agent-01", "agent-02"}, result.GapAgents)
}

// TestRCV03_ResumePreservesAssignments verifies [RCV-03, RCV-04, S9]: crash does not lose
// assignments. New session recovers from ledger (RCV-04). Assignment durability (S9).
// After resume, workers are reset to idle but task status is preserved via reconciliation.
func TestRCV03_ResumePreservesAssignments(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-rcv03")

	// Create a worker that was active before crash.
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "some-task",
	}))

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)

	// Worker should be reset to idle.
	worker, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerIdle, worker.Status, "worker should be reset to idle after resume")
	assert.Equal(t, "some-task", worker.CurrentTaskID, "WKR-06/RCV-03: assignment must survive resume")
	_ = result
}

// TestRCV05_OrphanDetectionViaTmux verifies [RCV-05]: resume kills tmux sessions not
// referenced by any active worker. killOrphanSessions is called during Resume step 3.
func TestRCV05_OrphanDetectionViaTmux(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	runID := "test-rcv05"
	tmuxClient := newTestTmux(t, runID)

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	// Create an orphan tmux session (not referenced by any worker).
	orphanName := orch.SessionName(runID, "w-orphan")
	require.NoError(t, tmuxClient.CreateSession(orphanName, "sleep 300", ""))

	// Verify session exists.
	sessions, err := tmuxClient.ListSessions()
	require.NoError(t, err)
	assert.Contains(t, sessions, orphanName, "orphan session should exist before resume")

	// Resume should kill orphan sessions in step 3.
	_, err = orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)

	// Verify orphan was killed.
	sessions, err = tmuxClient.ListSessions()
	require.NoError(t, err)
	assert.NotContains(t, sessions, orphanName, "orphan session should be killed by resume (RCV-05)")
}

// TestRCV06_StaleSessionDetection verifies that resume resets workers with stale session
// metadata to idle when no matching tmux session exists. This covers session-name-based
// detection only; PID + session_started_at verification per RCV-06 is deferred.
func TestRCV06_StaleSessionDetection(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	// Create a worker with stale session metadata (old PID and timestamp).
	staleTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "some-task",
		SessionPID: 99999, SessionStartedAt: staleTime,
	}))

	tmuxClient := newTestTmux(t, "test-rcv06")

	// Resume should reset stale workers to idle (no matching tmux session).
	_, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)

	worker, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerIdle, worker.Status,
		"stale worker should be reset to idle — no matching tmux session")
}

// --- Failed-Task Recovery Tests ---

// TestResume_FailedTasksWithMissingOutputs verifies that failed tasks with no
// output file are reset to open on resume, enabling re-dispatch after transient
// failures like API rate limits.
func TestResume_FailedTasksWithMissingOutputs(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.ConsensusPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-failed")

	// Fail all tasks in the consensus phase (simulating rate-limit cascade).
	phaseID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phaseID)
	require.NoError(t, err)

	failedCount := 0
	for _, child := range children {
		if child.Output != "" {
			child.Status = orch.StatusFailed
			child.UpdatedAt = time.Now()
			require.NoError(t, store.UpdateTask(child))
			failedCount++
		}
	}
	require.Positive(t, failedCount, "should have tasks with output to fail")

	// No output files written — simulating total failure.

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)
	assert.Equal(t, failedCount, result.FailuresReset,
		"all failed tasks with missing outputs should be reset")
	assert.Equal(t, 0, result.ResumePhase,
		"should resume from phase 0 since tasks were reset to open")

	// Verify all tasks are now open.
	children, err = store.GetChildren(phaseID)
	require.NoError(t, err)
	for _, child := range children {
		if child.Output != "" {
			assert.Equal(t, orch.StatusOpen, child.Status,
				"task %s should be reset to open", child.ID)
		}
	}
}

// TestResume_FailedTaskWithValidOutput verifies that a failed task whose output
// exists and is valid is NOT reset — the output is usable despite the non-zero exit.
func TestResume_FailedTaskWithValidOutput(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.ConsensusPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-fail-valid")

	phaseID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phaseID)
	require.NoError(t, err)

	// Find an instance task and a merge task.
	var instanceTask, mergeTask *orch.Task
	for _, child := range children {
		if child.Output == "" {
			continue
		}
		switch child.Type { //nolint:exhaustive // only need instance and merge for this test
		case orch.TypeConsensusInstance:
			if instanceTask == nil {
				instanceTask = child
			}
		case orch.TypeConsensusMerge:
			if mergeTask == nil {
				mergeTask = child
			}
		}
	}
	require.NotNil(t, instanceTask, "need an instance task")
	require.NotNil(t, mergeTask, "need a merge task")

	// Fail the instance but write valid output (exited non-zero but left output).
	instanceTask.Status = orch.StatusFailed
	instanceTask.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateTask(instanceTask))
	outPath := filepath.Join(dir, instanceTask.Output)
	require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755))
	writeFile(t, outPath, "# Valid output\n"+largeBody)

	// Fail the merge without output.
	mergeTask.Status = orch.StatusFailed
	mergeTask.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateTask(mergeTask))

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)

	// Instance with valid output should stay failed.
	updated, err := store.GetTask(instanceTask.ID)
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, updated.Status,
		"failed task with valid output should NOT be reset")

	// Merge without output should be reset to open.
	updated, err = store.GetTask(mergeTask.ID)
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, updated.Status,
		"failed merge with no output should be reset to open")

	assert.Positive(t, result.FailuresReset, "should have reset at least the merge task")
}

// TestResume_RetryTasksLeftAsFailed verifies that retry tasks from a prior run
// are NOT reset to open during resume. Only the original task is reset — the
// dispatch loop's retry mechanism handles retry tasks via createRetryTask.
func TestResume_RetryTasksLeftAsFailed(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-retry")

	// Find the first task with output in phase 1.
	phase1ID := p.ID + ":" + p.Phases[1].ID
	children, err := store.GetChildren(phase1ID)
	require.NoError(t, err)

	var origTask *orch.Task
	for _, child := range children {
		if child.Output != "" {
			origTask = child
			break
		}
	}
	require.NotNil(t, origTask, "need a task with output")

	// Fail the original task.
	origTask.Status = orch.StatusFailed
	origTask.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateTask(origTask))

	// Create a retry task in the store (simulating a prior run's retry).
	retryID := orch.RetryTaskID(origTask.AgentID, 1)
	retryTask := &orch.Task{
		ID:       retryID,
		ParentID: phase1ID,
		Type:     origTask.Type,
		Status:   orch.StatusFailed,
		AgentID:  origTask.AgentID,
		Output:   origTask.Output,
	}
	require.NoError(t, store.CreateTask(retryTask))

	// No output files written.

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)

	// Original task should be reset to open.
	updated, err := store.GetTask(origTask.ID)
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, updated.Status,
		"original failed task should be reset to open")

	// Retry task should remain failed (not reset).
	retryUpdated, err := store.GetTask(retryID)
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, retryUpdated.Status,
		"retry task should remain failed — dispatch loop handles retries")

	assert.Equal(t, 1, result.FailuresReset,
		"only the original task should count as a failure reset")
	assert.Equal(t, 1, result.RetryTasksSkipped,
		"the retry task should be counted as skipped")
}

// TestResume_MixedPhases verifies that completed phases with valid outputs
// are skipped, while phases with failed-no-output tasks are targeted for re-run.
func TestResume_MixedPhases(t *testing.T) {
	dir := t.TempDir()
	store := newTestStoreInDir(t, filepath.Join(dir, "ledger"))

	p := testutil.MiniPipeline()
	require.NoError(t, orch.Expand(p, store))

	tmuxClient := newTestTmux(t, "test-resume-mixed")

	// Complete phase 0 with valid output files.
	phase0ID := p.ID + ":" + p.Phases[0].ID
	children, err := store.GetChildren(phase0ID)
	require.NoError(t, err)
	for _, child := range children {
		child.Status = orch.StatusCompleted
		child.UpdatedAt = time.Now()
		require.NoError(t, store.UpdateTask(child))
		if child.Output != "" {
			outPath := filepath.Join(dir, child.Output)
			require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755))
			writeFile(t, outPath, "# Valid output\n"+largeBody)
		}
	}

	// Fail phase 1 tasks without writing output.
	phase1ID := p.ID + ":" + p.Phases[1].ID
	children, err = store.GetChildren(phase1ID)
	require.NoError(t, err)
	for _, child := range children {
		if child.Output != "" {
			child.Status = orch.StatusFailed
			child.UpdatedAt = time.Now()
			require.NoError(t, store.UpdateTask(child))
		}
	}

	result, err := orch.Resume(store, tmuxClient, p, dir, "")
	require.NoError(t, err)

	assert.Equal(t, 1, result.ResumePhase,
		"should resume from phase 1 (phase 0 completed, phase 1 has reset tasks)")
	assert.Positive(t, result.FailuresReset,
		"should have reset failed tasks in phase 1")

	// Phase 0 tasks should remain completed.
	children, err = store.GetChildren(phase0ID)
	require.NoError(t, err)
	for _, child := range children {
		if child.Output != "" {
			assert.Equal(t, orch.StatusCompleted, child.Status,
				"phase 0 task %s should remain completed", child.ID)
		}
	}
}
