//go:build verify

package orch_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mock SessionPresence for testing ---

type mockSession struct {
	runID    string
	alive    map[string]bool   // session name → alive
	killed   []string          // sessions killed
	sessions []string          // all sessions (for ListSessions)
}

func newMockSession(runID string) *mockSession {
	return &mockSession{
		runID: runID,
		alive: make(map[string]bool),
	}
}

func (m *mockSession) HasSession(name string) (bool, error) {
	return m.alive[name], nil
}

func (m *mockSession) ListSessions() ([]string, error) {
	return m.sessions, nil
}

func (m *mockSession) KillSessionIfExists(name string) error {
	m.killed = append(m.killed, name)
	return nil
}

// --- Event log helper ---

func writeEvents(t *testing.T, path string, events []orch.Event) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range events {
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now()
		}
		require.NoError(t, enc.Encode(e))
	}
}

// --- Step 1: Stale assignment tests ---

// TestReconcile_StaleAssignmentReset verifies §11a.2 step 1: tasks with
// dead sessions are reset to open (BVV-ERR-08 inverse).
func TestReconcile_StaleAssignmentReset(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")

	// Simulate assigned+in_progress with a dead session.
	task.Status = orch.StatusInProgress
	task.Assignee = "w1"
	require.NoError(t, store.UpdateTask(task))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "build-1"}))

	tmux := newMockSession("run-1")
	// Session is NOT alive → stale.

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 1, result.Reconciled)

	got, _ := store.GetTask("build-1")
	assert.Equal(t, orch.StatusOpen, got.Status)
	assert.Empty(t, got.Assignee)
}

// TestReconcile_LiveSessionPreserved verifies BVV-ERR-08: in_progress tasks
// with live tmux sessions are NOT reset during reconciliation.
func TestReconcile_LiveSessionPreserved(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")

	task.Status = orch.StatusInProgress
	task.Assignee = "w1"
	require.NoError(t, store.UpdateTask(task))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "build-1"}))

	tmux := newMockSession("run-1")
	sessionName := orch.SessionName("run-1", "w1")
	tmux.alive[sessionName] = true

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 0, result.Reconciled)

	got, _ := store.GetTask("build-1")
	assert.Equal(t, orch.StatusInProgress, got.Status)
	assert.Equal(t, "w1", got.Assignee)
}

// TestReconcile_AssignedNoSession verifies that assigned (not yet in_progress)
// tasks with dead sessions are also reset.
func TestReconcile_AssignedNoSession(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")

	task.Status = orch.StatusAssigned
	task.Assignee = "w1"
	require.NoError(t, store.UpdateTask(task))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "build-1"}))

	tmux := newMockSession("run-1")
	// Session dead for assigned task.

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 1, result.Reconciled)

	got, _ := store.GetTask("build-1")
	assert.Equal(t, orch.StatusOpen, got.Status)
}

// --- Step 2: Orphan session tests ---

// TestReconcile_OrphanSessionKilled verifies §11a.2 step 2: tmux sessions
// with no corresponding in_progress task are killed.
func TestReconcile_OrphanSessionKilled(t *testing.T) {
	store := testutil.NewMockStore()
	testutil.SingleTask(t, store, "build-1", "feat/x", "builder")
	// build-1 is open — no session expected.

	tmux := newMockSession("run-1")
	tmux.sessions = []string{
		orch.SessionName("run-1", "w1"), // orphan — no in_progress task for w1
		orch.SessionName("run-1", "w2"), // orphan
	}

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 2, result.OrphanedSessions)
	assert.Len(t, tmux.killed, 2)
}

// --- Steps 3-5: Event log recovery tests ---

// TestReconcile_GapRecovery verifies §11a.2 step 3: gap_recorded events
// are recovered from the event log (BVV-ERR-05 monotonic).
func TestReconcile_GapRecovery(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := newMockSession("run-1")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventGapRecorded, TaskID: "task-a"},
		{Kind: orch.EventTaskCompleted, TaskID: "task-b"}, // not a gap
		{Kind: orch.EventGapRecorded, TaskID: "task-c"},
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"task-a", "task-c"}, result.GapsRecovered)
}

// TestReconcile_RetryRecovery verifies §11a.2 step 4: task_retried events
// are counted per task (BVV-ERR-01 monotonic).
func TestReconcile_RetryRecovery(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := newMockSession("run-1")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskRetried, TaskID: "task-a"},
		{Kind: orch.EventTaskRetried, TaskID: "task-a"},
		{Kind: orch.EventTaskRetried, TaskID: "task-b"},
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)
	assert.Equal(t, 2, result.RetriesRecovered["task-a"])
	assert.Equal(t, 1, result.RetriesRecovered["task-b"])
}

// TestReconcile_HandoffRecovery verifies §11a.2 step 5: task_handoff events
// are counted per task (BVV-L-04 monotonic).
func TestReconcile_HandoffRecovery(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := newMockSession("run-1")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskHandoff, TaskID: "task-a"},
		{Kind: orch.EventTaskHandoff, TaskID: "task-a"},
		{Kind: orch.EventTaskHandoff, TaskID: "task-a"},
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)
	assert.Equal(t, 3, result.HandoffsRecovered["task-a"])
}

// --- Step 6: Human re-open detection ---

// TestReconcile_HumanReopenDetection verifies BVV-S-02a: tasks that were
// terminal in the event log but are now open are flagged as human re-opens.
func TestReconcile_HumanReopenDetection(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")
	// Task is currently open — but was completed in event history.
	_ = task

	tmux := newMockSession("run-1")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskCompleted, TaskID: "build-1"},
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)
	assert.Contains(t, result.HumanReopens, "build-1")
}

// --- Step 7: Worker reset ---

// TestReconcile_WorkerReset verifies §11a.2 step 7: all workers are set
// to idle (except those with live sessions).
func TestReconcile_WorkerReset(t *testing.T) {
	store := testutil.NewMockStore()
	testutil.SingleTask(t, store, "build-1", "feat/x", "builder")

	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "old-task"}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerActive, CurrentTaskID: ""}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w3", Status: orch.WorkerIdle}))

	tmux := newMockSession("run-1")

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)

	w1, _ := store.GetWorker("w1")
	assert.Equal(t, orch.WorkerIdle, w1.Status)
	assert.Empty(t, w1.CurrentTaskID)

	w2, _ := store.GetWorker("w2")
	assert.Equal(t, orch.WorkerIdle, w2.Status)

	w3, _ := store.GetWorker("w3")
	assert.Equal(t, orch.WorkerIdle, w3.Status) // already idle
}

// TestReconcile_WorkerPreservedForLiveSession verifies that workers whose
// assigned task has a live session are NOT reset to idle (step 7 + ERR-08).
func TestReconcile_WorkerPreservedForLiveSession(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")

	task.Status = orch.StatusInProgress
	task.Assignee = "w1"
	require.NoError(t, store.UpdateTask(task))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "build-1"}))

	tmux := newMockSession("run-1")
	tmux.alive[orch.SessionName("run-1", "w1")] = true

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)

	w1, _ := store.GetWorker("w1")
	assert.Equal(t, orch.WorkerActive, w1.Status)
	assert.Equal(t, "build-1", w1.CurrentTaskID)
}

// --- Edge cases ---

// TestReconcile_EmptyState verifies that reconciliation on an empty store
// succeeds with a zero result.
func TestReconcile_EmptyState(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := newMockSession("run-1")

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 0, result.Reconciled)
	assert.Equal(t, 0, result.OrphanedSessions)
	assert.Nil(t, result.GapsRecovered)
	assert.Nil(t, result.HumanReopens)
}

// TestReconcile_MissingEventLog verifies that a non-existent event log path
// produces empty recovery (not an error).
func TestReconcile_MissingEventLog(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := newMockSession("run-1")

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "/nonexistent/events.jsonl")
	require.NoError(t, err)
	assert.Empty(t, result.GapsRecovered)
	assert.Empty(t, result.RetriesRecovered)
	assert.Empty(t, result.HandoffsRecovered)
}

// TestReconcile_TerminalTasksUntouched verifies BVV-S-02: terminal tasks
// (completed, failed, blocked) are never modified by reconciliation.
func TestReconcile_TerminalTasksUntouched(t *testing.T) {
	store := testutil.NewMockStore()

	for _, status := range []orch.TaskStatus{orch.StatusCompleted, orch.StatusFailed, orch.StatusBlocked} {
		task := testutil.SingleTask(t, store, "task-"+string(status), "feat/x", "builder")
		task.Status = status
		task.Assignee = "w1" // assigned but terminal
		require.NoError(t, store.UpdateTask(task))
	}

	tmux := newMockSession("run-1")

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 0, result.Reconciled)

	for _, status := range []orch.TaskStatus{orch.StatusCompleted, orch.StatusFailed, orch.StatusBlocked} {
		got, _ := store.GetTask("task-" + string(status))
		assert.Equal(t, status, got.Status, "terminal task %s should not change", status)
	}
}

// TestReconcile_CorruptEventLogLines verifies that malformed JSON lines
// are counted and surfaced via ResumeResult.EventLogCorruptLines.
// Corrupt lines must not be silently dropped — they would otherwise
// under-count BVV-ERR-01 / BVV-L-04 monotonic counters on resume.
func TestReconcile_CorruptEventLogLines(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := newMockSession("run-1")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	// Write a mix of valid and corrupt lines.
	f, err := os.Create(logPath)
	require.NoError(t, err)
	enc := json.NewEncoder(f)
	require.NoError(t, enc.Encode(orch.Event{Kind: orch.EventGapRecorded, TaskID: "task-a", Timestamp: time.Now()}))
	_, _ = f.WriteString("not valid json\n")
	_, _ = f.WriteString("{truncated\n")
	require.NoError(t, enc.Encode(orch.Event{Kind: orch.EventGapRecorded, TaskID: "task-b", Timestamp: time.Now()}))
	f.Close()

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"task-a", "task-b"}, result.GapsRecovered)
	assert.Equal(t, 2, result.EventLogCorruptLines, "both bad lines must be counted")
}

// TestReconcile_HasSessionError verifies that a tmux probe error must surface
// rather than silently leaving an in_progress task unverified. A swallowed
// probe error leaves the task in_progress with no live session, the
// dispatcher will not re-queue it (status != open), and the watchdog has
// no worker to monitor — a silent orphan that stalls the lifecycle.
func TestReconcile_HasSessionError(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")
	task.Status = orch.StatusInProgress
	task.Assignee = "w1"
	require.NoError(t, store.UpdateTask(task))

	tmux := &errSession{runID: "run-1", err: assertProbeErr}

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, assertProbeErr)

	// Task status must be unchanged so the operator can investigate.
	got, _ := store.GetTask("build-1")
	assert.Equal(t, orch.StatusInProgress, got.Status)
}

// errSession is a SessionPresence that returns a fixed error from HasSession.
type errSession struct {
	runID string
	err   error
}

func (e *errSession) HasSession(string) (bool, error)  { return false, e.err }
func (e *errSession) ListSessions() ([]string, error)  { return nil, nil }
func (e *errSession) KillSessionIfExists(string) error { return nil }

var assertProbeErr = errProbe("tmux: socket EBADF")

type errProbe string

func (e errProbe) Error() string { return string(e) }

// TestReconcile_FailedKillTracked verifies that when KillSessionIfExists
// errors, the session is recorded in FailedKills and NOT counted in
// OrphanedSessions. Reporting failed kills as cleaned up lies in the audit
// trail.
func TestReconcile_FailedKillTracked(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := &killErrSession{
		runID:    "run-1",
		sessions: []string{orch.SessionName("run-1", "w-orphan")},
	}

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 0, result.OrphanedSessions, "failed kill must not count as orphan cleanup")
	assert.Equal(t, []string{orch.SessionName("run-1", "w-orphan")}, result.FailedKills)
}

type killErrSession struct {
	runID    string
	sessions []string
}

func (k *killErrSession) HasSession(string) (bool, error) { return false, nil }
func (k *killErrSession) ListSessions() ([]string, error) { return k.sessions, nil }
func (k *killErrSession) KillSessionIfExists(string) error {
	return errProbe("tmux: kill failed")
}

// TestReconcile_MultipleBranches verifies that reconciliation only touches
// tasks on the specified branch (BVV-DSP-08 lifecycle scoping).
func TestReconcile_MultipleBranches(t *testing.T) {
	store := testutil.NewMockStore()

	// Branch feat/x — in_progress with dead session.
	taskX := testutil.SingleTask(t, store, "task-x", "feat/x", "builder")
	taskX.Status = orch.StatusInProgress
	taskX.Assignee = "w1"
	require.NoError(t, store.UpdateTask(taskX))

	// Branch feat/y — in_progress with dead session (should NOT be touched).
	taskY := testutil.SingleTask(t, store, "task-y", "feat/y", "builder")
	taskY.Status = orch.StatusInProgress
	taskY.Assignee = "w2"
	require.NoError(t, store.UpdateTask(taskY))

	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "task-x"}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerActive, CurrentTaskID: "task-y"}))

	tmux := newMockSession("run-1")

	// Reconcile only feat/x.
	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.NoError(t, err)
	assert.Equal(t, 1, result.Reconciled) // only task-x

	gotX, _ := store.GetTask("task-x")
	assert.Equal(t, orch.StatusOpen, gotX.Status)

	gotY, _ := store.GetTask("task-y")
	assert.Equal(t, orch.StatusInProgress, gotY.Status, "task on feat/y should be untouched")
}

// TestReconcile_HumanReopenNotTriggeredForCurrentlyTerminal verifies that
// step 6 only detects terminal→open transitions, not terminal→terminal.
func TestReconcile_HumanReopenNotTriggeredForCurrentlyTerminal(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")
	task.Status = orch.StatusCompleted
	require.NoError(t, store.UpdateTask(task))

	tmux := newMockSession("run-1")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskCompleted, TaskID: "build-1"},
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)
	assert.Empty(t, result.HumanReopens, "already-terminal task is not a re-open")
}

// TestReconcile_FullScenario exercises all 7 steps in a realistic scenario.
func TestReconcile_FullScenario(t *testing.T) {
	store := testutil.NewMockStore()

	// Task graph: build-1 (in_progress, dead), build-2 (in_progress, alive),
	//             verify-1 (open, was completed → human re-open).
	t1 := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")
	t1.Status = orch.StatusInProgress
	t1.Assignee = "w1"
	require.NoError(t, store.UpdateTask(t1))

	t2 := testutil.SingleTask(t, store, "build-2", "feat/x", "builder")
	t2.Status = orch.StatusInProgress
	t2.Assignee = "w2"
	require.NoError(t, store.UpdateTask(t2))

	// verify-1 is open (human re-opened after completion).
	_ = testutil.SingleTask(t, store, "verify-1", "feat/x", "verifier")

	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "build-1"}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w2", Status: orch.WorkerActive, CurrentTaskID: "build-2"}))
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w3", Status: orch.WorkerIdle}))

	tmux := newMockSession("run-1")
	// w2 session alive, w1 dead.
	tmux.alive[orch.SessionName("run-1", "w2")] = true
	// Orphan session that shouldn't exist.
	tmux.sessions = []string{
		orch.SessionName("run-1", "w2"),    // expected (build-2 alive)
		orch.SessionName("run-1", "w-old"), // orphan
	}

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskRetried, TaskID: "build-1"},
		{Kind: orch.EventTaskRetried, TaskID: "build-1"},
		{Kind: orch.EventTaskHandoff, TaskID: "build-2"},
		{Kind: orch.EventGapRecorded, TaskID: "old-task"},
		{Kind: orch.EventTaskCompleted, TaskID: "verify-1"}, // verify-1 was completed
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err)

	// Step 1: build-1 reset (dead session), build-2 preserved (alive).
	assert.Equal(t, 1, result.Reconciled)
	gotT1, _ := store.GetTask("build-1")
	assert.Equal(t, orch.StatusOpen, gotT1.Status)
	gotT2, _ := store.GetTask("build-2")
	assert.Equal(t, orch.StatusInProgress, gotT2.Status)

	// Step 2: orphan session killed.
	assert.Equal(t, 1, result.OrphanedSessions)

	// Steps 3-5: counters recovered.
	assert.Equal(t, []string{"old-task"}, result.GapsRecovered)
	assert.Equal(t, 2, result.RetriesRecovered["build-1"])
	assert.Equal(t, 1, result.HandoffsRecovered["build-2"])

	// Step 6: human re-open detected.
	assert.Contains(t, result.HumanReopens, "verify-1")

	// Step 7: w1 reset (dead task), w2 preserved (live session), w3 already idle.
	w1, _ := store.GetWorker("w1")
	assert.Equal(t, orch.WorkerIdle, w1.Status)
	w2, _ := store.GetWorker("w2")
	assert.Equal(t, orch.WorkerActive, w2.Status)
	w3, _ := store.GetWorker("w3")
	assert.Equal(t, orch.WorkerIdle, w3.Status)

	// Sort human reopens for determinism.
	sort.Strings(result.HumanReopens)
}
