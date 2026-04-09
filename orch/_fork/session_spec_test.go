package orch_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if !orch.Available() {
		t.Skip("tmux not available")
	}
}

// newTestTmux creates a TmuxClient, starts the server, and registers cleanup.
// Skips the test if tmux is not available.
func newTestTmux(t *testing.T, runID string) *orch.TmuxClient {
	t.Helper()
	skipIfNoTmux(t)
	tc := orch.NewTmuxClient(runID)
	require.NoError(t, tc.StartServer())
	t.Cleanup(func() { _ = tc.KillServer() })
	return tc
}

// newTestPool creates a WorkerPool backed by a fresh FSStore and TmuxClient
// in a temporary directory. Returns the pool, store, and output dir.
func newTestPool(t *testing.T) (*orch.WorkerPool, orch.Store, string) {
	t.Helper()
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)

	runID := "test-run"
	tmuxClient := newTestTmux(t, runID)

	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, "/repo", outDir)
	return pool, store, outDir
}

// createAssignedTask creates a task+worker pair in the store with the task assigned.
func createAssignedTask(t *testing.T, store orch.Store, taskID, workerName string) (*orch.Task, *orch.Worker) { //nolint:unparam // taskID varies in future tests
	t.Helper()
	task := &orch.Task{
		ID:      taskID,
		Type:    orch.TypeAgent,
		Status:  orch.StatusOpen,
		AgentID: "test-agent",
		Output:  "OUTPUT.md",
	}
	require.NoError(t, store.CreateTask(task))
	w := &orch.Worker{Name: workerName, Status: orch.WorkerIdle}
	require.NoError(t, store.CreateWorker(w))
	require.NoError(t, store.Assign(taskID, workerName))

	task, err := store.GetTask(taskID)
	require.NoError(t, err)
	w, err = store.GetWorker(workerName)
	require.NoError(t, err)
	return task, w
}

func testPreset() *orch.Preset {
	return &orch.Preset{
		Name:    "mock",
		Command: "sleep",
		Args:    []string{"3600"},
		Env:     map[string]string{},
	}
}

func testAgentDef() orch.AgentDef {
	return orch.AgentDef{
		ID:       "test-agent",
		MaxTurns: 0, // 0 so BuildCommand doesn't append --max-turns to sleep
		Output:   "OUTPUT.md",
		Format:   orch.FormatMd,
	}
}

// TestAllocate_CreatesNewWorkerUnderCapacity verifies Allocate creates new workers with w-NN naming.
func TestAllocate_CreatesNewWorkerUnderCapacity(t *testing.T) {
	pool, store, _ := newTestPool(t)

	// First allocation creates w-01.
	w, err := pool.Allocate()
	require.NoError(t, err)
	assert.Equal(t, "w-01", w.Name)
	assert.Equal(t, orch.WorkerIdle, w.Status)

	// Verify persisted in store.
	w, err = store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, "w-01", w.Name)
}

// TestAllocate_NamingSequence verifies w-01, w-02, w-03 sequential naming.
func TestAllocate_NamingSequence(t *testing.T) {
	pool, store, _ := newTestPool(t)

	// Allocate creates w-01. Mark active so next Allocate creates w-02.
	for i := range 3 {
		w, err := pool.Allocate()
		require.NoError(t, err)
		expected := fmt.Sprintf("w-%02d", i+1)
		assert.Equal(t, expected, w.Name)
		// Mark active so next Allocate creates a new worker instead of reusing.
		w.Status = orch.WorkerActive
		w.CurrentTaskID = fmt.Sprintf("task-%02d", i+1)
		require.NoError(t, store.UpdateWorker(w))
	}
}

// TestAllocate_ReusesIdleWorker verifies [WKR-07] Allocate returns existing idle workers.
func TestAllocate_ReusesIdleWorker(t *testing.T) {
	pool, store, _ := newTestPool(t)

	// Create two workers: one active, one idle.
	active := &orch.Worker{Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "task-01"}
	require.NoError(t, store.CreateWorker(active))
	idle := &orch.Worker{Name: "w-02", Status: orch.WorkerIdle}
	require.NoError(t, store.CreateWorker(idle))

	// Allocate should return the idle worker, not create a new one.
	w, err := pool.Allocate()
	require.NoError(t, err)
	assert.Equal(t, "w-02", w.Name, "should reuse idle worker")
}

// TestAllocate_PoolExhausted verifies ErrPoolExhausted when all slots are consumed.
func TestAllocate_PoolExhausted(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	ledgerDir := filepath.Join(dir, "ledger")
	store := newTestStoreInDir(t, ledgerDir)
	tmuxClient := newTestTmux(t, "test-exhaust")

	// Pool with maxWorkers=2.
	pool := orch.NewWorkerPool(store, tmuxClient, 2, "test-exhaust", "/repo", outDir)

	// Fill both slots with active workers.
	for _, name := range []string{"w-01", "w-02"} {
		w := &orch.Worker{Name: name, Status: orch.WorkerActive, CurrentTaskID: "some-task"}
		require.NoError(t, store.CreateWorker(w))
	}

	// Third allocation should fail.
	_, err := pool.Allocate()
	assert.ErrorIs(t, err, orch.ErrPoolExhausted)
}

// TestWKR01_IdentityPersistsAcrossSessions verifies [WKR-01]: identity survives session death.
func TestWKR01_IdentityPersistsAcrossSessions(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	// Spawn session.
	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	// Worker is active with assignment.
	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerActive, w.Status)
	assert.Equal(t, "task-01", w.CurrentTaskID)

	// Kill the tmux session (simulating crash).
	tmux := orch.NewTmuxClient("test-run")
	_ = tmux.KillSession(orch.SessionName("test-run", "w-01"))

	// Identity persists in ledger despite session death.
	w, err = store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, "w-01", w.Name, "WKR-01: name persists")
	assert.Equal(t, "task-01", w.CurrentTaskID, "WKR-01: assignment persists")
}

// TestWKR02_SingleActiveSession verifies [WKR-02]: at most one session per worker.
func TestWKR02_SingleActiveSession(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	// First spawn succeeds.
	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)
	alive, aliveErr := pool.IsAlive("w-01")
	require.NoError(t, aliveErr)
	assert.True(t, alive, "first session alive")

	// Worker is active — verify only one session exists.
	tmux := orch.NewTmuxClient("test-run")
	sessions, err := tmux.ListSessions()
	require.NoError(t, err)
	count := 0
	sessionName := orch.SessionName("test-run", "w-01")
	for _, s := range sessions {
		if s == sessionName {
			count++
		}
	}
	assert.Equal(t, 1, count, "WKR-02: exactly one session for worker")
}

// TestWKR03_AssignmentRecoverableFromLedger verifies [WKR-03]: ledger-only recovery.
func TestWKR03_AssignmentRecoverableFromLedger(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	// Simulate session death.
	tmux := orch.NewTmuxClient("test-run")
	_ = tmux.KillSession(orch.SessionName("test-run", "w-01"))

	// Recovery: read assignment from ledger only (no tmux state needed).
	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, "task-01", w.CurrentTaskID, "WKR-03: assignment recoverable from ledger")

	recoveredTask, err := store.GetTask(w.CurrentTaskID)
	require.NoError(t, err)
	assert.Equal(t, "w-01", recoveredTask.Assignee, "WKR-03: task assignee matches worker")
}

// TestWKR04_IdleStateSemantics verifies [WKR-04]: idle = no session, may have assignment.
func TestWKR04_IdleStateSemantics(t *testing.T) {
	_, store, _ := newTestPool(t)

	// Create an idle worker.
	w := &orch.Worker{Name: "w-01", Status: orch.WorkerIdle}
	require.NoError(t, store.CreateWorker(w))

	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerIdle, w.Status)
	assert.Zero(t, w.SessionPID, "WKR-04: idle worker has no session PID")
	assert.True(t, w.SessionStartedAt.IsZero(), "WKR-04: idle worker has no session start time")
}

// TestWKR05_ActiveStateSemantics verifies [WKR-05]: active = exactly one session.
func TestWKR05_ActiveStateSemantics(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerActive, w.Status, "WKR-05: worker is active")
	alive, aliveErr := pool.IsAlive("w-01")
	require.NoError(t, aliveErr)
	assert.True(t, alive, "WKR-05: session is alive")
	assert.False(t, w.SessionStartedAt.IsZero(), "WKR-05: session start time recorded")
}

// TestWKR06_ActiveToIdlePreservesAssignment verifies [WKR-06]: release preserves task assignment.
func TestWKR06_ActiveToIdlePreservesAssignment(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	// Release worker → idle.
	err = pool.Release("w-01")
	require.NoError(t, err)

	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerIdle, w.Status, "WKR-06: worker is idle after release")

	// Task assignment is unchanged in ledger.
	task, err = store.GetTask("task-01")
	require.NoError(t, err)
	assert.Equal(t, "w-01", task.Assignee, "WKR-06: task assignee preserved after release")
}

// TestWKR08_WorkspaceResetOnReuse verifies [WKR-08]: reuse clears outputs; restart does not.
func TestWKR08_WorkspaceResetOnReuse(t *testing.T) {
	pool, _, outDir := newTestPool(t)

	// Create a prior output artefact.
	priorOutput := "PRIOR_OUTPUT.md"
	priorPath := filepath.Join(outDir, priorOutput)
	writeFile(t, priorPath, generateValidMd("Prior"))
	require.FileExists(t, priorPath)

	// Reset workspace for new task assignment.
	err := pool.ResetWorkspace(priorOutput)
	require.NoError(t, err)
	assert.NoFileExists(t, priorPath, "WKR-08: prior output removed on reuse")

	// Same-task restart: empty previous output → no reset.
	err = pool.ResetWorkspace("")
	assert.NoError(t, err, "WKR-08: no-op on same-task restart")
}

// TestWKR11_NoDeallocateWithActiveAssignment verifies [WKR-11]: busy worker blocks deallocation.
func TestWKR11_NoDeallocateWithActiveAssignment(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	// Create worker with an assignment (CurrentTaskID set).
	w := &orch.Worker{Name: "w-01", Status: orch.WorkerIdle, CurrentTaskID: "task-01"}
	require.NoError(t, store.CreateWorker(w))

	err := pool.Deallocate("w-01")
	assert.ErrorIs(t, err, orch.ErrWorkerBusy, "WKR-11: cannot deallocate with active assignment")
}

// TestWKR12_DeallocationCleansUp verifies [WKR-12]: deallocation kills session.
func TestWKR12_DeallocationCleansUp(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	// Create idle worker with no assignment.
	w := &orch.Worker{Name: "w-01", Status: orch.WorkerIdle}
	require.NoError(t, store.CreateWorker(w))

	err := pool.Deallocate("w-01")
	require.NoError(t, err, "WKR-12: deallocation succeeds for idle worker without assignment")

	// Session should not exist.
	alive, aliveErr := pool.IsAlive("w-01")
	require.NoError(t, aliveErr)
	assert.False(t, alive, "WKR-12: no session after deallocation")
}

// TestCTY06_AssignmentPreservedAcrossRestarts verifies [CTY-06]: restart preserves assignment.
func TestCTY06_AssignmentPreservedAcrossRestarts(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	// Initial spawn.
	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	firstStart := time.Now()

	// Restart session (simulating watchdog restart).
	task, err = store.GetTask("task-01") // re-read for updated status
	require.NoError(t, err)
	err = pool.RestartSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	// Assignment unchanged.
	task, err = store.GetTask("task-01")
	require.NoError(t, err)
	assert.Equal(t, "w-01", task.Assignee, "CTY-06: assignee unchanged after restart")

	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, "task-01", w.CurrentTaskID, "CTY-06: worker assignment unchanged")
	assert.True(t, w.SessionStartedAt.After(firstStart) || w.SessionStartedAt.Equal(firstStart),
		"CTY-06: session start time updated")
}

// TestCTY07_MultipleRestartsSupported verifies [CTY-07]: unlimited restarts per task.
func TestCTY07_MultipleRestartsSupported(t *testing.T) {
	skipIfNoTmux(t)
	pool, store, _ := newTestPool(t)

	task, _ := createAssignedTask(t, store, "task-01", "w-01")

	// Initial spawn.
	err := pool.SpawnSession("w-01", task, testAgentDef(), testPreset(), "")
	require.NoError(t, err)

	// Restart 3+ times.
	for i := range 3 {
		task, err = store.GetTask("task-01")
		require.NoError(t, err)
		err = pool.RestartSession("w-01", task, testAgentDef(), testPreset(), "")
		require.NoError(t, err, "CTY-07: restart #%d failed", i+1)
		alive, aliveErr := pool.IsAlive("w-01")
		require.NoError(t, aliveErr)
		assert.True(t, alive, "CTY-07: session alive after restart #%d", i+1)
	}

	// Assignment still intact.
	task, err = store.GetTask("task-01")
	require.NoError(t, err)
	assert.Equal(t, "w-01", task.Assignee, "CTY-07: assignee unchanged after 3 restarts")
}
