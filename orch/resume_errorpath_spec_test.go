//go:build verify

package orch_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Store error-propagation tests ---
//
// These tests pin the error-surface behavior of each Reconcile step that
// touches the Store. A regression that swallowed any of these errors would
// ship silent state corruption — the dispatcher would run against a store
// that does not reflect the reconciliation outcome.

// TestReconcile_ListTasksErrorPropagates pins the entry error gate of the
// algorithm (resume.go:71). If this returned nil on a store error, steps 2-7
// would still run against empty tasks — but the real in_progress tasks would
// never be reconciled, leaving them with dead sessions and no re-queue path.
func TestReconcile_ListTasksErrorPropagates(t *testing.T) {
	store := testutil.NewMockStore()
	store.SetListTasksErr(errProbe("store: connection refused"))
	tmux := newMockSession("run-1")

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list tasks")
}

// TestReconcile_UpdateTaskErrorPropagates verifies step 1 surfaces a store
// write failure rather than silently under-reporting Reconciled.
func TestReconcile_UpdateTaskErrorPropagates(t *testing.T) {
	store := testutil.NewMockStore()
	task := testutil.SingleTask(t, store, "build-1", "feat/x", "builder")
	task.Status = orch.StatusInProgress
	task.Assignee = "w1"
	require.NoError(t, store.UpdateTask(task))
	// Block subsequent UpdateTask calls (the stale-reset write is what we want to fail).
	store.SetUpdateTaskErr(errProbe("store: disk full"))

	tmux := newMockSession("run-1") // session dead → stale

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reset task build-1")
	assert.Nil(t, result, "partial reconcile must not leak a partial result to callers")
}

// TestReconcile_ListWorkersErrorPropagates covers resume.go:171 (step 7).
// Tests don't touch other steps — a separate row in ListTasks would also
// cover this implicitly, but the error message differs and callers that
// match on wrapped messages would be confused without a dedicated test.
func TestReconcile_ListWorkersErrorPropagates(t *testing.T) {
	store := testutil.NewMockStore()
	store.SetListWorkersErr(errProbe("store: disconnected"))
	tmux := newMockSession("run-1")

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list workers")
}

// TestReconcile_UpdateWorkerErrorPropagates pins step 7's write error
// surface. A regression that swallowed this would leave workers pointing at
// dead sessions, which would then collide with the watchdog's assumptions.
func TestReconcile_UpdateWorkerErrorPropagates(t *testing.T) {
	store := testutil.NewMockStore()
	require.NoError(t, store.CreateWorker(&orch.Worker{
		Name: "w1", Status: orch.WorkerActive, CurrentTaskID: "dead-task",
	}))
	store.SetUpdateWorkerErr(errProbe("store: lock timeout"))

	tmux := newMockSession("run-1")

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reset worker w1")
}

// --- Human-reopen GetTask error branch (step 6) ---
//
// resume.go:154-161 distinguishes ErrNotFound (deleted task → skip, not a
// re-open) from other GetTask errors (surface). Before this test, the
// branch was untested: a regression inverting the condition would either
// (a) treat every deleted task as a re-open — wildly inflating HumanReopens
// and triggering spurious counter resets, or (b) swallow real errors —
// masking store corruption.

// TestReconcile_ReopenLoopSkipsDeletedTask verifies that a task whose
// terminal history is in the log but which has been deleted from the store
// is treated as "deleted by operator" and NOT as a human re-open.
func TestReconcile_ReopenLoopSkipsDeletedTask(t *testing.T) {
	store := testutil.NewMockStore()
	// Create and immediately delete would require a Delete API the Store
	// doesn't expose — simulate via GetTask error injection. Use a fresh
	// mock and set GetTaskErr to ErrNotFound so the re-open loop's
	// errors.Is(err, ErrNotFound) branch fires.
	store.SetGetTaskErr(orch.ErrNotFound)
	tmux := newMockSession("run-1")

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskCompleted, TaskID: "deleted-1"},
	})

	result, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.NoError(t, err, "ErrNotFound in reopen loop must not propagate")
	assert.Empty(t, result.HumanReopens, "deleted task is not a re-open")
}

// TestReconcile_ReopenLoopSurfacesNonNotFoundError verifies that any
// GetTask error OTHER than ErrNotFound aborts reconciliation. A regression
// that broadened the ErrNotFound branch (e.g. `if err != nil { continue }`)
// would silently drop every HumanReopen under a store outage, defeating
// BVV-S-02a.
func TestReconcile_ReopenLoopSurfacesNonNotFoundError(t *testing.T) {
	store := testutil.NewMockStore()
	store.SetGetTaskErr(errProbe("store: I/O error"))
	tmux := newMockSession("run-1")

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, logPath, []orch.Event{
		{Kind: orch.EventTaskCompleted, TaskID: "some-task"},
	})

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", logPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get task some-task")
}

// --- ListSessions error surface (I2 fix) ---
//
// Previously swallowed with misleading "tmux server may be dead" comment.
// TmuxClient.ListSessions already handles no-server internally, so the only
// remaining errors are genuine exec failures that invalidate orphan cleanup.

// TestReconcile_ListSessionsErrorIsFatal pins the post-I2 behavior: a genuine
// tmux exec failure aborts Reconcile. A regression that re-introduced the
// silent `sessions = nil` fallback would let the dispatcher run with
// stale sessions accumulating on the socket.
func TestReconcile_ListSessionsErrorIsFatal(t *testing.T) {
	store := testutil.NewMockStore()
	tmux := &listErrSession{err: errProbe("tmux: exec: 'tmux': executable file not found")}

	_, err := orch.Reconcile(store, tmux, "run-1", "feat/x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list sessions")
}

// listErrSession is a SessionPresence that returns errors from ListSessions.
type listErrSession struct {
	err error
}

func (l *listErrSession) HasSession(string) (bool, error)  { return false, nil }
func (l *listErrSession) ListSessions() ([]string, error)  { return nil, l.err }
func (l *listErrSession) KillSessionIfExists(string) error { return nil }

// --- Engine-level resume error paths ---

// TestEngine_ResumeNoEventLogReturnsSentinel pins the ErrResumeNoEventLog wrap.
// Callers (Phase 7 CLI) branch on errors.Is(err, ErrResumeNoEventLog) to
// decide whether to offer "fresh start" — a regression that dropped the
// %w wrap would break that CLI surface invisibly.
//
// Sentinel switched from ErrResumeNoLedger to ErrResumeNoEventLog because
// --ledger beads now shares <repo>/.beads/ across branches; a ledger-stat
// would falsely succeed on any bd-installed repo. The event log is wonka-owned
// and per-RunDir, so its absence is the canonical "no prior wonka run" signal.
func TestEngine_ResumeNoEventLogReturnsSentinel(t *testing.T) {
	runDir := t.TempDir()
	// No event log on purpose.

	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat-x", "builder"),
		runDir, "/repo")
	cfg.RunID = "run-1"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrResumeNoEventLog)
}

// TestEngine_ResumeEmptyEventLogTreatedAsMissing covers the "init created
// the file via O_CREATE but crashed before the first emit" race. A zero-byte
// events.jsonl must surface as ErrResumeNoEventLog so the operator gets the
// "use `wonka run`" hint, not the corrupt sentinel.
func TestEngine_ResumeEmptyEventLogTreatedAsMissing(t *testing.T) {
	runDir := t.TempDir()
	logPath := filepath.Join(runDir, "events.jsonl")
	require.NoError(t, os.WriteFile(logPath, []byte{}, 0o644))

	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat-x", "builder"),
		runDir, "/repo")
	cfg.RunID = "run-1"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrResumeNoEventLog)
}

// TestEngine_ResumeCorruptEventLogReturnsCorruptSentinel pins the
// ErrCorruptEventLog wrap. A first record that fails JSON parse means a
// prior run started and crashed mid-write — operator-intervention territory.
// Recovery would otherwise replay an undefined event stream.
func TestEngine_ResumeCorruptEventLogReturnsCorruptSentinel(t *testing.T) {
	runDir := t.TempDir()
	logPath := filepath.Join(runDir, "events.jsonl")
	require.NoError(t, os.WriteFile(logPath, []byte("{not valid json\n"), 0o644))

	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat-x", "builder"),
		runDir, "/repo")
	cfg.RunID = "run-1"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrCorruptEventLog)
	assert.NotErrorIs(t, err, orch.ErrResumeNoEventLog,
		"corrupt-log must not be squashed into the missing sentinel — different recovery action")
}

// `{}` parses cleanly but has no Kind — must surface as ErrCorruptEventLog,
// not pass the sentinel check.
func TestEngine_ResumeEmptyObjectFirstRecordCorrupt(t *testing.T) {
	runDir := t.TempDir()
	logPath := filepath.Join(runDir, "events.jsonl")
	require.NoError(t, os.WriteFile(logPath, []byte("{}\n"), 0o644))

	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat-x", "builder"),
		runDir, "/repo")
	cfg.RunID = "run-1"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrCorruptEventLog)
	assert.Contains(t, err.Error(), "no event kind")
}

// First record exceeding maxEventLogLine (16 MiB) must surface as
// ErrCorruptEventLog, not the generic runtime-error path.
func TestEngine_ResumeOversizeFirstRecordCorrupt(t *testing.T) {
	if testing.Short() {
		t.Skip("writes >16 MiB; skipped under -short")
	}
	runDir := t.TempDir()
	logPath := filepath.Join(runDir, "events.jsonl")
	require.NoError(t, os.WriteFile(logPath, bytes.Repeat([]byte("x"), 17*1024*1024), 0o644))

	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat-x", "builder"),
		runDir, "/repo")
	cfg.RunID = "run-1"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrCorruptEventLog)
	assert.NotErrorIs(t, err, orch.ErrResumeNoEventLog)
}

// TestEngine_ResumeNonNotExistEventLogStatErrorDoesNotMapToSentinel covers I4
// adapted for the event-log sentinel. A permission-denied stat must NOT be
// reported as ErrResumeNoEventLog, because a caller taking the "fresh start"
// branch on that sentinel would clobber the inaccessible run state.
func TestEngine_ResumeNonNotExistEventLogStatErrorDoesNotMapToSentinel(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission bits")
	}

	parent := t.TempDir()
	sealed := filepath.Join(parent, "sealed")
	require.NoError(t, os.Mkdir(sealed, 0o755))
	runDir := filepath.Join(sealed, "run")
	require.NoError(t, os.Mkdir(runDir, 0o755))
	// Pre-create the event log so it exists, then seal the outer dir so
	// Stat fails with EACCES rather than ENOENT.
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "events.jsonl"), []byte(`{"kind":"lifecycle_started","summary":"x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644))
	require.NoError(t, os.Chmod(sealed, 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(sealed, 0o755) // restore so TempDir cleanup works
	})

	cfg := orch.DefaultEngineConfig(
		testutil.MockLifecycleConfig("feat-x", "builder"),
		runDir, "/repo")
	cfg.RunID = "run-1"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, orch.ErrResumeNoEventLog,
		"EACCES must not be squashed into ErrResumeNoEventLog — would trigger silent run-dir clobber")
	assert.Contains(t, err.Error(), "stat event log")
}

// TestEngine_ResumeCorruptLockAborts verifies C3: a lock file that cannot be
// parsed aborts Resume with ErrCorruptLock rather than silently fabricating
// a fresh RunID and orphaning the surviving tmux socket (BVV-ERR-08 hazard).
func TestEngine_ResumeCorruptLockAborts(t *testing.T) {
	runDir := t.TempDir()
	branch := "feat-x"

	// Pre-create event log so initForResume passes the existence check.
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "events.jsonl"),
		[]byte(`{"kind":"lifecycle_started","summary":"prior","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644))

	// Write a corrupt lock file.
	lockPath := filepath.Join(runDir, ".wonka-"+branch+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte("{truncated json"), 0o644))

	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.Path = lockPath

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, "/repo")
	cfg.RunID = "caller-provided"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrCorruptLock)
	// Abort happens before any RunID overwrite or tmux open, so the
	// operator's configured RunID stays intact.
	assert.Equal(t, "caller-provided", e.RunID())
}
