package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLookupPreset_Claude verifies the single registered preset resolves and
// carries the load-bearing flags. The assertions pin values that would
// silently break orch spawning if a refactor dropped them:
//   - SystemPromptFlag drives --append-system-prompt-file injection
//     (orch.SpawnSession writes the body to PromptPath; orch.BuildCommand
//     passes the path through). The file-form is required because CHARLIE.md
//     (~16 KB) overflows tmux's command-parsing buffer in body-form on macOS.
//   - SystemPromptIsFile must stay true for the sidecar write to fire — the
//     flag string alone is not enough; orch.SpawnSession reads the boolean.
//   - --dangerously-skip-permissions prevents tmux sessions from hanging on
//     permission prompts (see preset doc comment)
//   - --print + KickoffPrompt are required for non-interactive operation;
//     dropping either makes claude exit immediately with "Input must be
//     provided either through stdin or as a prompt argument"
//   - TextFilter enables the stream-json → .txt log split used for debugging
func TestLookupPreset_Claude(t *testing.T) {
	p, err := LookupPreset("claude")
	require.NoError(t, err)
	require.NotNil(t, p)

	assert.Equal(t, "claude", p.Name)
	assert.Equal(t, "claude", p.Command)
	assert.Equal(t, "--append-system-prompt-file", p.SystemPromptFlag,
		"must use the file-path flag, not --append-system-prompt — large prompts overflow tmux's argv buffer")
	assert.True(t, p.SystemPromptIsFile,
		"SystemPromptIsFile must be true so SpawnSession writes a sidecar; without it the body inlines into argv and overflows tmux on macOS")
	assert.Equal(t, "--model", p.ModelFlag)
	assert.Contains(t, p.Args, "--dangerously-skip-permissions",
		"without this, claude blocks on tool-use permission prompts")
	assert.Contains(t, p.Args, "--print",
		"without --print, claude rejects --output-format stream-json with 'Input must be provided'")
	assert.Contains(t, p.Args, "--output-format")
	assert.Contains(t, p.Args, "stream-json")
	assert.NotEmpty(t, p.KickoffPrompt,
		"--print mode requires a positional prompt; empty kickoff means claude exits 1 at startup")
	assert.NotEmpty(t, p.TextFilter, "stream-json output needs a jq filter for human-readable logs")
}

// TestLookupPreset_Unknown verifies unknown names produce a helpful error that
// names the alternatives — the CLI surface depends on this wording to guide
// users after a typo.
func TestLookupPreset_Unknown(t *testing.T) {
	_, err := LookupPreset("gpt4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gpt4")
	assert.Contains(t, err.Error(), "claude", "error must enumerate available presets")
}

// TestPresetNames_Sorted verifies the helper returns names in deterministic
// (lexical) order so error messages and future --help output are stable.
func TestPresetNames_Sorted(t *testing.T) {
	names := presetNames()
	require.NotEmpty(t, names)
	for i := 1; i < len(names); i++ {
		assert.Less(t, names[i-1], names[i], "presetNames must be sorted")
	}
}
