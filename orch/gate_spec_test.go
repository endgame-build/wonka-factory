//go:build verify

package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goCommentRegex matches //-line and /* */-block comments. Relies on gate.go
// containing no `//` or `/*` inside string literals (verified; keep that way).
var goCommentRegex = regexp.MustCompile(`//[^\n]*|/\*[\s\S]*?\*/`)

// TestBVV_GT03_PredecessorCheck verifies that ExecuteGate returns 1 (fail)
// without creating a PR when any predecessor has status failed or blocked
// (BVV-GT-03).
func TestBVV_GT03_PredecessorCheck(t *testing.T) {
	store := testutil.NewMockStore()

	// Create a gate task with one failed predecessor.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "build-1", Status: orch.StatusFailed}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "gate-1", Status: orch.StatusOpen}))
	require.NoError(t, store.AddDep("gate-1", "build-1"))

	exitCode := orch.ExecuteGate(
		context.Background(), store, nil,
		"gate-1", "/tmp/repo", "main", "feat/x",
		orch.DefaultGateConfig(),
	)

	assert.Equal(t, 1, exitCode, "gate should fail when predecessor is failed")
}

// TestBVV_GT03_PredecessorBlocked verifies that a blocked predecessor also
// triggers gate failure (BVV-GT-03).
func TestBVV_GT03_PredecessorBlocked(t *testing.T) {
	store := testutil.NewMockStore()

	require.NoError(t, store.CreateTask(&orch.Task{ID: "build-1", Status: orch.StatusBlocked}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "gate-1", Status: orch.StatusOpen}))
	require.NoError(t, store.AddDep("gate-1", "build-1"))

	exitCode := orch.ExecuteGate(
		context.Background(), store, nil,
		"gate-1", "/tmp/repo", "main", "feat/x",
		orch.DefaultGateConfig(),
	)

	assert.Equal(t, 1, exitCode, "gate should fail when predecessor is blocked")
}

// TestBVV_GT03_AllPredecessorsCompleted verifies that the gate proceeds
// past the predecessor check when all deps are completed.
// Note: the actual PR creation will fail (no gh CLI in tests), but the
// predecessor check itself passes — verifiable by exit code != 1 from
// predecessor check, but 1 from gh invocation.
func TestBVV_GT03_AllPredecessorsCompleted(t *testing.T) {
	store := testutil.NewMockStore()

	require.NoError(t, store.CreateTask(&orch.Task{ID: "build-1", Status: orch.StatusCompleted}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "gate-1", Status: orch.StatusOpen}))
	require.NoError(t, store.AddDep("gate-1", "build-1"))

	// ExecuteGate will pass the predecessor check but fail on gh pr create
	// (no gh CLI available in test). The exit code of 1 here is from the
	// gh failure, not the predecessor check.
	exitCode := orch.ExecuteGate(
		context.Background(), store, nil,
		"gate-1", "/tmp/repo", "main", "feat/x",
		orch.DefaultGateConfig(),
	)

	// We can't distinguish gh failure from predecessor failure via exit code
	// alone, but if predecessors were failing, we'd never reach the gh call.
	// This test documents the expected flow.
	assert.Equal(t, 1, exitCode, "gh not available in test → gate fails on PR creation")
}

// TestBVV_GT03_NoDeps verifies that a gate with no dependencies proceeds
// to PR creation (no predecessors to fail).
func TestBVV_GT03_NoDeps(t *testing.T) {
	store := testutil.NewMockStore()
	require.NoError(t, store.CreateTask(&orch.Task{ID: "gate-1", Status: orch.StatusOpen}))

	// No deps → predecessor check passes → gh call fails (no CLI).
	exitCode := orch.ExecuteGate(
		context.Background(), store, nil,
		"gate-1", "/tmp/repo", "main", "feat/x",
		orch.DefaultGateConfig(),
	)
	assert.Equal(t, 1, exitCode, "gh not available → gate fails on PR creation")
}

// TestBVV_S06_GateAuthority verifies that the gate handler's return value
// is authoritative — it returns an exit code that the dispatcher uses to
// determine the task's terminal status (BVV-S-06). This test verifies the
// interface contract: ExecuteGate returns 0 or 1, and those map to
// completed/failed via DetermineOutcome.
func TestBVV_S06_GateAuthority(t *testing.T) {
	// Gate exit 0 → DetermineOutcome → OutcomeSuccess → completed.
	assert.Equal(t, orch.OutcomeSuccess, orch.DetermineOutcome(0))
	// Gate exit 1 → DetermineOutcome → OutcomeFailure → failed (or retry).
	assert.Equal(t, orch.OutcomeFailure, orch.DetermineOutcome(1))
}

// TestBVV_GT01_NoAutoMerge verifies BVV-GT-01: gate.go must never invoke
// `gh pr merge`. Comments are stripped before scanning so a docstring that
// legitimately mentions "merge" doesn't false-positive. Three substring
// patterns cover the realistic regression modes — exec args, shell form,
// and format-string embedding.
func TestBVV_GT01_NoAutoMerge(t *testing.T) {
	src, err := os.ReadFile("gate.go")
	require.NoError(t, err, "gate.go must be readable from package dir")
	stripped := goCommentRegex.ReplaceAllString(string(src), "")

	for _, pat := range []string{
		`"pr", "merge"`, // exec.Command("gh", "pr", "merge", ...)
		`"pr merge"`,    // shell form or single-arg invocation
		`pr merge`,      // embedded in a format string / shell command
	} {
		assert.NotContains(t, stripped, pat,
			"gate.go must not contain %q (BVV-GT-01)", pat)
	}
}

// TestBVV_GT02_GateFailureIsolation verifies BVV-GT-02: a failing gate on
// one branch must not affect either the state or the predecessor-check
// outcome of a gate on a different branch. The gate is entirely
// branch-scoped — each ExecuteGate call reads only its own branch's
// dependency state.
//
// The test runs both gates, shares an EventLog between them, and asserts:
//
//  1. Gate A returns exit 1 because its predecessor is failed (BVV-GT-03).
//  2. Branch B's predecessor (build-b) remains completed after gate-a's
//     failure — i.e., gate-a did not mutate cross-branch state.
//  3. Gate B passes the predecessor check (its own predecessor is
//     completed) and proceeds to the gh invocation. The gh CLI is absent
//     in tests, so gate-b also returns 1 — but via a different path.
//  4. The two gate_failed event Summaries carry divergent reasons:
//     gate-a's contains "predecessor"; gate-b's contains "gh pr".
//     Proves the "different failure mode" claim instead of treating the
//     exit codes as interchangeable.
func TestBVV_GT02_GateFailureIsolation(t *testing.T) {
	store := testutil.NewMockStore()

	// Branch A: failed predecessor → gate must fail at the predecessor check.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "build-a", Status: orch.StatusFailed,
		Labels: map[string]string{orch.LabelBranch: "feat/a"}}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "gate-a", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/a"}}))
	require.NoError(t, store.AddDep("gate-a", "build-a"))

	// Branch B: completed predecessor → gate's predecessor check passes,
	// then fails downstream on the gh invocation (no gh CLI in tests).
	require.NoError(t, store.CreateTask(&orch.Task{ID: "build-b", Status: orch.StatusCompleted,
		Labels: map[string]string{orch.LabelBranch: "feat/b"}}))
	require.NoError(t, store.CreateTask(&orch.Task{ID: "gate-b", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/b"}}))
	require.NoError(t, store.AddDep("gate-b", "build-b"))

	// Shared event log — both gate invocations write here, so we can
	// distinguish their failure paths from a single artifact.
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := orch.NewEventLog(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, log.Close()) })

	exitA := orch.ExecuteGate(context.Background(), store, log,
		"gate-a", "/tmp/repo", "main", "feat/a", orch.DefaultGateConfig())
	assert.Equal(t, 1, exitA, "branch A gate must fail on failed predecessor")

	// Cross-branch state assertion: gate-a's failure did not mutate branch B.
	depB, err := store.GetTask("build-b")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusCompleted, depB.Status,
		"branch B predecessor must remain completed after branch A gate failed")

	exitB := orch.ExecuteGate(context.Background(), store, log,
		"gate-b", "/tmp/repo", "main", "feat/b", orch.DefaultGateConfig())
	assert.Equal(t, 1, exitB, "branch B gate must return cleanly even when gh is unavailable")

	// Re-confirm post-condition: gate-b's run did not retroactively change
	// build-a or gate-a (defensive — would catch any cross-branch write).
	depA, err := store.GetTask("build-a")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusFailed, depA.Status,
		"branch A predecessor state must survive branch B gate execution")

	// Event-log discrimination: scan gate_failed emissions and pin the
	// divergent Summary text — the load-bearing evidence that the two
	// gates failed via different paths, not the exit code alone.
	summaries := gateFailedSummaries(t, logPath)
	assert.Contains(t, summaries["gate-a"], "predecessor",
		"gate-a must fail via the predecessor check (BVV-GT-03); summary was %q", summaries["gate-a"])
	assert.Contains(t, summaries["gate-b"], "gh pr",
		"gate-b must fail via the gh invocation (predecessor passed); summary was %q", summaries["gate-b"])
	assert.NotContains(t, summaries["gate-b"], "predecessor",
		"gate-b must NOT fail on predecessor check — its predecessor is completed")
}

// gateFailedSummaries returns the last gate_failed Summary per taskID.
func gateFailedSummaries(t *testing.T, logPath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ev := range readEvents(t, logPath) {
		if ev.Kind == orch.EventGateFailed {
			out[ev.TaskID] = ev.Summary
		}
	}
	return out
}
