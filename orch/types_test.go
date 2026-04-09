//go:build verify

package orch

import "testing"

// TestTaskStatus_Terminal verifies that all terminal statuses — including
// the BVV-added StatusBlocked — report Terminal() == true, and that
// non-terminal statuses report false.
//
// Covers: BVV spec §5.1a (task status enum).
// Prerequisite for: BVV-S-02 (terminal irreversibility) — a status must be
// classifiable as terminal before runtime invariant assertions can enforce
// irreversibility (planned for Phase 7).
func TestTaskStatus_Terminal(t *testing.T) {
	cases := []struct {
		status TaskStatus
		want   bool
	}{
		{StatusOpen, false},
		{StatusAssigned, false},
		{StatusInProgress, false},
		{StatusCompleted, true},
		{StatusFailed, true},
		{StatusBlocked, true},          // BVV addition per §5.1a
		{TaskStatus("garbage"), false}, // unknown statuses must not be terminal
		{TaskStatus(""), false},        // zero-value must not be terminal
	}
	for _, c := range cases {
		if got := c.status.Terminal(); got != c.want {
			t.Errorf("%s.Terminal() = %v, want %v", c.status, got, c.want)
		}
	}
}

// TestTask_LabelAccessors verifies the Role(), Branch(), and IsCritical()
// accessors read from the Labels map correctly, including the nil-map case.
//
// Covers: BVV-AI-02 (role routing), BVV-S-01 (branch scoping),
// BVV-ERR-03/04 (criticality classification).
// Regression: IsCritical() must use string(Critical), not "true".
func TestTask_LabelAccessors(t *testing.T) {
	t.Run("populated labels", func(t *testing.T) {
		task := &Task{Labels: map[string]string{
			LabelRole:        "builder",
			LabelBranch:      "feat/login",
			LabelCriticality: string(Critical),
		}}
		if got := task.Role(); got != "builder" {
			t.Errorf("Role() = %q, want %q", got, "builder")
		}
		if got := task.Branch(); got != "feat/login" {
			t.Errorf("Branch() = %q, want %q", got, "feat/login")
		}
		if !task.IsCritical() {
			t.Error("IsCritical() = false, want true for criticality=critical")
		}
	})

	t.Run("non-critical task", func(t *testing.T) {
		task := &Task{Labels: map[string]string{
			LabelCriticality: string(NonCritical),
		}}
		if task.IsCritical() {
			t.Error("IsCritical() = true, want false for criticality=non_critical")
		}
	})

	t.Run("missing criticality key", func(t *testing.T) {
		task := &Task{Labels: map[string]string{LabelRole: "builder"}}
		if task.IsCritical() {
			t.Error("IsCritical() = true, want false when criticality key absent")
		}
	})

	t.Run("nil labels", func(t *testing.T) {
		task := &Task{} // Labels is nil
		if got := task.Role(); got != "" {
			t.Errorf("Role() on nil Labels = %q, want empty", got)
		}
		if got := task.Branch(); got != "" {
			t.Errorf("Branch() on nil Labels = %q, want empty", got)
		}
		if task.IsCritical() {
			t.Error("IsCritical() on nil Labels = true, want false")
		}
	})
}

// TestAgentOutcome_String verifies that all four BVV outcomes produce stable,
// human-readable strings suitable for JSONL event serialization.
//
// Covers: BVV-DSP-03 (exit 0), BVV-ERR-01 (exit 1), BVV-ERR-04a (exit 2),
// BVV-DSP-14 / BVV-L-04 (exit 3).
func TestAgentOutcome_String(t *testing.T) {
	cases := []struct {
		outcome AgentOutcome
		want    string
	}{
		{OutcomeSuccess, "success"},
		{OutcomeFailure, "failure"},
		{OutcomeBlocked, "blocked"},
		{OutcomeHandoff, "handoff"},
		{AgentOutcome(""), ""},             // zero value
		{AgentOutcome("custom"), "custom"}, // unknown value passthrough
	}
	for _, c := range cases {
		if got := c.outcome.String(); got != c.want {
			t.Errorf("%#v.String() = %q, want %q", c.outcome, got, c.want)
		}
	}
}

// TestAgentOutcome_ExitCodeMapping documents the canonical exit-code-to-outcome
// mapping as a table test. Phase 3's dispatcher will implement the actual mapping;
// this test pins the enum values that mapping must target.
//
// Covers: BVV spec §8.3 exit code protocol.
// See also: BVVTaskMachine.tla (Assign, SessionComplete actions).
func TestAgentOutcome_ExitCodeMapping(t *testing.T) {
	exitCodeToOutcome := map[int]AgentOutcome{
		0: OutcomeSuccess,
		1: OutcomeFailure,
		2: OutcomeBlocked,
		3: OutcomeHandoff,
	}
	// Verify all four exit codes map to distinct outcomes.
	seen := make(map[AgentOutcome]int)
	for code, outcome := range exitCodeToOutcome {
		if prev, dup := seen[outcome]; dup {
			t.Errorf("exit code %d and %d both map to %q", prev, code, outcome)
		}
		seen[outcome] = code
	}
	if len(seen) != 4 {
		t.Errorf("expected 4 distinct outcomes, got %d", len(seen))
	}
}
