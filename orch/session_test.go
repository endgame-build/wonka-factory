//go:build verify

package orch_test

import (
	"errors"
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

// TestBVV_DSP04_SpawnSessionExitCodeTable verifies the full BVV exit-code
// protocol (BVV-DSP-04): exit 0 → success, exit 1 → retryable failure, exit
// 2 → blocked (terminal), exit 3 → handoff. SpawnSession itself doesn't
// interpret the exit codes — Phase 4 will map them to outcomes — but the
// sidecar-capture path must preserve all four values faithfully.
//
// This is the integration smoke the Phase 3 plan's verification section
// referenced (exit 2 specifically). Running under -tags verify (no separate
// integration tag) because the mechanism under test is the infrastructure
// primitive, not a Phase 4 dispatch decision.
func TestBVV_DSP04_SpawnSessionExitCodeTable(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   int
	}{
		{"exit_0_success", "ok.sh", 0},
		{"exit_1_failure", "fail.sh", 1},
		{"exit_2_blocked", "blocked.sh", 2},
		{"exit_3_handoff", "handoff.sh", 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, store, outDir := newTestSessionPool(t)
			taskID := "task-" + tc.name
			task, _ := createAssignedTask(t, store, taskID, "w-01")

			roleCfg := mockRoleConfig(t, tc.script)
			require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

			logPath := orch.LogPath(outDir, taskID)
			waitForSidecar(t, logPath, 3*time.Second)

			code, err := orch.ReadExitCode(logPath)
			require.NoError(t, err)
			assert.Equal(t, tc.want, code, "sidecar must preserve exit code %d from %s", tc.want, tc.script)
		})
	}
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

// TestBVV_S10_SpawnSessionTerminalRace verifies the BVV-S-10 dynamic
// backstop in SpawnSession: AssertTerminalIrreversibility panics if the
// task's status is terminal at the moment of the StatusInProgress write.
//
// Scenario modelled: the watchdog reads task.Status as in_progress at
// watchdog.go:280, then the dispatcher transitions the task to completed
// before the watchdog's pool.RestartSession → pool.SpawnSession reaches
// step 8. Without the backstop, SpawnSession would silently reverse the
// terminal status with StatusInProgress (BVV-S-02 violation). With the
// backstop, the panic surfaces the race instead. This is the production
// enforcement that replaced the racy entry/exit snapshot in
// Watchdog.CheckOnce — see invariant.go AssertWatchdogNoStatusChange
// godoc for context.
func TestBVV_S10_SpawnSessionTerminalRace(t *testing.T) {
	pool, store, _ := newTestSessionPool(t)

	// Plant a worker and a task whose store status is already terminal
	// (simulating the race outcome where the dispatcher won).
	require.NoError(t, store.CreateWorker(&orch.Worker{Name: "w-01", Status: orch.WorkerIdle}))
	task := &orch.Task{
		ID:     "task-terminal-race",
		Status: orch.StatusCompleted,
		Labels: map[string]string{
			orch.LabelRole:   "builder",
			orch.LabelBranch: "feature-x",
		},
	}
	require.NoError(t, store.CreateTask(task))

	roleCfg := mockRoleConfig(t, "ok.sh")
	assert.PanicsWithValue(t,
		"[BVV-S-02] terminal irreversibility violated: completed → in_progress",
		func() { _ = pool.SpawnSession("w-01", task, roleCfg, "feature-x") },
		"SpawnSession must panic before reversing a terminal status with StatusInProgress",
	)
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

var errInjectedUpdateTask = errors.New("injected UpdateTask failure")

// TestSpawnSessionRevertsWorkerOnTaskUpdateFailure drives a step-8 failure
// (UpdateTask after UpdateWorker landed) and asserts the worker snapshot
// is restored. The load-bearing field is SessionStartedAt: Assign already
// set Status=Active and CurrentTaskID, so only SessionStartedAt is
// spawn-specific and must not persist if the task update fails.
func TestSpawnSessionRevertsWorkerOnTaskUpdateFailure(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	inner, err := orch.NewFSStore(filepath.Join(dir, "ledger"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = inner.Close() })

	store := &recordingStore{inner: inner, t: t}

	runID := "test-revert"
	tmuxClient := newTestTmux(t, runID)
	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, "/repo", outDir)

	task, _ := createAssignedTask(t, store, "task-revert", "w-01")

	preWorker, err := store.GetWorker("w-01")
	require.NoError(t, err)
	require.Equal(t, orch.WorkerActive, preWorker.Status)
	require.Equal(t, "task-revert", preWorker.CurrentTaskID)
	require.Zero(t, preWorker.SessionStartedAt)

	store.mu.Lock()
	store.failUpdateTask = errInjectedUpdateTask
	store.mu.Unlock()

	err = pool.SpawnSession("w-01", task, mockRoleConfig(t, "ok.sh"), "feature-x")
	require.Error(t, err)
	assert.ErrorIs(t, err, errInjectedUpdateTask)

	got, err := store.GetWorker("w-01")
	require.NoError(t, err)
	assert.Equal(t, orch.WorkerActive, got.Status)
	assert.Equal(t, "task-revert", got.CurrentTaskID)
	assert.Zero(t, got.SessionStartedAt, "SessionStartedAt must be reverted")
}

// fileFormMockRoleConfig returns a RoleConfig whose preset declares
// SystemPromptIsFile so SpawnSession exercises the sidecar-write path. The
// mock script ignores CLI arguments, so the bogus flag value is safe.
func fileFormMockRoleConfig(t *testing.T, scriptName string) orch.RoleConfig {
	t.Helper()
	cfg := mockRoleConfig(t, scriptName)
	cfg.Preset.SystemPromptFlag = "--append-system-prompt-file"
	cfg.Preset.SystemPromptIsFile = true
	return cfg
}

// TestSpawnSession_FileFormWritesPromptSidecar asserts SpawnSession writes
// the role instruction body to PromptPath() with mode 0o600 when the preset
// declares file-form. Regression guard for the macOS argv overflow fix —
// without this test, a refactor that drops the sidecar write silently
// re-introduces the original "command too long" failure (Linux CI does not
// catch it because Linux tmux's buffer is much larger).
func TestSpawnSession_FileFormWritesPromptSidecar(t *testing.T) {
	pool, store, outDir := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-fileform", "w-01")

	roleCfg := fileFormMockRoleConfig(t, "ok.sh")
	require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

	promptPath := orch.PromptPath(outDir, "task-fileform")
	info, err := os.Stat(promptPath)
	require.NoError(t, err, "sidecar file must exist for file-form preset")

	// Mode 0o600 — readable only by the invoking user. The orchestrator
	// otherwise never writes 0o644 under .wonka/, and a permissions
	// regression here changes the runtime-state security posture.
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"prompt sidecar must be 0o600; got %v", info.Mode().Perm())

	// Content matches the frontmatter-stripped body of the role instruction
	// file. ReadAgentPrompt is the canonical reader; if its return shape ever
	// drifts, this test is the one that surfaces the desync.
	wantBody, _, err := orch.ReadAgentPrompt(roleCfg.InstructionFile)
	require.NoError(t, err)
	got, err := os.ReadFile(promptPath)
	require.NoError(t, err)
	assert.Equal(t, wantBody, string(got),
		"sidecar contents must equal frontmatter-stripped instruction body")
}

// TestSpawnSession_BodyFormSkipsSidecar asserts SpawnSession does NOT write
// a sidecar when the preset declares body-form. Protects future presets
// (codex, goose) that pass the body inline — a refactor promoting file-form
// to the unconditional default would silently change their CLI surface.
func TestSpawnSession_BodyFormSkipsSidecar(t *testing.T) {
	pool, store, outDir := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-bodyform", "w-01")

	roleCfg := mockRoleConfig(t, "ok.sh")
	// Explicit body-form: a non-empty flag, IsFile=false.
	roleCfg.Preset.SystemPromptFlag = "--append-system-prompt"
	roleCfg.Preset.SystemPromptIsFile = false
	require.NoError(t, pool.SpawnSession("w-01", task, roleCfg, "feature-x"))

	_, err := os.Stat(orch.PromptPath(outDir, "task-bodyform"))
	assert.True(t, os.IsNotExist(err),
		"body-form preset must not write a sidecar; got stat err=%v", err)
}

// TestSpawnSession_RemovesSidecarOnEarlyFailure asserts the deferred rollback
// removes the prompt sidecar when SpawnSession fails BEFORE the tmux session
// is created — specifically, when step 5's BuildShellCommand rejects an
// invalid env key. Earlier revisions registered the rollback only after step
// 6 (CreateSession), which leaked the sidecar on every step-3-to-step-6
// failure path. This test pins the early-defer registration.
func TestSpawnSession_RemovesSidecarOnEarlyFailure(t *testing.T) {
	pool, store, outDir := newTestSessionPool(t)
	task, _ := createAssignedTask(t, store, "task-early-fail", "w-01")

	roleCfg := fileFormMockRoleConfig(t, "ok.sh")
	// Inject an invalid env key — BuildShellCommand (step 5) rejects keys
	// that do not match POSIX [A-Za-z_][A-Za-z0-9_]*. This forces a return
	// between step 2 (sidecar written) and step 6 (CreateSession).
	roleCfg.Preset.Env["BAD KEY"] = "value"

	err := pool.SpawnSession("w-01", task, roleCfg, "feature-x")
	require.Error(t, err, "BuildShellCommand must reject the invalid env key")

	_, statErr := os.Stat(orch.PromptPath(outDir, "task-early-fail"))
	assert.True(t, os.IsNotExist(statErr),
		"sidecar must be removed by rollback when step 5 fails before CreateSession; got stat err=%v", statErr)
}

// TestSpawnSession_RemovesSidecarOnUpdateTaskFailure asserts the deferred
// rollback removes the prompt sidecar when a step after CreateSession fails.
// Pairs with TestSpawnSessionRevertsWorkerOnTaskUpdateFailure — same fault
// injection (UpdateTask error), additional assertion that the sidecar
// written at step 2 is cleaned up rather than orphaned in <runDir>/logs/.
func TestSpawnSession_RemovesSidecarOnUpdateTaskFailure(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	inner, err := orch.NewFSStore(filepath.Join(dir, "ledger"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = inner.Close() })

	store := &recordingStore{inner: inner, t: t}

	runID := "test-sidecar-rollback"
	tmuxClient := newTestTmux(t, runID)
	pool := orch.NewWorkerPool(store, tmuxClient, 4, runID, "/repo", outDir)

	task, _ := createAssignedTask(t, store, "task-sidecar-rollback", "w-01")

	store.mu.Lock()
	store.failUpdateTask = errInjectedUpdateTask
	store.mu.Unlock()

	err = pool.SpawnSession("w-01", task, fileFormMockRoleConfig(t, "ok.sh"), "feature-x")
	require.Error(t, err)
	assert.ErrorIs(t, err, errInjectedUpdateTask)

	_, statErr := os.Stat(orch.PromptPath(outDir, "task-sidecar-rollback"))
	assert.True(t, os.IsNotExist(statErr),
		"sidecar must be removed by rollback when a post-CreateSession step fails; got stat err=%v", statErr)
}
