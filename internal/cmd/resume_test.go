package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResumeCmd_RequiresBranch mirrors the run-side test — the persistent
// flag is shared, but having a dedicated test means flag scope regressions
// can't sneak in (e.g., someone moving --branch to run-only by accident).
func TestResumeCmd_RequiresBranch(t *testing.T) {
	err, stderr := runCobra(t, "resume")
	require.Error(t, err)
	assert.Contains(t, stderr+err.Error(), "branch")
}

// TestResumeCmd_RejectsPositional pins the run-vs-resume contract: a user
// who types `wonka resume --branch X work-packages/y` (the run-flow muscle
// memory) must get a clear hint pointing at `wonka run`, not a cobra default
// "unknown command" message that loses the work-package value.
func TestResumeCmd_RejectsPositional(t *testing.T) {
	err, stderr := runCobra(t,
		"resume",
		"--branch", "feat/x",
		"work-packages/demo",
	)
	require.Error(t, err)
	combined := stderr + err.Error()
	assert.Contains(t, combined, "wonka run", "must point at the right verb")
	assert.Contains(t, combined, "work-packages/demo", "must echo the user's argument so they recognize it")
}

// TestResumeCmd_NoLedger drives through real orch.Engine.Resume with no
// prior state. orch/resume_errorpath_spec_test.go:170-187 pins the
// ErrResumeNoLedger wrap — this test pins the *CLI-facing* error wording
// and exit code so operators see a helpful message, not just the sentinel.
//
// Uses --ledger fs so the test is tier 1 (no dolt dependency).
func TestResumeCmd_NoLedger(t *testing.T) {
	repo := seedRepoWithAgents(t)
	runDir := filepath.Join(t.TempDir(), "no-ledger")

	err, stderr := runCobra(t,
		"resume",
		"--branch", "never-existed",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--run-dir", runDir,
		"--ledger", "fs",
	)
	require.Error(t, err)

	// The CLI die() message must name the fix action so operators don't
	// hunt through logs. classifyEngineError in run.go owns this string.
	assert.Contains(t, stderr, "wonka run")
	requireExitCode(t, err, exitConfigError)
}
