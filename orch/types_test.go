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
