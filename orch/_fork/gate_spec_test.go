package orch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- EvaluateGate Tests (EXP-10, S6) ---

// TestEXP10_GateNone verifies [EXP-10]: no gate → GateNone.
func TestEXP10_GateNone(t *testing.T) {
	verdict := orch.EvaluateGate(nil, nil)
	assert.Equal(t, orch.GateNone, verdict)
}

// TestEXP10_GatePass verifies [EXP-10]: completed gate agent → GatePass.
func TestEXP10_GatePass(t *testing.T) {
	children := []*orch.Task{
		{ID: "validator", Type: orch.TypeAgent, Status: orch.StatusCompleted, AgentID: "11-validation"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GatePass, verdict)
}

// TestS6_GateFail verifies [S6, S4]: failed gate agent + halt=true → GateFail.
// No silent capability loss (S4) — halting gate failure terminates pipeline, no bypass path.
func TestS6_GateFail(t *testing.T) {
	children := []*orch.Task{
		{ID: "validator", Type: orch.TypeAgent, Status: orch.StatusFailed, AgentID: "11-validation"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GateFail, verdict)
}

// TestEXP10_GatePending verifies gate agent not yet terminal → GatePending.
func TestEXP10_GatePending(t *testing.T) {
	children := []*orch.Task{
		{ID: "validator", Type: orch.TypeAgent, Status: orch.StatusInProgress, AgentID: "11-validation"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GatePending, verdict)
}

// TestEXP10_GateAgentNotFound verifies missing gate agent → GateNone.
func TestEXP10_GateAgentNotFound(t *testing.T) {
	children := []*orch.Task{
		{ID: "other", Type: orch.TypeAgent, Status: orch.StatusCompleted, AgentID: "other-agent"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GateNone, verdict)
}

// TestEXP10_GateRetryPassOverridesFail verifies that a successful retry task
// causes GatePass even when the original task failed (PR #6 review).
func TestEXP10_GateRetryPassOverridesFail(t *testing.T) {
	children := []*orch.Task{
		{ID: "validator", Type: orch.TypeAgent, Status: orch.StatusFailed, AgentID: "11-validation"},
		{ID: orch.RetryTaskID("11-validation", 1), Type: orch.TypeAgent, Status: orch.StatusCompleted, AgentID: "11-validation"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GatePass, verdict, "completed retry should override prior failure")
}

// TestEXP10_GateRetryPendingOverridesFail verifies that a running retry task
// causes GatePending even when the original task failed (PR #6 review).
func TestEXP10_GateRetryPendingOverridesFail(t *testing.T) {
	children := []*orch.Task{
		{ID: "validator", Type: orch.TypeAgent, Status: orch.StatusFailed, AgentID: "11-validation"},
		{ID: orch.RetryTaskID("11-validation", 1), Type: orch.TypeAgent, Status: orch.StatusOpen, AgentID: "11-validation"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GatePending, verdict, "open retry should yield pending, not fail")
}

// TestEXP10_GateAllRetriesFailed verifies that GateFail only fires when ALL
// attempts (original + retries) have failed (PR #6 review).
func TestEXP10_GateAllRetriesFailed(t *testing.T) {
	children := []*orch.Task{
		{ID: "validator", Type: orch.TypeAgent, Status: orch.StatusFailed, AgentID: "11-validation"},
		{ID: orch.RetryTaskID("11-validation", 1), Type: orch.TypeAgent, Status: orch.StatusFailed, AgentID: "11-validation"},
	}
	gate := &orch.QualityGate{Agent: "11-validation", Halt: true}
	verdict := orch.EvaluateGate(children, gate)
	assert.Equal(t, orch.GateFail, verdict, "all attempts failed should be GateFail")
}

// --- EvaluateZeroFindings Tests (CON-05) ---

// TestCON05_ZeroFindingsMd verifies [CON-05]: md file with only headers → zero findings.
func TestCON05_ZeroFindingsMd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.md")

	require.NoError(t, os.WriteFile(path, []byte("---\ntitle: test\n---\n# Section\n\n## Subsection\n"), 0o644))
	zero, err := orch.EvaluateZeroFindings(path, orch.FormatMd)
	require.NoError(t, err)
	assert.True(t, zero, "md with only headers should be zero findings")
}

// TestCON05_NonZeroFindingsMd verifies md file with content → non-zero findings.
func TestCON05_NonZeroFindingsMd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.md")

	require.NoError(t, os.WriteFile(path, []byte("# Section\nSome actual finding here.\n"), 0o644))
	zero, err := orch.EvaluateZeroFindings(path, orch.FormatMd)
	require.NoError(t, err)
	assert.False(t, zero, "md with content should not be zero findings")
}

// TestCON05_ZeroFindingsJsonl verifies [CON-05]: empty JSONL → zero findings.
func TestCON05_ZeroFindingsJsonl(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.jsonl")

	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))
	zero, err := orch.EvaluateZeroFindings(path, orch.FormatJsonl)
	require.NoError(t, err)
	assert.True(t, zero)
}

// TestCON05_NonZeroFindingsJsonl verifies JSONL with data → non-zero.
func TestCON05_NonZeroFindingsJsonl(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.jsonl")

	require.NoError(t, os.WriteFile(path, []byte(`{"finding":"test"}`+"\n"), 0o644))
	zero, err := orch.EvaluateZeroFindings(path, orch.FormatJsonl)
	require.NoError(t, err)
	assert.False(t, zero)
}
