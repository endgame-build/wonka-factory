package orch_test

import (
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Circuit Breaker Tests (SUP-05, SUP-06) ---

// TestSUP05_CircuitBreakerTripsAtThreshold verifies [SUP-05]: CB trips after N rapid failures.
func TestSUP05_CircuitBreakerTripsAtThreshold(t *testing.T) {
	cb := orch.NewCircuitBreaker(3, 60*time.Second)

	recentStart := time.Now().Add(-10 * time.Second) // session started 10s ago = rapid

	assert.False(t, cb.RecordFailure("w-01", recentStart))
	assert.False(t, cb.Tripped())
	assert.False(t, cb.RecordFailure("w-01", recentStart))
	assert.False(t, cb.Tripped())
	assert.True(t, cb.RecordFailure("w-01", recentStart), "3rd rapid failure should trip CB")
	assert.True(t, cb.Tripped())
}

// TestSUP06_RapidFailureWindow verifies [SUP-06]: only failures within window count.
func TestSUP06_RapidFailureWindow(t *testing.T) {
	cb := orch.NewCircuitBreaker(3, 60*time.Second)

	// Session that ran for 2 minutes = NOT rapid.
	longRunStart := time.Now().Add(-2 * time.Minute)
	assert.False(t, cb.RecordFailure("w-01", longRunStart))

	// Only short-lived sessions should contribute.
	recentStart := time.Now().Add(-5 * time.Second)
	assert.False(t, cb.RecordFailure("w-01", recentStart))
	assert.False(t, cb.RecordFailure("w-01", recentStart))
	assert.True(t, cb.RecordFailure("w-01", recentStart), "3 rapid failures should trip")
}

// TestSUP05_CircuitBreakerReset verifies Reset clears the tripped state.
func TestSUP05_CircuitBreakerReset(t *testing.T) {
	cb := orch.NewCircuitBreaker(1, 60*time.Second)
	recentStart := time.Now().Add(-5 * time.Second)
	cb.RecordFailure("w-01", recentStart)
	assert.True(t, cb.Tripped())

	cb.Reset()
	assert.False(t, cb.Tripped())
}

// TestSUP05_CircuitBreakerCrossWorker verifies rapid failures across workers count.
func TestSUP05_CircuitBreakerCrossWorker(t *testing.T) {
	cb := orch.NewCircuitBreaker(3, 60*time.Second)
	recentStart := time.Now().Add(-5 * time.Second)

	cb.RecordFailure("w-01", recentStart)
	cb.RecordFailure("w-02", recentStart)
	assert.True(t, cb.RecordFailure("w-03", recentStart), "3 rapid failures across workers should trip")
}

// --- Watchdog Behavioural Tests (SUP-01..04) ---

// TestSUP01_WatchdogDefaultConfig verifies [SUP-01]: default interval is 30s.
func TestSUP01_WatchdogDefaultConfig(t *testing.T) {
	cfg := orch.DefaultWatchdogConfig()
	assert.Equal(t, 30*time.Second, cfg.Interval)
	assert.Equal(t, 3, cfg.CBThreshold)
	assert.Equal(t, 60*time.Second, cfg.CBWindow)
}

// TestSUP04_WatchdogDoesNotChangeTaskStatus verifies [SUP-04, S11]:
// watchdog CheckOnce does not modify task status in the store.
func TestSUP04_WatchdogDoesNotChangeTaskStatus(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	store := newTestStoreInDir(t, dir)

	el, err := orch.NewEventLog(dir + "/events.jsonl")
	require.NoError(t, err)
	defer el.Close()

	runID := "test-sup04"
	tmuxClient := newTestTmux(t, runID)

	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, dir, dir)

	pipeline := &orch.Pipeline{
		Phases: []orch.Phase{{
			Agents: []orch.AgentDef{{ID: "test-agent", Output: "out.md", Format: orch.FormatMd}},
		}},
	}

	cfg := orch.WatchdogConfig{Interval: time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}
	wd := orch.NewWatchdog(pool, store, el, pipeline, testPreset(), "", cfg, nil)

	// Create a worker with an in_progress task.
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "task-01"}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "task-01", Status: orch.StatusInProgress, AgentID: "test-agent",
	}))

	// Snapshot task status before watchdog check.
	taskBefore, err := store.GetTask("task-01")
	require.NoError(t, err)

	// Run a watchdog cycle (session doesn't exist, so it's "dead").
	_ = wd.CheckOnce()

	// Verify task status unchanged (SUP-04).
	taskAfter, err := store.GetTask("task-01")
	require.NoError(t, err)
	assert.Equal(t, taskBefore.Status, taskAfter.Status, "watchdog must not change task status")
}

// TestOBS02a_WatchdogUsesOSChecks verifies [OBS-02a]: liveness via tmux has-session.
func TestOBS02a_WatchdogUsesOSChecks(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	store := newTestStoreInDir(t, dir)

	runID := "test-obs02a"
	tmuxClient := newTestTmux(t, runID)

	// Create a real tmux session, then verify IsAlive works.
	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, dir, dir)

	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-01", Status: orch.WorkerIdle}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "task-01", Status: orch.StatusOpen, AgentID: "test-agent", Output: "out.md",
	}))
	require.NoError(t, store.Assign("task-01", "w-01"))

	// Spawn a long-running session.
	require.NoError(t, pool.SpawnSession("w-01", &orch.Task{ID: "task-01", AgentID: "test-agent", Output: "out.md"},
		orch.AgentDef{ID: "test-agent", Output: "out.md", Format: orch.FormatMd},
		testPreset(), ""))

	alive, err := pool.IsAlive("w-01")
	require.NoError(t, err)
	assert.True(t, alive, "session should be alive")
}

// TestSUP05_CBTripOnDeadSession verifies [SUP-05]: CB trips when watchdog detects
// a dead session with a recent start time (rapid failure).
func TestSUP05_CBTripOnDeadSession(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	store := newTestStoreInDir(t, dir)

	logPath := dir + "/events.jsonl"
	el, err := orch.NewEventLog(logPath)
	require.NoError(t, err)
	defer el.Close()

	runID := "test-sup08"
	tmuxClient := newTestTmux(t, runID)

	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, dir, dir)

	pipeline := &orch.Pipeline{
		Phases: []orch.Phase{{
			Agents: []orch.AgentDef{{ID: "test-agent", Output: "out.md", Format: orch.FormatMd}},
		}},
	}

	// CB threshold = 1 for quick testing.
	cfg := orch.WatchdogConfig{Interval: time.Second, CBThreshold: 1, CBWindow: 60 * time.Second}
	wd := orch.NewWatchdog(pool, store, el, pipeline, testPreset(), "", cfg, nil)

	// Worker with dead session and recent start (rapid failure).
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "task-01",
		SessionStartedAt: time.Now().Add(-5 * time.Second),
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "task-01", Status: orch.StatusInProgress, AgentID: "test-agent",
	}))

	_ = wd.CheckOnce()
	assert.True(t, wd.CBTripped(), "CB should be tripped")
}
