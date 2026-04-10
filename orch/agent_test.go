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
func TestBVV_DSP04_BuildCommandAssembly(t *testing.T) {
	preset := &orch.Preset{
		Name:             "claude",
		Command:          "claude",
		Args:             []string{"-p", "--verbose"},
		SystemPromptFlag: "--append-system-prompt",
		ModelFlag:        "--model",
	}

	cmd := orch.BuildCommand(preset, "/path/to/builder.md", "opus", 20)

	assert.Equal(t, []string{
		"claude",
		"-p", "--verbose",
		"--append-system-prompt", "/path/to/builder.md",
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

	cmd := orch.BuildCommand(preset, "/path/to/role.md", "", 0)

	assert.Equal(t, []string{
		"claude",
		"--append-system-prompt", "/path/to/role.md",
	}, cmd)
	for _, arg := range cmd {
		assert.NotEqual(t, "--model", arg, "ModelFlag empty → no --model")
		assert.NotEqual(t, "--max-turns", arg, "MaxTurns=0 → no --max-turns")
	}
}

// TestBVV_DSP04_BuildCommandOmitsSystemPromptWhenPathEmpty verifies that a
// role with no instruction file falls through to preset defaults.
func TestBVV_DSP04_BuildCommandOmitsSystemPromptWhenPathEmpty(t *testing.T) {
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

// TestReadAgentPrompt_MissingFile verifies that a missing instruction file
// returns empty body+model with nil error. This lets generic presets run
// without a role file (their own system prompt takes over).
func TestReadAgentPrompt_MissingFile(t *testing.T) {
	body, model, err := orch.ReadAgentPrompt("/nonexistent/path/file.md")
	require.NoError(t, err)
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
