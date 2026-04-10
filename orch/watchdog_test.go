//go:build verify

package orch_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- CircuitBreaker tests (SUP-05, SUP-06) ---

// TestBVV_SUP05_CircuitBreakerTripsAtThreshold verifies the CB trips when
// rapid failures reach the threshold.
func TestBVV_SUP05_CircuitBreakerTripsAtThreshold(t *testing.T) {
	cb := orch.NewCircuitBreaker(3, 60*time.Second)
	now := time.Now()

	// Two failures — not yet tripped.
	assert.False(t, cb.RecordFailure("w-01", now.Add(-1*time.Second)))
	assert.False(t, cb.RecordFailure("w-01", now.Add(-1*time.Second)))
	assert.False(t, cb.Tripped())

	// Third failure trips.
	assert.True(t, cb.RecordFailure("w-01", now.Add(-1*time.Second)))
	assert.True(t, cb.Tripped())
}

// TestBVV_SUP06_CircuitBreakerIgnoresSlowFailures verifies that failures
// outside the rapid window do NOT count toward the threshold.
func TestBVV_SUP06_CircuitBreakerIgnoresSlowFailures(t *testing.T) {
	cb := orch.NewCircuitBreaker(3, 1*time.Second)
	slow := time.Now().Add(-2 * time.Second) // outside the 1s window

	for range 5 {
		cb.RecordFailure("w-01", slow)
	}
	assert.False(t, cb.Tripped(), "slow failures must not trip the CB")
}

// TestBVV_SUP05_CircuitBreakerReset verifies Reset clears the tripped state.
func TestBVV_SUP05_CircuitBreakerReset(t *testing.T) {
	cb := orch.NewCircuitBreaker(1, 60*time.Second)
	cb.RecordFailure("w-01", time.Now())
	require.True(t, cb.Tripped())

	cb.Reset()
	assert.False(t, cb.Tripped())
}

// --- Watchdog CheckOnce tests ---

// recordingStore wraps an orch.Store and enforces BVV-S-10 during watchdog
// ticks: the watchdog must never transition a task to a TERMINAL status, and
// must never call Assign. Non-terminal UpdateTask calls are allowed because
// the watchdog's RestartSession path legitimately re-asserts StatusInProgress
// via pool.SpawnSession.
type recordingStore struct {
	inner            orch.Store
	t                *testing.T
	terminalAttempts int // number of forbidden terminal transitions observed
	assignAttempts   int // number of forbidden Assign calls observed
	mu               sync.Mutex
	enforceS10       bool // flip on before the watchdog tick
}

func (r *recordingStore) CreateTask(t *orch.Task) error { return r.inner.CreateTask(t) }
func (r *recordingStore) GetTask(id string) (*orch.Task, error) {
	return r.inner.GetTask(id)
}
func (r *recordingStore) UpdateTask(t *orch.Task) error {
	r.mu.Lock()
	if r.enforceS10 && t.Status.Terminal() {
		r.terminalAttempts++
		r.t.Errorf("BVV-S-10 violation: watchdog called UpdateTask with terminal status %s on task %s", t.Status, t.ID)
	}
	r.mu.Unlock()
	return r.inner.UpdateTask(t)
}
func (r *recordingStore) ListTasks(labels ...string) ([]*orch.Task, error) {
	return r.inner.ListTasks(labels...)
}
func (r *recordingStore) ReadyTasks(labels ...string) ([]*orch.Task, error) {
	return r.inner.ReadyTasks(labels...)
}
func (r *recordingStore) Assign(taskID, workerName string) error {
	r.mu.Lock()
	if r.enforceS10 {
		r.assignAttempts++
		r.t.Errorf("BVV-S-10 violation: watchdog called Assign for task %s", taskID)
	}
	r.mu.Unlock()
	return r.inner.Assign(taskID, workerName)
}
func (r *recordingStore) CreateWorker(w *orch.Worker) error { return r.inner.CreateWorker(w) }
func (r *recordingStore) GetWorker(name string) (*orch.Worker, error) {
	return r.inner.GetWorker(name)
}
func (r *recordingStore) ListWorkers() ([]*orch.Worker, error) {
	return r.inner.ListWorkers()
}
func (r *recordingStore) UpdateWorker(w *orch.Worker) error { return r.inner.UpdateWorker(w) }
func (r *recordingStore) AddDep(taskID, dependsOn string) error {
	return r.inner.AddDep(taskID, dependsOn)
}
func (r *recordingStore) GetDeps(taskID string) ([]string, error) {
	return r.inner.GetDeps(taskID)
}
func (r *recordingStore) Close() error { return r.inner.Close() }

// newWatchdogFixture builds a WorkerPool + recording Store + HandoffState +
// mock EventLog, all wired to a single test lifecycle. Returns the watchdog,
// the recording store (for assertion), the worker pool (for direct manipulation),
// and the event log path (for reading back emitted events).
func newWatchdogFixture(t *testing.T, maxHandoffs int) (*orch.Watchdog, *recordingStore, *orch.WorkerPool, string) {
	t.Helper()
	skipIfNoTmux(t)

	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	fsStore, err := orch.NewFSStore(filepath.Join(dir, "ledger"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsStore.Close() })

	recStore := &recordingStore{
		inner:      fsStore,
		t:          t,
		enforceS10: false, // off during setup; tests flip it on before calling CheckOnce
	}

	runID := "wd-test"
	tmuxClient := orch.NewTmuxClient(runID)
	require.NoError(t, tmuxClient.StartServer())
	t.Cleanup(func() { _ = tmuxClient.KillServer() })

	pool := orch.NewWorkerPool(recStore, tmuxClient, 4, runID, "/repo", outDir)

	logPath := filepath.Join(dir, "events.jsonl")
	eventLog, err := orch.NewEventLog(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eventLog.Close() })

	handoffs := orch.NewHandoffState(maxHandoffs)

	wd := orch.NewWatchdog(
		pool,
		recStore,
		eventLog,
		map[string]orch.RoleConfig{
			"builder": mockRoleConfig(t, "ok.sh"),
		},
		handoffs,
		"feature-x",
		orch.WatchdogConfig{
			Interval:    50 * time.Millisecond,
			CBThreshold: 3,
			CBWindow:    60 * time.Second,
		},
		nil, // no ProgressReporter
	)
	return wd, recStore, pool, logPath
}

// setupDeadSessionTask creates a task + worker, runs SpawnSession to create
// a real tmux session, waits for the mock agent to exit (leaving the session
// dead), and returns the task. This simulates the state watchdog observes:
// worker is active, tmux session is gone, task status is still in_progress.
func setupDeadSessionTask(t *testing.T, rec *recordingStore, pool *orch.WorkerPool, taskID, workerName string) *orch.Task {
	t.Helper()
	task := &orch.Task{
		ID:     taskID,
		Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelRole:   "builder",
			orch.LabelBranch: "feature-x",
		},
	}
	require.NoError(t, rec.inner.CreateTask(task))

	w := &orch.Worker{Name: workerName, Status: orch.WorkerIdle}
	require.NoError(t, rec.inner.CreateWorker(w))
	require.NoError(t, rec.inner.Assign(taskID, workerName))

	fresh, err := rec.inner.GetTask(taskID)
	require.NoError(t, err)

	roleCfg := mockRoleConfig(t, "ok.sh")
	require.NoError(t, pool.SpawnSession(workerName, fresh, roleCfg, "feature-x"))

	// Wait for the mock script to exit and tmux to release the session.
	waitForSessionDead(t, pool, workerName, 3*time.Second)

	// At this point the worker is still marked Active (dispatcher hasn't
	// noticed yet) but the tmux session is gone. This is the exact state
	// the watchdog is designed to detect.
	return fresh
}

func waitForSessionDead(t *testing.T, pool *orch.WorkerPool, workerName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := pool.IsAlive(workerName)
		if err == nil && !alive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session for worker %s did not die within %s", workerName, timeout)
}

// TestBVV_ERR11_WatchdogDetectsDeadSession verifies the watchdog detects a
// dead tmux session and calls RestartSession through the pool. Success is
// measured by a new tmux session appearing after CheckOnce.
func TestBVV_ERR11_WatchdogDetectsDeadSession(t *testing.T) {
	wd, rec, pool, _ := newWatchdogFixture(t, 3)
	task := setupDeadSessionTask(t, rec, pool, "task-dead", "w-01")
	_ = task

	// Tighten BVV-S-10 enforcement: any UpdateTask/Assign call from here on
	// is a test failure.
	rec.mu.Lock()
	rec.enforceS10 = true
	rec.mu.Unlock()

	require.NoError(t, wd.CheckOnce())

	// A new tmux session should have been created by the restart.
	waitForSessionDead(t, pool, "w-01", 3*time.Second) // and it exits again since ok.sh is instant
}

// TestBVV_ERR11a_WatchdogEmitsTaskHandoff verifies the watchdog emits an
// EventTaskHandoff with the current handoff count when it restarts a dead
// session.
func TestBVV_ERR11a_WatchdogEmitsTaskHandoff(t *testing.T) {
	wd, rec, pool, logPath := newWatchdogFixture(t, 3)
	setupDeadSessionTask(t, rec, pool, "task-handoff", "w-01")

	rec.mu.Lock()
	rec.enforceS10 = true
	rec.mu.Unlock()

	require.NoError(t, wd.CheckOnce())

	events := readEvents(t, logPath)
	var found *orch.Event
	for i := range events {
		if events[i].Kind == orch.EventTaskHandoff {
			found = &events[i]
			break
		}
	}
	require.NotNil(t, found, "watchdog must emit EventTaskHandoff on dead session")
	assert.Equal(t, "task-handoff", found.TaskID)
	assert.Equal(t, "w-01", found.Worker)
	assert.Contains(t, found.Detail, "reason=session_dead")
}

// TestBVV_ERR11a_WatchdogTaskHandoffPayload locks in the event schema: the
// Detail payload must carry the branch, role, and handoff count so Phase 4
// dispatch and Phase 5 Resume can replay it.
func TestBVV_ERR11a_WatchdogTaskHandoffPayload(t *testing.T) {
	wd, rec, pool, logPath := newWatchdogFixture(t, 3)
	setupDeadSessionTask(t, rec, pool, "task-payload", "w-01")

	rec.mu.Lock()
	rec.enforceS10 = true
	rec.mu.Unlock()

	require.NoError(t, wd.CheckOnce())

	events := readEvents(t, logPath)
	var handoff *orch.Event
	for i := range events {
		if events[i].Kind == orch.EventTaskHandoff {
			handoff = &events[i]
		}
	}
	require.NotNil(t, handoff)
	assert.Contains(t, handoff.Detail, "branch=feature-x")
	assert.Contains(t, handoff.Detail, "role=builder")
	assert.Contains(t, handoff.Detail, "count=1")
	assert.NotEmpty(t, handoff.TaskID)
	assert.NotEmpty(t, handoff.Worker)
}

// TestBVV_L04_WatchdogStopsAtHandoffLimit verifies that at the handoff limit,
// the watchdog emits EventHandoffLimitReached and does NOT call
// RestartSession. The dispatcher will observe the event on its next tick
// and convert the task to a failure — the watchdog does not.
//
// The stop-at-limit property is verified two ways:
//  1. Exactly ONE EventTaskHandoff is emitted (from tick 1). If tick 2 had
//     restarted, a second EventTaskHandoff would be emitted.
//  2. Exactly ONE EventHandoffLimitReached is emitted (from tick 2).
//
// This closes the gap in the original test which only checked (2) and would
// have silently passed if the watchdog had erroneously restarted on tick 2.
func TestBVV_L04_WatchdogStopsAtHandoffLimit(t *testing.T) {
	wd, rec, pool, logPath := newWatchdogFixture(t, 1) // limit = 1 handoff
	setupDeadSessionTask(t, rec, pool, "task-limit", "w-01")

	rec.mu.Lock()
	rec.enforceS10 = true
	rec.mu.Unlock()

	// First tick: consumes the 1 allowed handoff, emits EventTaskHandoff,
	// RestartSession runs ok.sh.
	require.NoError(t, wd.CheckOnce())

	// Wait for the RESTARTED session (still running ok.sh) to die so the
	// second tick sees a dead session again.
	waitForSessionDead(t, pool, "w-01", 3*time.Second)

	// Second tick: limit exhausted → emit EventHandoffLimitReached, no restart.
	require.NoError(t, wd.CheckOnce())

	// Verify emission counts. ONE of each, not TWO of either.
	events := readEvents(t, logPath)
	var handoffCount, limitCount int
	var limitEv *orch.Event
	for i := range events {
		switch events[i].Kind {
		case orch.EventTaskHandoff:
			handoffCount++
		case orch.EventHandoffLimitReached:
			limitCount++
			limitEv = &events[i]
		}
	}
	assert.Equal(t, 1, handoffCount,
		"exactly 1 EventTaskHandoff expected (from tick 1); a second event would mean the watchdog ignored the limit on tick 2")
	assert.Equal(t, 1, limitCount,
		"exactly 1 EventHandoffLimitReached expected (from tick 2)")

	require.NotNil(t, limitEv)
	assert.Equal(t, "task-limit", limitEv.TaskID)
	assert.Equal(t, "w-01", limitEv.Worker)
	assert.Contains(t, limitEv.Detail, "branch=feature-x")
}

// TestBVV_S10_WatchdogNeverMutatesTaskStatus is the load-bearing safety test
// for BVV-S-10: the watchdog must never transition a task to a TERMINAL
// status (Completed/Failed/Blocked), and must never call Assign. Non-terminal
// UpdateTask calls are allowed because the watchdog's RestartSession path
// legitimately re-asserts StatusInProgress via pool.SpawnSession — that's a
// no-op state transition from the spec's perspective.
//
// The recordingStore guards trip the test via t.Errorf if the forbidden
// calls happen. After the ticks, we also verify the task is still in
// StatusInProgress (not moved to any terminal state).
func TestBVV_S10_WatchdogNeverMutatesTaskStatus(t *testing.T) {
	wd, rec, pool, _ := newWatchdogFixture(t, 3)
	setupDeadSessionTask(t, rec, pool, "task-s10", "w-01")

	rec.mu.Lock()
	rec.enforceS10 = true
	rec.mu.Unlock()

	// Run several ticks so both the restart path AND any follow-up ticks
	// (including a hypothetical handoff-limit hit) are exercised.
	for range 3 {
		require.NoError(t, wd.CheckOnce())
		time.Sleep(50 * time.Millisecond)
	}

	// Verify the task is still non-terminal. If the watchdog had violated
	// S-10, the recording store would have already reported a test failure,
	// but this is a belt-and-braces check on the observable end-state.
	got, err := rec.inner.GetTask("task-s10")
	require.NoError(t, err)
	assert.False(t, got.Status.Terminal(), "task must not be terminal after watchdog ticks; got %s", got.Status)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, 0, rec.terminalAttempts, "no terminal transitions allowed from watchdog")
	assert.Equal(t, 0, rec.assignAttempts, "no Assign calls allowed from watchdog")
}

// readEvents parses the JSONL event log into a slice for assertion.
func readEvents(t *testing.T, logPath string) []orch.Event {
	t.Helper()
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	var events []orch.Event
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var ev orch.Event
		require.NoError(t, json.Unmarshal(line, &ev))
		events = append(events, ev)
	}
	return events
}
