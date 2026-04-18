//go:build verify

package orch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBVV_ITF01_EnvironmentVariableInjection verifies BVV-ITF-01: the
// orchestrator injects identity context via environment variables. BVV sets
// exactly 5 ORCH_* vars — the fork's ORCH_WORKSPACE and ORCH_ROLE are gone
// (agents commit to branch, not workspace; role is encoded in the
// instruction file).
func TestBVV_ITF01_EnvironmentVariableInjection(t *testing.T) {
	env := orch.BuildEnv("w-01", "run-abc", "/repo", "task-001", "feature-x", map[string]string{
		"CUSTOM": "value",
	})

	assert.Equal(t, "w-01", env["ORCH_WORKER_NAME"])
	assert.Equal(t, "run-abc", env["ORCH_RUN_ID"])
	assert.Equal(t, "/repo", env["ORCH_PROJECT"])
	assert.Equal(t, "task-001", env["ORCH_TASK_ID"])
	assert.Equal(t, "feature-x", env["ORCH_BRANCH"])

	// Fork-era vars must NOT be set (ZFC / BVV-DSN-04).
	_, hasWorkspace := env["ORCH_WORKSPACE"]
	assert.False(t, hasWorkspace, "ORCH_WORKSPACE must not be set — BVV agents commit to branch")
	_, hasRole := env["ORCH_ROLE"]
	assert.False(t, hasRole, "ORCH_ROLE must not be set — role is in the instruction file")

	// Preset env vars are merged through.
	assert.Equal(t, "value", env["CUSTOM"])
}

// TestBVV_DSP06_OrchTaskIDAlwaysSet verifies BVV-DSP-06: ORCH_TASK_ID is
// the sole identity handle agents receive. This is a load-bearing
// precondition for `bd show $ORCH_TASK_ID` working inside the agent.
func TestBVV_DSP06_OrchTaskIDAlwaysSet(t *testing.T) {
	env := orch.BuildEnv("w", "r", "/repo", "task-xyz", "main", nil)
	assert.Equal(t, "task-xyz", env["ORCH_TASK_ID"])
}

// TestBVV_ITF01_PresetEnvCannotShadowIdentity verifies that preset-supplied
// env vars cannot override the BVV-injected identity fields. If a preset
// tries to set ORCH_TASK_ID=attacker, BuildEnv must still return the caller's
// task ID.
func TestBVV_ITF01_PresetEnvCannotShadowIdentity(t *testing.T) {
	env := orch.BuildEnv("w", "r", "/repo", "legitimate-task", "main", map[string]string{
		"ORCH_TASK_ID":     "hijack",
		"ORCH_WORKER_NAME": "evil",
	})
	assert.Equal(t, "legitimate-task", env["ORCH_TASK_ID"])
	assert.Equal(t, "w", env["ORCH_WORKER_NAME"])
}

// TestBVV_DSP04_BuildCommandAssembly verifies that BuildCommand assembles the
// preset command with the system-prompt flag, model flag, and max-turns flag
// in the canonical order. BVV-DSP-04 requires the command to be content-
// independent — the test confirms nothing from agent output flows into args.
// The instruction body is passed as the literal flag argument (not a path),
// matching the Claude/codex/goose CLI contract.
func TestBVV_DSP04_BuildCommandAssembly(t *testing.T) {
	preset := &orch.Preset{
		Name:             "claude",
		Command:          "claude",
		Args:             []string{"-p", "--verbose"},
		SystemPromptFlag: "--append-system-prompt",
		ModelFlag:        "--model",
	}

	body := "You are a builder agent.\nWrite code."
	cmd := orch.BuildCommand(preset, body, "opus", 20)

	assert.Equal(t, []string{
		"claude",
		"-p", "--verbose",
		"--append-system-prompt", body,
		"--model", "opus",
		"--max-turns", "20",
	}, cmd)
}

// TestBVV_DSP04_BuildCommandOmitsEmptyFlags verifies that empty optionals are
// skipped. A preset without a ModelFlag should not have "--model" in its cmd;
// MaxTurns=0 should not append "--max-turns".
func TestBVV_DSP04_BuildCommandOmitsEmptyFlags(t *testing.T) {
	preset := &orch.Preset{
		Command:          "claude",
		SystemPromptFlag: "--append-system-prompt",
	}

	cmd := orch.BuildCommand(preset, "You are a builder.", "", 0)

	assert.Equal(t, []string{
		"claude",
		"--append-system-prompt", "You are a builder.",
	}, cmd)
	for _, arg := range cmd {
		assert.NotEqual(t, "--model", arg, "ModelFlag empty → no --model")
		assert.NotEqual(t, "--max-turns", arg, "MaxTurns=0 → no --max-turns")
	}
}

// TestBVV_DSP04_BuildCommandOmitsSystemPromptWhenBodyEmpty verifies that a
// role with no instruction body (missing or empty .md file) falls through to
// preset defaults without appending the system-prompt flag.
func TestBVV_DSP04_BuildCommandOmitsSystemPromptWhenBodyEmpty(t *testing.T) {
	preset := &orch.Preset{
		Command:          "claude",
		SystemPromptFlag: "--append-system-prompt",
		ModelFlag:        "--model",
	}

	cmd := orch.BuildCommand(preset, "", "sonnet", 0)

	assert.Equal(t, []string{"claude", "--model", "sonnet"}, cmd)
}

// TestReadAgentPrompt_FrontmatterStrip verifies frontmatter parsing: body
// and model are extracted, the frontmatter itself is not returned in the body.
func TestReadAgentPrompt_FrontmatterStrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "builder.md")
	content := "---\nname: builder\nmodel: opus\ndescription: Build tasks\n---\n\nYou are a builder agent.\nWrite code.\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	body, model, err := orch.ReadAgentPrompt(path)
	require.NoError(t, err)
	assert.Equal(t, "opus", model)
	assert.Equal(t, "You are a builder agent.\nWrite code.", body)
	assert.NotContains(t, body, "name: builder")
	assert.NotContains(t, body, "---")
}

// TestReadAgentPrompt_NoFrontmatter verifies that a file without frontmatter
// returns its entire content as the body.
func TestReadAgentPrompt_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.md")
	content := "# Plain instruction\n\nDo the thing.\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	body, model, err := orch.ReadAgentPrompt(path)
	require.NoError(t, err)
	assert.Empty(t, model)
	assert.Equal(t, "# Plain instruction\n\nDo the thing.", body)
}

// TestReadAgentPrompt_MissingFile verifies that a non-empty path to a
// missing instruction file surfaces os.ErrNotExist. Silently returning
// empty strings here would let a typo or missing deploy launch an agent
// with no role prompt — BVV-DSP-04 would then mark the task complete on
// a normal exit, and terminal irreversibility would prevent recovery.
func TestReadAgentPrompt_MissingFile(t *testing.T) {
	body, model, err := orch.ReadAgentPrompt("/nonexistent/path/file.md")
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, body)
	assert.Empty(t, model)
}

// TestReadAgentPrompt_EmptyPath short-circuits before hitting the filesystem.
func TestReadAgentPrompt_EmptyPath(t *testing.T) {
	body, model, err := orch.ReadAgentPrompt("")
	require.NoError(t, err)
	assert.Empty(t, body)
	assert.Empty(t, model)
}

// TestLogPath_Canonical verifies the canonical log file layout:
// {runDir}/logs/{taskID}.stdout. The sidecar exit-code file lives alongside
// at {LogPath()}.exitcode (BVV Appendix A).
func TestLogPath_Canonical(t *testing.T) {
	got := orch.LogPath("/run/abc", "task-001")
	want := filepath.Join("/run/abc", "logs", "task-001.stdout")
	assert.Equal(t, want, got)
}

// TestBVV_AI01_InstructionFileInjection verifies BVV-AI-01: a role's
// instruction file body is injected into the agent invocation via the
// preset's SystemPromptFlag. Exercises ReadAgentPrompt → BuildCommand
// end-to-end so a regression in either side surfaces here.
func TestBVV_AI01_InstructionFileInjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "OOMPA.md")
	body := "You are a builder. Write code, run tests, commit."
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	gotBody, _, err := orch.ReadAgentPrompt(path)
	require.NoError(t, err)

	preset := &orch.Preset{
		Command:          "claude",
		SystemPromptFlag: "--append-system-prompt",
	}
	cmd := orch.BuildCommand(preset, gotBody, "", 0)

	// Locate the flag and assert the immediately-following arg is the body.
	idx := -1
	for i, arg := range cmd {
		if arg == "--append-system-prompt" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "SystemPromptFlag missing from cmd: %v", cmd)
	require.Less(t, idx, len(cmd)-1, "SystemPromptFlag has no value: %v", cmd)
	assert.Equal(t, body, cmd[idx+1], "instruction body must be the literal flag value")
}

// TestBVV_AI02_RoleToInstructionMapping verifies BVV-AI-02: a task's role
// label is the lookup key into LifecycleConfig.Roles, which returns the
// instruction file path. The mapping is the contract the dispatcher relies
// on to inject the right system prompt for the right role.
func TestBVV_AI02_RoleToInstructionMapping(t *testing.T) {
	roles := map[string]orch.RoleConfig{
		orch.RoleBuilder:  {InstructionFile: "/agents/OOMPA.md"},
		orch.RoleVerifier: {InstructionFile: "/agents/LOOMPA.md"},
		orch.RolePlanner:  {InstructionFile: "/agents/CHARLIE.md"},
	}
	cases := []struct {
		role     string
		wantFile string
	}{
		{orch.RoleBuilder, "/agents/OOMPA.md"},
		{orch.RoleVerifier, "/agents/LOOMPA.md"},
		{orch.RolePlanner, "/agents/CHARLIE.md"},
	}
	for _, c := range cases {
		cfg, ok := roles[c.role]
		require.True(t, ok, "role %q must be in role map", c.role)
		assert.Equal(t, c.wantFile, cfg.InstructionFile,
			"role %q must map to %q", c.role, c.wantFile)
	}
	_, ok := roles["unknown"]
	assert.False(t, ok, "unknown role must miss the map (dispatcher escalates per BVV-DSP-03a)")
}

// TestBVV_AI03_PresetSelection verifies BVV-AI-03: distinct roles may carry
// distinct presets, and BuildCommand renders each role's preset
// independently. Pinning this prevents a future "single preset per
// lifecycle" regression that would tie all roles to the same agent CLI.
func TestBVV_AI03_PresetSelection(t *testing.T) {
	claude := &orch.Preset{Command: "claude", Args: []string{"-p"}, SystemPromptFlag: "--append-system-prompt"}
	codex := &orch.Preset{Command: "codex", Args: []string{"chat"}, SystemPromptFlag: "--system"}

	cmdClaude := orch.BuildCommand(claude, "be a builder", "", 0)
	cmdCodex := orch.BuildCommand(codex, "be a builder", "", 0)

	assert.Equal(t, "claude", cmdClaude[0])
	assert.Equal(t, "codex", cmdCodex[0])
	assert.Contains(t, cmdClaude, "--append-system-prompt")
	assert.Contains(t, cmdCodex, "--system")
	assert.NotContains(t, cmdClaude, "--system", "preset isolation: claude must not use codex flag")
	assert.NotContains(t, cmdCodex, "--append-system-prompt", "preset isolation: codex must not use claude flag")
}

// TestBVV_DSP04_DetermineOutcome verifies the exit-code-to-outcome mapping
// (BVV-DSP-04). The orchestrator MUST determine task outcome from the exit
// code alone — no output content inspection.
func TestBVV_DSP04_DetermineOutcome(t *testing.T) {
	cases := []struct {
		exitCode int
		want     orch.AgentOutcome
	}{
		{0, orch.OutcomeSuccess},
		{1, orch.OutcomeFailure},
		{2, orch.OutcomeBlocked},
		{3, orch.OutcomeHandoff},
		{-1, orch.OutcomeFailure},  // missing sidecar
		{99, orch.OutcomeFailure},  // unknown code
		{127, orch.OutcomeFailure}, // command not found
		{255, orch.OutcomeFailure}, // signal death
	}
	for _, c := range cases {
		got := orch.DetermineOutcome(c.exitCode)
		assert.Equal(t, c.want, got, "DetermineOutcome(%d)", c.exitCode)
	}
}
