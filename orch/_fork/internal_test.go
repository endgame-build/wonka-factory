// White-box tests for unexported functions (isRetryTask, agentPromptArgs).
// Uses package orch (not orch_test) to access unexported symbols.
package orch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupAgentFile creates a plugin directory with a "test-agent.md" file and returns the plugin dir path.
func setupAgentFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(agentDir, "test-agent.md"),
		[]byte(content),
		0o644,
	))
	return dir
}

// --- isRetryTask edge cases ---

func TestIsRetryTask_Canonical(t *testing.T) {
	assert.True(t, isRetryTask(RetryTaskID("agent-01", 1)))
	assert.True(t, isRetryTask(RetryTaskID("consensus-merger", 3)))
	assert.True(t, isRetryTask("some-agent_retry_42"))
}

func TestIsRetryTask_NonRetry(t *testing.T) {
	assert.False(t, isRetryTask("agent-01"))
	assert.False(t, isRetryTask("00-facet-scan-scout"))
	assert.False(t, isRetryTask("consensus-merger"))
}

func TestIsRetryTask_FalsePositiveGuard(t *testing.T) {
	// Agent IDs that contain "_retry_" but are NOT retry tasks.
	assert.False(t, isRetryTask("auto_retry_handler"),
		"agent ID ending with _retry_handler should not match — suffix must be digits")
	assert.False(t, isRetryTask("task_retry_abc"),
		"agent ID with non-digit suffix should not match")
	assert.False(t, isRetryTask("task_retry_"),
		"agent ID ending with _retry_ and no digits should not match")
}

func TestIsRetryTask_PartialMatch(t *testing.T) {
	// Ensure the regex is anchored at the end.
	assert.False(t, isRetryTask("agent_retry_1_extra"),
		"should not match when _retry_N is not at end of string")
}

// --- agentPromptArgs tests ---

func TestAgentPromptArgs_EarlyReturnNoSystemPromptFlag(t *testing.T) {
	wp := &WorkerPool{}
	preset := &Preset{} // SystemPromptFlag is empty

	args, err := wp.agentPromptArgs(preset, "/some/dir", "agent-01")
	require.NoError(t, err)
	assert.Nil(t, args)
}

func TestAgentPromptArgs_EarlyReturnNoPluginDir(t *testing.T) {
	wp := &WorkerPool{}
	preset := &Preset{SystemPromptFlag: "--system-prompt"}

	args, err := wp.agentPromptArgs(preset, "", "agent-01")
	require.NoError(t, err)
	assert.Nil(t, args)
}

func TestAgentPromptArgs_AgentNotFound(t *testing.T) {
	wp := &WorkerPool{}
	preset := &Preset{SystemPromptFlag: "--system-prompt"}

	// Plugin dir exists but has no agents/ subdirectory.
	dir := t.TempDir()
	args, err := wp.agentPromptArgs(preset, dir, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, args, "missing agent file returns nil args, not an error")
}

func TestAgentPromptArgs_InjectsPrompt(t *testing.T) {
	dir := setupAgentFile(t, "You are a test agent.")

	wp := &WorkerPool{}
	preset := &Preset{SystemPromptFlag: "--system-prompt"}

	args, err := wp.agentPromptArgs(preset, dir, "test-agent")
	require.NoError(t, err)
	require.Equal(t, []string{"--system-prompt", "You are a test agent."}, args)
}

func TestAgentPromptArgs_InjectsPromptAndModel(t *testing.T) {
	dir := setupAgentFile(t, "---\nmodel: opus\n---\nYou are a test agent.")

	wp := &WorkerPool{}
	preset := &Preset{SystemPromptFlag: "--system-prompt", ModelFlag: "--model"}

	args, err := wp.agentPromptArgs(preset, dir, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, []string{"--system-prompt", "You are a test agent.", "--model", "opus"}, args)
}

func TestAgentPromptArgs_ModelSkippedWithoutFlag(t *testing.T) {
	dir := setupAgentFile(t, "---\nmodel: opus\n---\nYou are a test agent.")

	wp := &WorkerPool{}
	preset := &Preset{SystemPromptFlag: "--system-prompt"} // no ModelFlag

	args, err := wp.agentPromptArgs(preset, dir, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, []string{"--system-prompt", "You are a test agent."}, args,
		"model should be omitted when preset has no ModelFlag")
}

func TestAgentPromptArgs_AppliesPromptTransform(t *testing.T) {
	dir := setupAgentFile(t, "Read facet/references/TEMPLATE.md then produce output.")

	var capturedAgentID string
	wp := &WorkerPool{
		promptTransform: func(agentID, body string) string {
			capturedAgentID = agentID
			return "TRANSFORMED: " + body
		},
	}
	preset := &Preset{SystemPromptFlag: "--system-prompt"}

	args, err := wp.agentPromptArgs(preset, dir, "test-agent")
	require.NoError(t, err)

	assert.Equal(t, "test-agent", capturedAgentID,
		"transform should receive the agent ID")
	require.Len(t, args, 2)
	assert.Equal(t, "--system-prompt", args[0])
	assert.Contains(t, args[1], "TRANSFORMED:",
		"prompt body should be transformed before injection")
}

func TestAgentPromptArgs_NilTransformSkipped(t *testing.T) {
	dir := setupAgentFile(t, "Original prompt.")

	wp := &WorkerPool{promptTransform: nil}
	preset := &Preset{SystemPromptFlag: "--system-prompt"}

	args, err := wp.agentPromptArgs(preset, dir, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, []string{"--system-prompt", "Original prompt."}, args,
		"nil transform should pass prompt through unchanged")
}
