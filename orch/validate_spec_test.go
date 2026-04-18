//go:build verify

package orch_test

import (
	"errors"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildWellFormedGraph seeds a minimal, canonical BVV lifecycle graph:
//
//	plan → build → verify → gate
//
// Every task sits under one branch and carries a role label that matches
// the RoleConfig map used in the tests. This is the baseline that every
// BVV-TG-* negative test mutates to trigger a specific failure mode.
func buildWellFormedGraph(t *testing.T, store *testutil.MockStore, branch string) {
	t.Helper()
	mk := func(id, role string) {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: id, Status: orch.StatusOpen,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        role,
				orch.LabelCriticality: string(orch.NonCritical),
			},
		}))
	}
	mk("plan-1", "planner")
	mk("build-1", "builder")
	mk("verify-1", "verifier")
	mk("gate-1", "gate")
	require.NoError(t, store.AddDep("build-1", "plan-1"))
	require.NoError(t, store.AddDep("verify-1", "build-1"))
	require.NoError(t, store.AddDep("gate-1", "verify-1"))
}

// standardRoles returns the role set the validator expects for the canonical
// lifecycle. Keeps test setup terse — every positive test uses the same map.
func standardRoles() map[string]orch.RoleConfig {
	return map[string]orch.RoleConfig{
		"planner":  testutil.MockRoleConfig(),
		"builder":  testutil.MockRoleConfig(),
		"verifier": testutil.MockRoleConfig(),
		"gate":     testutil.MockRoleConfig(),
	}
}

// requireGraphError asserts that err is a *GraphValidationError pinned to
// the expected BVV-TG-* requirement. Centralizes the assertion so each
// test reads as "build graph, call validator, check requirement ID".
// wantReq is a string for readability; converted to orch.TGRequirement
// internally.
func requireGraphError(t *testing.T, err error, wantReq string) *orch.GraphValidationError {
	t.Helper()
	require.Error(t, err, "expected validation error, got nil")
	var ve *orch.GraphValidationError
	require.True(t, errors.As(err, &ve), "expected *GraphValidationError, got %T: %v", err, err)
	assert.Equal(t, orch.TGRequirement(wantReq), ve.Requirement, "wrong BVV-TG-* requirement: %s", err)
	return ve
}

// TestValidate_WellFormed verifies the positive baseline: the canonical
// plan→build→verify→gate graph passes BVV-TG-07..10.
func TestValidate_WellFormed(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	assert.NoError(t, orch.ValidateLifecycleGraph(store, "feat/x", standardRoles()))
}

// TestBVV_TG05_SkipWhenNoPlanner verifies the "Level 1 pre-populated ledger"
// skip — validation returns nil silently when no role:planner task exists
// for the branch. Maps to BVV-TG-05 semantics (the planner-created pattern
// is optional at Level 1).
func TestBVV_TG05_SkipWhenNoPlanner(t *testing.T) {
	store := testutil.NewMockStore()
	// Seed only build/verify/gate — no planner.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "build-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder"},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "verify-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "verifier"},
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "gate-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "gate"},
	}))
	assert.NoError(t, orch.ValidateLifecycleGraph(store, "feat/x", standardRoles()))
}

// TestBVV_TG07_UnknownRole verifies that tasks with a role label not in
// the configured role set are rejected as undispatchable.
func TestBVV_TG07_UnknownRole(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	// Inject an extra task with an unconfigured role.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "mystery-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "mystery"},
	}))
	require.NoError(t, store.AddDep("mystery-1", "plan-1"))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-07")
	assert.Contains(t, ve.TaskIDs, "mystery-1")
}

// TestBVV_TG07_MissingRole verifies that a task with no role label at all
// is also rejected — the validator treats "" identically to "unknown role".
func TestBVV_TG07_MissingRole(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "roleless-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x"},
	}))
	require.NoError(t, store.AddDep("roleless-1", "plan-1"))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-07")
	assert.Contains(t, ve.TaskIDs, "roleless-1")
}

// TestBVV_TG07_EscalationExempt verifies that role:escalation tasks do
// not trigger TG-07 failures even though "escalation" is never in the
// configured role set. Escalations are orchestrator-created human inboxes,
// not dispatchable work — see dispatch.go's role=="escalation" skip.
func TestBVV_TG07_EscalationExempt(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "escalation-foo", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "escalation"},
	}))
	// Note: escalation tasks are intentionally NOT wired to the plan graph.
	// The TG-10 exemption below must cover this.
	assert.NoError(t, orch.ValidateLifecycleGraph(store, "feat/x", standardRoles()))
}

// TestBVV_TG08_Cycle verifies that a dependency cycle is detected even when
// it bypassed AddDep (the ledger's primary cycle enforcement). Uses
// MockStore.InjectDep to construct an impossible-through-AddDep cycle.
func TestBVV_TG08_Cycle(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	// Force a cycle: gate-1 already depends on verify-1 (via the canonical
	// edge). Inject verify-1 → gate-1 directly and we have gate-1 ↔ verify-1.
	store.InjectDep("verify-1", "gate-1")

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-08")
	assert.NotEmpty(t, ve.TaskIDs, "cycle evidence must name at least one node")
}

// TestBVV_TG09_MissingGate verifies zero gate tasks → TG-09 failure.
func TestBVV_TG09_MissingGate(t *testing.T) {
	store := testutil.NewMockStore()
	// Build plan→build→verify without the gate task.
	mk := func(id, role string) {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: id, Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: role},
		}))
	}
	mk("plan-1", "planner")
	mk("build-1", "builder")
	mk("verify-1", "verifier")
	require.NoError(t, store.AddDep("build-1", "plan-1"))
	require.NoError(t, store.AddDep("verify-1", "build-1"))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	_ = requireGraphError(t, err, "BVV-TG-09")
}

// TestBVV_TG09_MultipleGates verifies that >1 role:gate task → TG-09 failure.
func TestBVV_TG09_MultipleGates(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	// Add a second gate — still reachable from plan, still covers verifiers.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "gate-2", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "gate"},
	}))
	require.NoError(t, store.AddDep("gate-2", "verify-1"))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-09")
	assert.ElementsMatch(t, []string{"gate-1", "gate-2"}, ve.TaskIDs)
}

// TestBVV_TG09_GateDoesNotCoverVerifier verifies that a gate that skips a
// verifier — directly or transitively — fails TG-09.
func TestBVV_TG09_GateDoesNotCoverVerifier(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	// Add a second verifier that the gate does NOT depend on.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "verify-2", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "verifier"},
	}))
	require.NoError(t, store.AddDep("verify-2", "plan-1"))
	// verify-2 is in the branch graph but unreachable from gate-1.

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-09")
	assert.Contains(t, ve.TaskIDs, "verify-2")
}

// TestBVV_TG10_OrphanTask verifies that a branch task unreachable from the
// plan via dependency edges → TG-10 failure.
func TestBVV_TG10_OrphanTask(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	// Add a task with a valid role but no path to plan-1.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "orphan-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "builder"},
	}))
	// No AddDep tying orphan-1 into the graph.

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-10")
	assert.Contains(t, ve.TaskIDs, "orphan-1")
}

// TestBVV_TG10_MultiplePlanners verifies that >1 role:planner task → TG-10
// failure. The spec writes "the plan task" (singular); multiple planners
// on one branch is undefined behavior.
func TestBVV_TG10_MultiplePlanners(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/x")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "plan-2", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "planner"},
	}))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	ve := requireGraphError(t, err, "BVV-TG-10")
	assert.ElementsMatch(t, []string{"plan-1", "plan-2"}, ve.TaskIDs)
}

// TestBVV_TG11_PartialGraphFails captures spec §7.4 intent: a partially-
// created graph (planner crashed mid-creation — e.g., plan + build only,
// no verify or gate) is NOT dispatchable at Level 2 because it can't
// satisfy TG-09 (missing gate). Validation surfaces the degraded state.
func TestBVV_TG11_PartialGraphFails(t *testing.T) {
	store := testutil.NewMockStore()
	// Partial: plan created a build task, then died before verify/gate.
	mk := func(id, role string) {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: id, Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: role},
		}))
	}
	mk("plan-1", "planner")
	mk("build-1", "builder")
	require.NoError(t, store.AddDep("build-1", "plan-1"))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	// Partial graph must fail — it has a planner but no gate (TG-09).
	_ = requireGraphError(t, err, "BVV-TG-09")
}

// TestBVV_TG12_PlanOnlyFails captures spec §7.4 intent: a "plan only"
// terminal state (planner produced no build/V&V/gate) is also not
// well-formed — TG-09 catches the missing gate.
func TestBVV_TG12_PlanOnlyFails(t *testing.T) {
	store := testutil.NewMockStore()
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "plan-1", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/x", orch.LabelRole: "planner"},
	}))

	err := orch.ValidateLifecycleGraph(store, "feat/x", standardRoles())
	_ = requireGraphError(t, err, "BVV-TG-09")
}

// TestAllTGRequirements pins the closed set of BVV-TG-* requirement IDs.
// Adding a new requirement without updating AllTGRequirements would pass
// compile but fail this test — surfacing the drift at its narrowest seam
// (the enum) rather than deep in audit-trail tooling.
func TestAllTGRequirements(t *testing.T) {
	want := []orch.TGRequirement{orch.ReqTG07, orch.ReqTG08, orch.ReqTG09, orch.ReqTG10}
	assert.ElementsMatch(t, want, orch.AllTGRequirements,
		"AllTGRequirements must exactly match the declared ReqTG* constants")
	// Pin the format — operator tooling greps the audit trail for this
	// shape ("BVV-TG-<digits>"), so accidental reformatting must fail loud.
	for _, r := range orch.AllTGRequirements {
		assert.Regexp(t, `^BVV-TG-\d{2}$`, string(r), "requirement %q must match BVV-TG-NN format", r)
	}
}

// TestValidate_GraphValidationErrorFormat verifies the Error() string carries
// both the requirement ID and the offending task IDs — operators grep audit
// trails for BVV-TG-* patterns, so format stability matters.
func TestValidate_GraphValidationErrorFormat(t *testing.T) {
	ve := &orch.GraphValidationError{
		Requirement: orch.ReqTG09,
		Reason:      "exactly one role:gate task required, got 0",
		TaskIDs:     nil,
	}
	assert.Equal(t, "[BVV-TG-09] exactly one role:gate task required, got 0", ve.Error())

	ve2 := &orch.GraphValidationError{
		Requirement: orch.ReqTG10,
		Reason:      "tasks not reachable from plan task via dependency edges",
		TaskIDs:     []string{"orphan-1", "orphan-2"},
	}
	assert.Contains(t, ve2.Error(), "[BVV-TG-10]")
	assert.Contains(t, ve2.Error(), "orphan-1")
	assert.Contains(t, ve2.Error(), "orphan-2")
}

// TestValidate_BranchIsolation verifies that tasks on OTHER branches don't
// affect validation of the target branch. Two lifecycles sharing a ledger
// is the common Beads-backed deployment.
func TestValidate_BranchIsolation(t *testing.T) {
	store := testutil.NewMockStore()
	buildWellFormedGraph(t, store, "feat/a")
	// Seed a malformed graph on a different branch — should not affect feat/a.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "orphan-b", Status: orch.StatusOpen,
		Labels: map[string]string{orch.LabelBranch: "feat/b", orch.LabelRole: "builder"},
	}))

	assert.NoError(t, orch.ValidateLifecycleGraph(store, "feat/a", standardRoles()),
		"other-branch tasks must not contaminate feat/a validation")
}
