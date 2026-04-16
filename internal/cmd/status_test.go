package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedFSLedger creates an FS-backed ledger under runDir/ledger and inserts
// the provided tasks. Returns the FS store (already closed on test end) —
// used by status tests to pre-populate state without running the engine.
func seedFSLedger(t *testing.T, runDir string, tasks []*orch.Task) {
	t.Helper()
	ledgerDir := filepath.Join(runDir, "ledger")
	require.NoError(t, os.MkdirAll(ledgerDir, 0o755))

	store, _, err := orch.NewStore(orch.LedgerFS, ledgerDir)
	require.NoError(t, err)
	for _, task := range tasks {
		if task.CreatedAt.IsZero() {
			task.CreatedAt = time.Now()
			task.UpdatedAt = task.CreatedAt
		}
		require.NoError(t, store.CreateTask(task))
	}
	require.NoError(t, store.Close())
}

// runStatusCmd is a status-specific harness that captures both stdout and
// stderr, since status writes the header to stderr and the payload (table
// or JSON) to stdout.
func runStatusCmd(t *testing.T, args ...string) (error, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()
	return err, stdout.String(), stderr.String()
}

// TestStatusCmd_RequiresBranch pins the shared-root behavior for status.
func TestStatusCmd_RequiresBranch(t *testing.T) {
	err, _, stderr := runStatusCmd(t, "status")
	require.Error(t, err)
	assert.Contains(t, stderr+err.Error(), "branch")
}

// TestStatusCmd_RejectsLifecycleFlags proves --workers (and friends) are
// scoped to run/resume, not status. Without this isolation, a user running
// `wonka status --workers 4` would see a silent no-op, which is worse than
// an explicit unknown-flag error.
func TestStatusCmd_RejectsLifecycleFlags(t *testing.T) {
	err, _, stderr := runStatusCmd(t,
		"status",
		"--branch", "x",
		"--workers", "4",
	)
	require.Error(t, err)
	assert.Contains(t, stderr+err.Error(), "workers")
}

// TestStatusCmd_NoLedger verifies the "no ledger at <path>" fail-fast path.
// The die() message must name the missing directory so operators can fix
// their --run-dir / --branch spelling quickly.
func TestStatusCmd_NoLedger(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "nothing-here")
	err, _, stderr := runStatusCmd(t,
		"status",
		"--branch", "nowhere",
		"--run-dir", missingDir,
		"--ledger", "fs",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "no ledger")
	ex, ok := err.(*exitError)
	require.True(t, ok)
	assert.Equal(t, exitConfigError, ex.code)
}

// TestStatusCmd_EmptyLedger runs against a freshly created (empty) store
// and verifies the header renders and the body is the bare column labels.
// Exit 0 — empty is a valid state.
func TestStatusCmd_EmptyLedger(t *testing.T) {
	runDir := t.TempDir()
	seedFSLedger(t, runDir, nil)

	err, stdout, stderr := runStatusCmd(t,
		"status",
		"--branch", "feat-x",
		"--run-dir", runDir,
		"--ledger", "fs",
	)
	require.NoError(t, err)
	assert.Contains(t, stderr, "branch: feat-x")
	assert.Contains(t, stderr, "ledger: fs")
	assert.Contains(t, stdout, "STATUS")
	assert.Contains(t, stdout, "TITLE")
}

// TestStatusCmd_JSONOutput seeds two tasks and asserts the JSON output
// round-trips cleanly. Covers the --json code path and pins the schema
// contract (if orch.Task fields change, this test breaks — intentional).
func TestStatusCmd_JSONOutput(t *testing.T) {
	runDir := t.TempDir()
	seedFSLedger(t, runDir, []*orch.Task{
		{
			ID:     "issue-1",
			Title:  "build user model",
			Status: orch.StatusOpen,
			Labels: map[string]string{
				orch.LabelRole:   "builder",
				orch.LabelBranch: "feat-x",
			},
		},
		{
			ID:     "issue-2",
			Title:  "verify auth",
			Status: orch.StatusInProgress,
			Labels: map[string]string{
				orch.LabelRole:   "verifier",
				orch.LabelBranch: "feat-x",
			},
			Assignee: "worker-1",
		},
	})

	err, stdout, _ := runStatusCmd(t,
		"status",
		"--branch", "feat-x",
		"--run-dir", runDir,
		"--ledger", "fs",
		"--json",
	)
	require.NoError(t, err)

	// JSON output must be a stdout-only decodable array (stderr has the
	// human header, which would poison any downstream `jq` pipe if merged).
	var got []*orch.Task
	require.NoError(t, json.NewDecoder(strings.NewReader(stdout)).Decode(&got))
	require.Len(t, got, 2)

	byID := map[string]*orch.Task{got[0].ID: got[0], got[1].ID: got[1]}
	assert.Equal(t, "builder", byID["issue-1"].Role())
	assert.Equal(t, orch.StatusInProgress, byID["issue-2"].Status)
	assert.Equal(t, "worker-1", byID["issue-2"].Assignee)
}

// TestStatusCmd_BranchFilter verifies ListTasks(branch:<name>) excludes
// tasks for other branches. Otherwise a shared run-dir (unusual, but
// possible) would leak tasks across lifecycles.
func TestStatusCmd_BranchFilter(t *testing.T) {
	runDir := t.TempDir()
	seedFSLedger(t, runDir, []*orch.Task{
		{
			ID:     "mine",
			Title:  "in branch",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelBranch: "feat-x"},
		},
		{
			ID:     "theirs",
			Title:  "wrong branch",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelBranch: "feat-y"},
		},
	})

	err, stdout, _ := runStatusCmd(t,
		"status",
		"--branch", "feat-x",
		"--run-dir", runDir,
		"--ledger", "fs",
	)
	require.NoError(t, err)
	assert.Contains(t, stdout, "mine")
	assert.NotContains(t, stdout, "theirs")
}
