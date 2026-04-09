package orch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDSN01_NoReasoningInOrchestrator verifies [DSN-01, DSN-02]: command is content-free.
func TestDSN01_NoReasoningInOrchestrator(t *testing.T) {
	preset := &orch.Preset{
		Name:       "claude",
		Command:    "claude",
		Args:       []string{"--dangerously-skip-permissions"},
		PromptFlag: "-p",
		PluginFlag: "--plugin-dir",
	}
	agent := orch.AgentDef{
		ID:       "00-scout",
		MaxTurns: 30,
		Output:   "MANIFEST.md",
		Format:   orch.FormatMd,
	}

	cmd := orch.BuildCommand(preset, agent, "/tmp/plugin", "Write output to: /out/MANIFEST.md")

	// Command is deterministic from static inputs.
	assert.Equal(t, "claude", cmd[0])
	assert.Contains(t, cmd, "--dangerously-skip-permissions")
	assert.Contains(t, cmd, "-p")
	assert.Contains(t, cmd, "--max-turns")
	assert.Contains(t, cmd, "30")
	assert.Contains(t, cmd, "--plugin-dir")
	assert.Contains(t, cmd, "/tmp/plugin")

	// No content-derived arguments — the command doesn't reference output content,
	// task status, agent output files, or any runtime state. All arguments come
	// from preset fields and agent metadata.
	for _, arg := range cmd {
		assert.NotContains(t, arg, "completed")
		assert.NotContains(t, arg, "failed")
	}
}

// TestDSN03_NoOutputParsing verifies [DSN-03]: validation checks structure, not content.
func TestDSN03_NoOutputParsing(t *testing.T) {
	dir := t.TempDir()

	// Two md files with different content but same structure → both valid.
	writeFile(t, filepath.Join(dir, "a.md"), generateValidMd("Topic A"))
	writeFile(t, filepath.Join(dir, "b.md"), generateValidMd("Topic B"))

	assert.NoError(t, orch.ValidateOutput(filepath.Join(dir, "a.md"), orch.FormatMd))
	assert.NoError(t, orch.ValidateOutput(filepath.Join(dir, "b.md"), orch.FormatMd))

	// A structurally valid JSON file with arbitrary content → valid.
	writeFile(t, filepath.Join(dir, "data.json"), `{"garbage": true, "meaning": 42}`)
	assert.NoError(t, orch.ValidateOutput(filepath.Join(dir, "data.json"), orch.FormatJson))
}

// TestITF01_EnvironmentVariableInjection verifies [ITF-01, OBS-03]: all 6 ORCH_* vars injected
// including ORCH_RUN_ID for unique session identification.
func TestITF01_EnvironmentVariableInjection(t *testing.T) {
	// buildEnv is tested via the exported BuildEnv helper (we test the function
	// that session.go will use internally — it's a pure function we can test
	// directly through the package API).
	env := orch.BuildEnv("w-01", "run-abc", "/out", "/repo", "00-scout", "", map[string]string{
		"CUSTOM": "value",
	})

	// ITF-01: All 6 orchestrator vars present.
	assert.Equal(t, "w-01", env["ORCH_WORKER_NAME"])
	assert.Equal(t, "run-abc", env["ORCH_RUN_ID"])
	assert.Equal(t, "/out", env["ORCH_WORKSPACE"])
	assert.Equal(t, "/repo", env["ORCH_PROJECT"])
	assert.Equal(t, "00-scout", env["ORCH_ROLE"])
	assert.Equal(t, "", env["ORCH_BRANCH"])

	// Preset env vars are merged.
	assert.Equal(t, "value", env["CUSTOM"])
}

// TestITF02_EnvVarsSoleIdentityMechanism verifies [ITF-02]: env vars are sole identity mechanism.
func TestITF02_EnvVarsSoleIdentityMechanism(t *testing.T) {
	preset := &orch.Preset{
		Name:       "claude",
		Command:    "claude",
		Args:       []string{},
		PromptFlag: "-p",
		PluginFlag: "--plugin-dir",
	}
	agent := orch.AgentDef{
		ID:       "01-view",
		MaxTurns: 20,
		Output:   "VIEW.md",
	}

	cmd := orch.BuildCommand(preset, agent, "/tmp/plugin", "prompt text")

	// The command doesn't write instruction files or pipe stdin.
	// All identity is in env vars (tested by ITF-01), not command args.
	// Verify no args reference worker identity or run state.
	for _, arg := range cmd {
		assert.NotContains(t, arg, "ORCH_WORKER")
		assert.NotContains(t, arg, "ORCH_RUN")
		assert.NotContains(t, arg, "instruction")
	}
}

// TestOPS03_InputValidation verifies [OPS-03]: inputs must exist and be non-empty.
func TestOPS03_InputValidation(t *testing.T) {
	dir := t.TempDir()

	// Setup: create some valid inputs.
	writeFile(t, filepath.Join(dir, "MANIFEST.md"), generateValidMd("Manifest"))
	writeFile(t, filepath.Join(dir, "VIEW.md"), generateValidMd("View"))

	// All inputs exist and are non-empty → valid.
	err := orch.ValidateInputs([]string{"MANIFEST.md", "VIEW.md"}, dir)
	require.NoError(t, err)

	// Missing input → ErrInputMissing.
	err = orch.ValidateInputs([]string{"MANIFEST.md", "MISSING.md"}, dir)
	require.ErrorIs(t, err, orch.ErrInputMissing)
	assert.Contains(t, err.Error(), "MISSING.md")

	// Empty input → ErrInputMissing.
	writeFile(t, filepath.Join(dir, "EMPTY.md"), "")
	err = orch.ValidateInputs([]string{"EMPTY.md"}, dir)
	require.ErrorIs(t, err, orch.ErrInputMissing)

	// No inputs → valid (some agents have no inputs, e.g., scout).
	err = orch.ValidateInputs(nil, dir)
	assert.NoError(t, err)
}

// TestOPS04_StructuralOutputValidation verifies [OPS-04]: per-format structural checks.
func TestOPS04_StructuralOutputValidation(t *testing.T) {
	dir := t.TempDir()

	// --- md format ---
	t.Run("md/valid_header", func(t *testing.T) {
		path := filepath.Join(dir, "valid.md")
		writeFile(t, path, generateValidMd("Test"))
		assert.NoError(t, orch.ValidateOutput(path, orch.FormatMd))
	})
	t.Run("md/valid_frontmatter", func(t *testing.T) {
		path := filepath.Join(dir, "frontmatter.md")
		writeFile(t, path, "---\ntitle: Test\n---\n\nContent that makes this file larger than one hundred bytes in total size for the structural validation check.\n")
		assert.NoError(t, orch.ValidateOutput(path, orch.FormatMd))
	})
	t.Run("md/too_small", func(t *testing.T) {
		path := filepath.Join(dir, "small.md")
		writeFile(t, path, "# Short")
		err := orch.ValidateOutput(path, orch.FormatMd)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})
	t.Run("md/no_header", func(t *testing.T) {
		path := filepath.Join(dir, "noheader.md")
		writeFile(t, path, "This file has no markdown header and is definitely longer than one hundred bytes for the size check to pass but still fails header.\n")
		err := orch.ValidateOutput(path, orch.FormatMd)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})
	t.Run("md/agent_crash_marker_in_progress", func(t *testing.T) {
		path := filepath.Join(dir, "crash_inprog.md")
		writeFile(t, path, "---\nstatus: IN_PROGRESS\nagent: 00-scout\nstarted: 2026-04-01T00:00:00Z\ncrash_marker: true\n---\n\nThis content is from the crash detection protocol.\n")
		err := orch.ValidateOutput(path, orch.FormatMd)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})
	t.Run("md/agent_crash_marker_status_only", func(t *testing.T) {
		path := filepath.Join(dir, "crash_status.md")
		writeFile(t, path, "---\nstatus: IN_PROGRESS\nagent: 00-scout\n---\n\nPadding content to exceed one hundred bytes threshold for validation check.\n")
		err := orch.ValidateOutput(path, orch.FormatMd)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})
	t.Run("md/valid_complete_frontmatter", func(t *testing.T) {
		path := filepath.Join(dir, "complete.md")
		writeFile(t, path, "---\nstatus: COMPLETE\nagent: 00-scout\n---\n\nReal content that exceeds one hundred bytes and represents valid agent output.\n")
		assert.NoError(t, orch.ValidateOutput(path, orch.FormatMd))
	})
	t.Run("md/missing", func(t *testing.T) {
		err := orch.ValidateOutput(filepath.Join(dir, "nope.md"), orch.FormatMd)
		assert.ErrorIs(t, err, orch.ErrOutputMissing)
	})

	// --- jsonl format ---
	t.Run("jsonl/valid", func(t *testing.T) {
		path := filepath.Join(dir, "valid.jsonl")
		writeFile(t, path, `{"finding":"test"}`+"\n"+`{"finding":"test2"}`+"\n")
		assert.NoError(t, orch.ValidateOutput(path, orch.FormatJsonl))
	})
	t.Run("jsonl/invalid_first_line", func(t *testing.T) {
		path := filepath.Join(dir, "bad.jsonl")
		writeFile(t, path, "not json\n")
		err := orch.ValidateOutput(path, orch.FormatJsonl)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})

	// --- json format ---
	t.Run("json/valid", func(t *testing.T) {
		path := filepath.Join(dir, "valid.json")
		writeFile(t, path, `{"key": "value", "nested": {"a": 1}}`)
		assert.NoError(t, orch.ValidateOutput(path, orch.FormatJson))
	})
	t.Run("json/invalid", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		writeFile(t, path, `{broken json`)
		err := orch.ValidateOutput(path, orch.FormatJson)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})

	// --- yaml format ---
	t.Run("yaml/valid", func(t *testing.T) {
		path := filepath.Join(dir, "valid.yaml")
		writeFile(t, path, "key: value\nnested:\n  a: 1\n")
		assert.NoError(t, orch.ValidateOutput(path, orch.FormatYaml))
	})
	t.Run("yaml/invalid", func(t *testing.T) {
		path := filepath.Join(dir, "bad.yaml")
		writeFile(t, path, ":\n  - :\n    :\n  bad: [unclosed")
		err := orch.ValidateOutput(path, orch.FormatYaml)
		assert.ErrorIs(t, err, orch.ErrOutputInvalid)
	})
}

// TestOPS05_OutputStatusRouting verifies [OPS-05]: outcome routing by exit+validity+criticality.
func TestOPS05_OutputStatusRouting(t *testing.T) {
	// Valid output → completed (regardless of criticality).
	assert.Equal(t, orch.OutcomeCompleted, orch.DetermineOutcome(0, nil, orch.Critical))
	assert.Equal(t, orch.OutcomeCompleted, orch.DetermineOutcome(0, nil, orch.NonCritical))

	// Non-zero exit → failed (regardless of output or criticality).
	assert.Equal(t, orch.OutcomeFailed, orch.DetermineOutcome(1, nil, orch.Critical))
	assert.Equal(t, orch.OutcomeFailed, orch.DetermineOutcome(1, orch.ErrOutputInvalid, orch.NonCritical))

	// Zero exit + invalid output + critical → retry.
	assert.Equal(t, orch.OutcomeRetry, orch.DetermineOutcome(0, orch.ErrOutputInvalid, orch.Critical))
	assert.Equal(t, orch.OutcomeRetry, orch.DetermineOutcome(0, orch.ErrOutputMissing, orch.Critical))

	// Zero exit + invalid output + non-critical → gap.
	assert.Equal(t, orch.OutcomeGap, orch.DetermineOutcome(0, orch.ErrOutputInvalid, orch.NonCritical))
	assert.Equal(t, orch.OutcomeGap, orch.DetermineOutcome(0, orch.ErrOutputMissing, orch.NonCritical))
}

// TestOPS15_TurnLimit verifies [OPS-15]: --max-turns present with correct value.
func TestOPS15_TurnLimit(t *testing.T) {
	preset := &orch.Preset{
		Name:       "claude",
		Command:    "claude",
		Args:       []string{},
		PromptFlag: "-p",
	}
	agent := orch.AgentDef{
		ID:       "test-agent",
		MaxTurns: 42,
	}

	cmd := orch.BuildCommand(preset, agent, "", "test prompt")

	// Find --max-turns and verify value.
	found := false
	for i, arg := range cmd {
		if arg == "--max-turns" {
			require.Less(t, i+1, len(cmd), "--max-turns has no value")
			assert.Equal(t, "42", cmd[i+1])
			found = true
			break
		}
	}
	assert.True(t, found, "--max-turns not found in command: %v", cmd)

	// MaxTurns=0 → no --max-turns flag.
	agent.MaxTurns = 0
	cmd = orch.BuildCommand(preset, agent, "", "test prompt")
	for _, arg := range cmd {
		assert.NotEqual(t, "--max-turns", arg)
	}
}

// TestBuildShellCommand_EnvExportAndExec verifies BuildShellCommand produces
// correct shell strings with sorted env exports, exec prefix, and log redirection.
func TestBuildShellCommand_EnvExportAndExec(t *testing.T) {
	t.Run("basic_command_no_env_no_log", func(t *testing.T) {
		cmd, err := orch.BuildShellCommand([]string{"echo", "hello"}, nil, "", "")
		require.NoError(t, err)
		assert.Equal(t, "'echo' 'hello'", cmd)
	})

	t.Run("env_exports_sorted_deterministically", func(t *testing.T) {
		env := map[string]string{
			"ORCH_RUN_ID": "run-1",
			"ORCH_BRANCH": "main",
			"CUSTOM_VAR":  "value",
		}
		cmd, err := orch.BuildShellCommand([]string{"agent"}, env, "", "")
		require.NoError(t, err)
		// Keys must be sorted: CUSTOM_VAR, ORCH_BRANCH, ORCH_RUN_ID.
		assert.Contains(t, cmd, "export CUSTOM_VAR='value'; export ORCH_BRANCH='main'; export ORCH_RUN_ID='run-1';")
	})

	t.Run("log_redirection_and_exitcode", func(t *testing.T) {
		cmd, err := orch.BuildShellCommand([]string{"agent"}, nil, "/out/logs/task-01.stdout", "")
		require.NoError(t, err)
		assert.Contains(t, cmd, " > '/out/logs/task-01.stdout' 2>&1")
		assert.Contains(t, cmd, "echo $? > '/out/logs/task-01.stdout.exitcode'")
	})

	t.Run("stream_json_tee_jq_pipeline", func(t *testing.T) {
		filter := `select(.type == "assistant") | .text`
		cmd, err := orch.BuildShellCommand([]string{"claude"}, nil, "/out/logs/task-01.stdout", filter)
		require.NoError(t, err)
		assert.Contains(t, cmd, "| tee '/out/logs/task-01.stdout'")
		assert.Contains(t, cmd, "| jq -r --unbuffered")
		assert.Contains(t, cmd, "> '/out/logs/task-01.txt'")
		assert.Contains(t, cmd, "echo ${PIPESTATUS[0]} > '/out/logs/task-01.stdout.exitcode'")
		assert.NotContains(t, cmd, "2>&1", "stream-json mode should not merge stderr into stdout")
	})

	t.Run("values_with_special_chars_are_quoted", func(t *testing.T) {
		env := map[string]string{
			"PATH_VAR": "/dir with spaces/bin",
		}
		cmd, err := orch.BuildShellCommand([]string{"agent"}, env, "", "")
		require.NoError(t, err)
		assert.Contains(t, cmd, "export PATH_VAR='/dir with spaces/bin';")
	})

	t.Run("values_with_single_quotes_escaped", func(t *testing.T) {
		env := map[string]string{
			"MSG": "it's alive",
		}
		cmd, err := orch.BuildShellCommand([]string{"agent"}, env, "", "")
		require.NoError(t, err)
		assert.Contains(t, cmd, "export MSG='it'\\''s alive';")
	})

	t.Run("invalid_env_key_rejected", func(t *testing.T) {
		env := map[string]string{
			"VALID_KEY":   "ok",
			"BAD;KEY=rm;": "evil",
		}
		_, err := orch.BuildShellCommand([]string{"agent"}, env, "", "")
		assert.ErrorIs(t, err, orch.ErrEnvKeyInvalid)
	})

	t.Run("invalid_env_key_starts_with_digit", func(t *testing.T) {
		env := map[string]string{
			"1BAD": "value",
		}
		_, err := orch.BuildShellCommand([]string{"agent"}, env, "", "")
		assert.ErrorIs(t, err, orch.ErrEnvKeyInvalid)
	})

	t.Run("no_exec_prefix_for_exitcode_capture", func(t *testing.T) {
		cmd, err := orch.BuildShellCommand([]string{"claude", "--agent", "scout"}, nil, "", "")
		require.NoError(t, err)
		assert.NotContains(t, cmd, "exec ",
			"command must not use exec (prevents exit code capture)")
	})

	t.Run("command_args_with_spaces_quoted", func(t *testing.T) {
		cmd, err := orch.BuildShellCommand([]string{"claude", "-p", "Write output to: /out/test.md"}, nil, "", "")
		require.NoError(t, err)
		assert.Contains(t, cmd, "'Write output to: /out/test.md'")
	})
}

// TestReadExitCode verifies exit code sidecar parsing edge cases (PR #6 review).
func TestReadExitCode(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing_file_returns_-1", func(t *testing.T) {
		code, err := orch.ReadExitCode(filepath.Join(dir, "no-such"))
		require.NoError(t, err)
		assert.Equal(t, -1, code)
	})

	t.Run("empty_file_returns_-1", func(t *testing.T) {
		base := filepath.Join(dir, "empty")
		require.NoError(t, os.WriteFile(base+".exitcode", []byte(""), 0o644))
		code, err := orch.ReadExitCode(base)
		require.NoError(t, err)
		assert.Equal(t, -1, code, "empty sidecar must be unknown, not success")
	})

	t.Run("whitespace_only_returns_-1", func(t *testing.T) {
		base := filepath.Join(dir, "ws")
		require.NoError(t, os.WriteFile(base+".exitcode", []byte("  \n"), 0o644))
		code, err := orch.ReadExitCode(base)
		require.NoError(t, err)
		assert.Equal(t, -1, code)
	})

	t.Run("valid_zero", func(t *testing.T) {
		base := filepath.Join(dir, "zero")
		require.NoError(t, os.WriteFile(base+".exitcode", []byte("0\n"), 0o644))
		code, err := orch.ReadExitCode(base)
		require.NoError(t, err)
		assert.Equal(t, 0, code)
	})

	t.Run("valid_nonzero", func(t *testing.T) {
		base := filepath.Join(dir, "fail")
		require.NoError(t, os.WriteFile(base+".exitcode", []byte("137"), 0o644))
		code, err := orch.ReadExitCode(base)
		require.NoError(t, err)
		assert.Equal(t, 137, code)
	})
}

// TestReadAgentPrompt verifies frontmatter stripping and model extraction.
func TestReadAgentPrompt(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	t.Run("full_frontmatter", func(t *testing.T) {
		writeFile(t, filepath.Join(agentsDir, "test-agent.md"),
			"---\nname: test-agent\nmodel: opus\ntools: Glob, Grep\n---\n\n# Instructions\n\nDo the thing.\n")
		body, model, err := orch.ReadAgentPrompt(dir, "test-agent")
		require.NoError(t, err)
		assert.Equal(t, "opus", model)
		assert.Contains(t, body, "# Instructions")
		assert.Contains(t, body, "Do the thing.")
		assert.NotContains(t, body, "---")
		assert.NotContains(t, body, "name: test-agent")
	})

	t.Run("no_frontmatter", func(t *testing.T) {
		writeFile(t, filepath.Join(agentsDir, "plain.md"), "# Just a prompt\n\nNo frontmatter here.\n")
		body, model, err := orch.ReadAgentPrompt(dir, "plain")
		require.NoError(t, err)
		assert.Empty(t, model)
		assert.Contains(t, body, "# Just a prompt")
	})

	t.Run("missing_file_returns_empty", func(t *testing.T) {
		body, model, err := orch.ReadAgentPrompt(dir, "nonexistent")
		require.NoError(t, err)
		assert.Empty(t, body)
		assert.Empty(t, model)
	})

	t.Run("model_sonnet", func(t *testing.T) {
		writeFile(t, filepath.Join(agentsDir, "sonnet-agent.md"),
			"---\nmodel: sonnet\n---\n\n# Agent\n")
		_, model, err := orch.ReadAgentPrompt(dir, "sonnet-agent")
		require.NoError(t, err)
		assert.Equal(t, "sonnet", model)
	})
}

// --- test helpers ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func generateValidMd(topic string) string {
	return "# " + topic + "\n\n" +
		"This is a mock output for testing purposes. It contains enough content " +
		"to pass the structural validation check which requires files to exceed " +
		"one hundred bytes and to have a recognisable markdown header.\n"
}
