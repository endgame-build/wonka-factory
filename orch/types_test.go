//go:build verify

package orch

import "testing"

// TestTaskStatus_Terminal verifies that all terminal statuses — including
// the BVV-added StatusBlocked — report Terminal() == true, and that
// non-terminal statuses report false.
//
// Covers: BVV spec §5.1a (task status enum).
// Prerequisite for: BVV-S-02 (terminal irreversibility) — a status must be
// classifiable as terminal for the safety invariant to be checkable at
// runtime by invariant.go's AssertTerminalIrreversibility (Phase 7).
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
		{StatusBlocked, true}, // NEW in BVV per §5.1a
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
// Regression: IsCritical() must use string(Critical), not "true" (fix 5082f32).
func TestTask_LabelAccessors(t *testing.T) {
	t.Run("populated labels", func(t *testing.T) {
		task := &Task{Labels: map[string]string{
			"role":        "builder",
			"branch":      "feat/login",
			"criticality": string(Critical),
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
			"criticality": string(NonCritical),
		}}
		if task.IsCritical() {
			t.Error("IsCritical() = true, want false for criticality=non_critical")
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
