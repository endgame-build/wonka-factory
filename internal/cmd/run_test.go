package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireExitCode asserts err's chain contains *exitError with code want.
func requireExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var ex *exitError
	require.True(t, errors.As(err, &ex), "expected *exitError in chain, got %T: %v", err, err)
	assert.Equal(t, want, ex.code)
}

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
	requireExitCode(t, err, exitConfigError)
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

// TestBuildEngineConfig_ValidateGraphDefault verifies BVV-TG-07..10 validation
// is ON by default — Level 2 conformance requires it.
func TestBuildEngineConfig_ValidateGraphDefault(t *testing.T) {
	repo := seedRepoWithAgents(t)
	flags := CLIFlags{
		Branch: "feat/x", Ledger: "fs", RepoPath: repo,
		AgentDir: filepath.Join(repo, "agents"), AgentPreset: defaultAgentPreset,
		Workers: defaultWorkers, GapTolerance: defaultGapTolerance,
		MaxRetries: defaultMaxRetries, MaxHandoffs: defaultMaxHandoffs,
		BaseTimeout: defaultBaseTimeout,
		// NoValidateGraph left at zero value (false) — default-on path.
	}
	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.True(t, cfg.Lifecycle.ValidateGraph, "default must enable graph validation (Level 2)")
}

// TestBuildEngineConfig_NoValidateGraph verifies --no-validate-graph plumbs
// through as ValidateGraph=false (Level 1 compatibility escape hatch).
func TestBuildEngineConfig_NoValidateGraph(t *testing.T) {
	repo := seedRepoWithAgents(t)
	flags := CLIFlags{
		Branch: "feat/x", Ledger: "fs", RepoPath: repo,
		AgentDir: filepath.Join(repo, "agents"), AgentPreset: defaultAgentPreset,
		Workers: defaultWorkers, GapTolerance: defaultGapTolerance,
		MaxRetries: defaultMaxRetries, MaxHandoffs: defaultMaxHandoffs,
		BaseTimeout:     defaultBaseTimeout,
		NoValidateGraph: true,
	}
	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.False(t, cfg.Lifecycle.ValidateGraph, "--no-validate-graph must disable validation")
}

// TestRunCmd_NoValidateGraphFlag exercises the cobra path end-to-end by
// parsing --no-validate-graph through a real root command. Exits with a
// non-zero code because we don't actually run the engine, but flag parsing
// must succeed (no "unknown flag" error).
func TestRunCmd_NoValidateGraphFlag(t *testing.T) {
	repo := seedRepoWithAgents(t)
	// Use an unrecognized ledger to short-circuit before engine init; we only
	// care that --no-validate-graph parses cleanly.
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--no-validate-graph",
		"--ledger", "dolt", // triggers exitConfigError — flag parsing happened first
	)
	require.Error(t, err)
	assert.NotContains(t, stderr, "unknown flag", "--no-validate-graph must parse")
}
