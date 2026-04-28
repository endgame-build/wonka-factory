//go:build verify

package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
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

// TestBuildCommand_KickoffPromptLast verifies the KickoffPrompt is appended
// after every flag. claude's --print mode interprets the trailing positional
// as the user prompt; if a flag like --max-turns leaks past it, claude either
// errors or treats the flag-value as the prompt. Pin the order explicitly.
func TestBuildCommand_KickoffPromptLast(t *testing.T) {
	preset := &orch.Preset{
		Name:             "claude",
		Command:          "claude",
		Args:             []string{"--print", "--verbose"},
		SystemPromptFlag: "--append-system-prompt",
		ModelFlag:        "--model",
		KickoffPrompt:    "Begin.",
	}
	cmd := orch.BuildCommand(preset, "you are a builder", "opus", 20)
	assert.Equal(t, "Begin.", cmd[len(cmd)-1], "kickoff must be the last positional")
	assert.Equal(t, []string{
		"claude",
		"--print", "--verbose",
		"--append-system-prompt", "you are a builder",
		"--model", "opus",
		"--max-turns", "20",
		"Begin.",
	}, cmd)
}

// TestBuildCommand_KickoffPromptOmittedWhenEmpty verifies the kickoff is
// skipped when empty — required so presets that don't need a positional
// (future codex/goose if they support stdin-based agentic mode) don't end up
// with a stray empty positional that some CLIs would interpret as an
// empty prompt.
func TestBuildCommand_KickoffPromptOmittedWhenEmpty(t *testing.T) {
	preset := &orch.Preset{
		Command:          "agent",
		SystemPromptFlag: "--system",
		// KickoffPrompt deliberately empty.
	}
	cmd := orch.BuildCommand(preset, "body", "", 0)
	assert.Equal(t, []string{"agent", "--system", "body"}, cmd,
		"empty KickoffPrompt must produce no trailing positional")
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

// TestPromptPath_Canonical verifies the canonical prompt sidecar layout:
// {runDir}/logs/{taskID}.prompt.md. SpawnSession writes here for file-form
// presets; a refactor that moves the path silently desyncs SpawnSession's
// write from anything else (cleanup, log inspection) reading it.
func TestPromptPath_Canonical(t *testing.T) {
	got := orch.PromptPath("/run/abc", "task-001")
	want := filepath.Join("/run/abc", "logs", "task-001.prompt.md")
	assert.Equal(t, want, got)
}

// TestBuildCommand_FilePathPassedVerbatim pins the BuildCommand contract:
// the function is value-agnostic — it does not read, transform, or expand
// the systemPromptValue, even when the value looks like a path. SpawnSession
// owns the body-vs-path decision; BuildCommand just appends the value.
func TestBuildCommand_FilePathPassedVerbatim(t *testing.T) {
	preset := &orch.Preset{
		Command:          "claude",
		SystemPromptFlag: "--append-system-prompt-file",
	}
	path := "/run/abc/logs/task-001.prompt.md"
	cmd := orch.BuildCommand(preset, path, "", 0)

	idx := slices.Index(cmd, "--append-system-prompt-file")
	require.NotEqual(t, -1, idx, "flag missing from command: %v", cmd)
	require.Less(t, idx, len(cmd)-1, "flag must have a value following it")
	assert.Equal(t, path, cmd[idx+1], "BuildCommand must pass the path verbatim, no read/expand")
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

// TestBVV_AI02_RoleToInstructionMapping verifies BVV-AI-02 end-to-end:
// the dispatcher resolves a task's role label to the configured
// RoleConfig and passes that config to the spawn path. Exercises the
// production lookup instead of a test-local map — a regression that
// routes by content (BVV-S-05 violation) or drops InstructionFile from
// roleCfg would fail here.
func TestBVV_AI02_RoleToInstructionMapping(t *testing.T) {
	branch := "feat/ai02"
	rolePaths := map[string]string{
		orch.RoleBuilder:  "/agents/OOMPA.md",
		orch.RoleVerifier: "/agents/LOOMPA.md",
		orch.RolePlanner:  "/agents/CHARLIE.md",
	}

	roles := make([]string, 0, len(rolePaths))
	for r := range rolePaths {
		roles = append(roles, r)
	}
	lifecycle := testutil.MockLifecycleConfig(branch, roles...)
	for r, path := range rolePaths {
		lifecycle.Roles[r] = orch.RoleConfig{InstructionFile: path}
	}

	store := testutil.NewMockStore()
	for r := range rolePaths {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: "t-" + r, Status: orch.StatusOpen,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        r,
				orch.LabelCriticality: string(orch.NonCritical),
			},
		}))
	}

	pool := orch.NewWorkerPool(store, nil, len(rolePaths), "test-run", "/repo", t.TempDir())
	d, err := orch.NewDispatcher(
		store, pool, nil, nil, nil,
		orch.NewGapTracker(lifecycle.GapTolerance),
		orch.NewRetryState(),
		orch.NewHandoffState(lifecycle.MaxHandoffs),
		orch.RetryConfig{MaxRetries: 0, BaseTimeout: 30 * time.Minute},
		lifecycle,
		orch.DispatchConfig{Interval: 10 * time.Millisecond, AgentPollInterval: 5 * time.Millisecond},
		nil,
	)
	require.NoError(t, err)

	var mu sync.Mutex
	observed := map[string]string{}
	d.SetSpawnFunc(func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, _ int, outcomes chan<- orch.TaskOutcome) {
		mu.Lock()
		observed[task.ID] = roleCfg.InstructionFile
		mu.Unlock()
		outcomes <- orch.NewTaskOutcome(task, worker, orch.OutcomeSuccess, 0, roleCfg)
	})

	d.Tick(context.Background())
	d.Wait()

	mu.Lock()
	defer mu.Unlock()
	for r, want := range rolePaths {
		assert.Equal(t, want, observed["t-"+r],
			"role %q must dispatch with %q (label-derived lookup)", r, want)
	}
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
