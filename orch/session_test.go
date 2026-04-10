//go:build verify

package orch_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
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

// newTestSessionPool creates a WorkerPool backed by a fresh FSStore and
// TmuxClient in a temporary directory. Returns the pool, store, and run dir.
func newTestSessionPool(t *testing.T) (*orch.WorkerPool, orch.Store, string) {
	t.Helper()
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	store, err := orch.NewFSStore(filepath.Join(dir, "ledger"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	runID := "test-run"
	tmuxClient := newTestTmux(t, runID)

	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, "/repo", outDir)
	return pool, store, outDir
}

// mockRoleConfig returns a RoleConfig whose Preset runs a mock shell script
// from testdata/mock-agents/. The script path is absolute so the tmux session
// can exec it regardless of the session's working directory.
func mockRoleConfig(t *testing.T, scriptName string) orch.RoleConfig {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	scriptPath := filepath.Join(wd, "testdata", "mock-agents", scriptName)
	instructionPath := filepath.Join(wd, "testdata", "mock-agents", "builder.md")

	return orch.RoleConfig{
		InstructionFile: instructionPath,
		Preset: &orch.Preset{
			Name:    "mock",
			Command: scriptPath,
			// No SystemPromptFlag — the mock scripts don't accept flags, and
			// BuildCommand correctly omits the flag when empty.
			Env: map[string]string{},
		},
	}
}

// createAssignedTask creates a task+worker pair in the store with the task
// assigned to the worker (StatusAssigned). SpawnSession expects this state
// and transitions Assigned → InProgress.
func createAssignedTask(t *testing.T, store orch.Store, taskID, workerName string) (*orch.Task, *orch.Worker) {
	t.Helper()
	task := &orch.Task{
		ID:     taskID,
		Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelRole:   "builder",
			orch.LabelBranch: "feature-x",
		},
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

// --- Allocate tests (WKR-04..07) ---

// TestBVV_WKR04_AllocateCreatesNewWorker verifies Allocate creates workers
// with w-NN sequential naming.
func TestBVV_WKR04_AllocateCreatesNewWorker(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)

	w, err := pool.Allocate()
	require.NoError(t, err)
	assert.Equal(t, "w-01", w.Name)
	assert.Equal(t, orch.WorkerIdle, w.Status)

	// Persisted.
	got, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, "w-01", got.Name)
}

// TestBVV_WKR04_AllocateNamingSequence verifies w-01, w-02, w-03 sequential
// naming when each is marked active before the next Allocate.
func TestBVV_WKR04_AllocateNamingSequence(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)

	for i := range 3 {
		w, err := pool.Allocate()
		require.NoError(t, err)
		expected := fmt.Sprintf("w-%02d", i+1)
		assert.Equal(t, expected, w.Name)
		// Mark active so the next Allocate creates a fresh worker.
		w.Status = orch.WorkerActive
		w.CurrentTaskID = fmt.Sprintf("task-%02d", i+1)
		require.NoError(t, store.UpdateWorker(w))
	}
}

// TestBVV_WKR07_AllocateReusesIdleWorker verifies an idle worker is reused
// rather than creating a new one.
func TestBVV_WKR07_AllocateReusesIdleWorker(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)

	active := &orch.Worker{Name: "w-01", Status: orch.WorkerActive, CurrentTaskID: "task-01"}
	require.NoError(t, store.CreateWorker(active))
	idle := &orch.Worker{Name: "w-02", Status: orch.WorkerIdle}
	require.NoError(t, store.CreateWorker(idle))

	w, err := pool.Allocate()
	require.NoError(t, err)
	assert.Equal(t, "w-02", w.Name, "idle worker must be reused")
}

// TestBVV_WKR05_AllocatePoolExhausted verifies ErrPoolExhausted when all
// worker slots are consumed.
func TestBVV_WKR05_AllocatePoolExhausted(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	store, err := orch.NewFSStore(filepath.Join(dir, "ledger"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	tmuxClient := newTestTmux(t, "test-exhaust")

	// Pool with maxWorkers=2, both already active.
	pool := orch.NewWorkerPool(store, tmuxClient, 2, "test-exhaust", "/repo", outDir)
	for i := 1; i <= 2; i++ {
		w := &orch.Worker{
			Name:          fmt.Sprintf("w-%02d", i),
			Status:        orch.WorkerActive,
			CurrentTaskID: fmt.Sprintf("task-%02d", i),
		}
		require.NoError(t, store.CreateWorker(w))
	}

	_, err = pool.Allocate()
	require.ErrorIs(t, err, orch.ErrPoolExhausted)
}

// --- SpawnSession tests ---

// TestBVV_DSP05_SpawnSessionExitZero verifies the happy path: SpawnSession
// runs ok.sh, the tmux session exits 0, and the sidecar exit-code file
// reads 0 (BVV Appendix A). Also verifies the worker → active and task →
// in_progress state transitions.
func TestBVV_DSP05_SpawnSessionExitZero(t *testing.T) {
	pool, store, outDir := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-ok", "w-01")

	roleCfg := mockRoleConfig(t, "ok.sh")
	require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

	// Worker transitioned to active.
	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerActive, w.Status)
	assert.Equal(t, "task-ok", w.CurrentTaskID)

	// Task transitioned from Assigned → InProgress.
	got, err := store.GetTask("task-ok")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusInProgress, got.Status)

	// Wait for the session to exit and the sidecar to be written.
	// ok.sh exits immediately, but tmux + bash add a few ms of latency.
	logPath := orch.LogPath(outDir, "task-ok")
	waitForSidecar(t, logPath, 3*time.Second)

	code, err := orch.ReadExitCode(logPath)
	require.NoError(t, err)
	assert.Equal(t, 0, code, "ok.sh exits 0")
}

// TestBVV_DSP05_SpawnSessionExitOne verifies that a failing mock agent
// records the correct exit code in the sidecar. BVV-DSP-04: exit code is
// the sole outcome signal; the orchestrator never reads agent output.
func TestBVV_DSP05_SpawnSessionExitOne(t *testing.T) {
	pool, store, outDir := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-fail", "w-01")

	roleCfg := mockRoleConfig(t, "fail.sh")
	require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

	logPath := orch.LogPath(outDir, "task-fail")
	waitForSidecar(t, logPath, 3*time.Second)

	code, err := orch.ReadExitCode(logPath)
	require.NoError(t, err)
	assert.Equal(t, 1, code, "fail.sh exits 1")
}

// TestBVV_DSP05_SpawnSessionNilPresetError verifies the nil-preset guard.
// A role config with a nil preset is a programming error the dispatcher
// must catch before calling SpawnSession.
func TestBVV_DSP05_SpawnSessionNilPresetError(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-nil", "w-01")

	badCfg := orch.RoleConfig{
		InstructionFile: "ignored",
		Preset:          nil,
	}
	err := pool.SpawnSession("w-01", task, badCfg, "feature-x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil preset")
}

// TestBVV_ERR11_RestartSessionReplacesExisting verifies the watchdog
// happy-path: an existing session is killed and a new one takes its place.
// Task assignment is unchanged (CTY-06). The HandoffState counter is NOT
// touched by WorkerPool — that accounting lives in the watchdog.
func TestBVV_ERR11_RestartSessionReplacesExisting(t *testing.T) {
	pool, store, outDir := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-restart", "w-01")

	roleCfg := mockRoleConfig(t, "ok.sh")
	require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

	// Wait for the first session to exit so we're not racing its cleanup.
	logPath := orch.LogPath(outDir, "task-restart")
	waitForSidecar(t, logPath, 3*time.Second)

	// Clear the sidecar so the restart's exit code is unambiguous.
	_ = os.Remove(logPath + ".exitcode")

	// Restart with the same role — task assignment preserved.
	require.NoError(t, pool.RestartSession("w-01", task, roleCfg, "feature-x"))

	// New session runs ok.sh again; sidecar reappears with exit 0.
	waitForSidecar(t, logPath, 3*time.Second)
	code, err := orch.ReadExitCode(logPath)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}

// --- Release / Deallocate tests ---

// TestBVV_WKR04_ReleaseKillsSessionAndIdles verifies Release kills the tmux
// session and transitions the worker to idle. The task Assignee field is
// NOT cleared — that's the dispatcher's job per BVV-S-02 (only the dispatcher
// mutates task ownership).
func TestBVV_WKR04_ReleaseKillsSessionAndIdles(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-rel", "w-01")

	roleCfg := mockRoleConfig(t, "ok.sh")
	require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

	require.NoError(t, pool.Release("w-01"))

	// Worker is idle, session state cleared.
	w, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerIdle, w.Status)
	assert.Empty(t, w.CurrentTaskID)
	assert.Zero(t, w.SessionStartedAt)

	// Task Assignee is NOT cleared (dispatcher owns that transition).
	got, err := store.GetTask("task-rel")
	require.NoError(t, err)
	assert.Equal(t, "w-01", got.Assignee, "Release must not clear task.Assignee")
}

// TestBVV_WKR11_DeallocateRejectsBusy verifies Deallocate returns
// ErrWorkerBusy when a worker still has CurrentTaskID set.
func TestBVV_WKR11_DeallocateRejectsBusy(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)

	w := &orch.Worker{
		Name:          "w-01",
		Status:        orch.WorkerActive,
		CurrentTaskID: "task-hot",
	}
	require.NoError(t, store.CreateWorker(w))

	err := pool.Deallocate("w-01")
	require.ErrorIs(t, err, orch.ErrWorkerBusy)
}

// --- helpers ---

// waitForSidecar polls for the existence of the exit-code sidecar file.
// Tests that verify exit codes need to wait for tmux + bash to flush the
// sidecar; the wait is short because the mock scripts exit immediately.
func waitForSidecar(t *testing.T, logPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(logPath + ".exitcode"); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sidecar file %s.exitcode did not appear within %s", logPath, timeout)
}
