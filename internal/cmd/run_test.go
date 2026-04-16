package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedRepoWithAgents creates a temp "repo" containing an agents dir seeded
// with all three instruction files — lets cobra tests run the full flag
// validation without triggering a real engine.
func seedRepoWithAgents(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	agents := filepath.Join(repo, "agents")
	require.NoError(t, os.Mkdir(agents, 0o755))
	for _, name := range []string{"OOMPA.md", "LOOMPA.md", "CHARLIE.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(agents, name), []byte("# placeholder\n"), 0o644))
	}
	return repo
}

// runCobra is the standard harness: fresh root, captured streams, returns
// the error and stderr contents for assertions. Tests never share a root
// command — flag state would leak across parallel cases.
func runCobra(t *testing.T, args ...string) (error, string) {
	t.Helper()
	var stderr bytes.Buffer
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(&stderr)
	err := root.Execute()
	return err, stderr.String()
}

// TestRunCmd_RequiresBranch verifies cobra's MarkPersistentFlagRequired
// fires before any lifecycle side effects — no tmux, no lock, no store.
// The error must name the missing flag so operators know what to add.
func TestRunCmd_RequiresBranch(t *testing.T) {
	err, stderr := runCobra(t, "run")
	require.Error(t, err)
	assert.Contains(t, stderr+err.Error(), "branch")
}

// TestRunCmd_InvalidLedger exercises the unknown-ledger path through
// BuildEngineConfig. The test uses --repo to avoid leaking into the CI
// working directory (a default agent-dir stat against cwd would otherwise
// either succeed or fail unpredictably).
func TestRunCmd_InvalidLedger(t *testing.T) {
	repo := seedRepoWithAgents(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--ledger", "dolt",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "dolt")
	assert.Contains(t, stderr, "beads")
	ex, ok := err.(*exitError)
	require.True(t, ok, "expected *exitError, got %T", err)
	assert.Equal(t, exitConfigError, ex.code)
}

// TestRunCmd_InvalidWorkers proves the --workers >= 1 guard in
// BuildEngineConfig fires for explicit zero (cobra's IntVar happily accepts
// zero at parse time; the semantic check is ours).
func TestRunCmd_InvalidWorkers(t *testing.T) {
	repo := seedRepoWithAgents(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--workers", "0",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "workers")
}
