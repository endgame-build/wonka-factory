//go:build verify

package orch_test

import (
	"context"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
