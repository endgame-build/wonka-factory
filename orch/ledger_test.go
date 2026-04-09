//go:build verify

package orch

import "testing"

// TestTaskLess_Ordering verifies the canonical sort: priority ASC, then ID ASC.
// LDG-07: deterministic tiebreaker for equal-priority ready tasks.
func TestTaskLess_Ordering(t *testing.T) {
	cases := []struct {
		name string
		a, b *Task
		want bool // taskLess(a, b)
	}{
		{"lower priority first", &Task{ID: "b", Priority: 1}, &Task{ID: "a", Priority: 2}, true},
		{"higher priority second", &Task{ID: "a", Priority: 2}, &Task{ID: "b", Priority: 1}, false},
		{"same priority, id tiebreak", &Task{ID: "a", Priority: 1}, &Task{ID: "b", Priority: 1}, true},
		{"same priority, reverse id", &Task{ID: "b", Priority: 1}, &Task{ID: "a", Priority: 1}, false},
		{"equal", &Task{ID: "a", Priority: 1}, &Task{ID: "a", Priority: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskLess(c.a, c.b); got != c.want {
				t.Errorf("taskLess(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestValidateLabelFilters verifies malformed label strings are rejected.
func TestValidateLabelFilters(t *testing.T) {
	// Valid filters.
	if err := validateLabelFilters([]string{"branch:main", "role:builder"}); err != nil {
		t.Errorf("valid filters rejected: %v", err)
	}
	if err := validateLabelFilters(nil); err != nil {
		t.Errorf("nil filters rejected: %v", err)
	}

	// Malformed filter (no colon).
	if err := validateLabelFilters([]string{"branch:main", "novalue"}); err == nil {
		t.Error("malformed filter accepted, want error")
	}
}

// TestLabelsMatch verifies the AND-match semantics.
func TestLabelsMatch(t *testing.T) {
	task := &Task{Labels: map[string]string{
		"branch": "feat/x",
		"role":   "builder",
	}}

	if !labelsMatch(task, nil) {
		t.Error("nil filters should match everything")
	}
	if !labelsMatch(task, []string{"branch:feat/x"}) {
		t.Error("single matching filter should match")
	}
	if !labelsMatch(task, []string{"branch:feat/x", "role:builder"}) {
		t.Error("all matching filters should match")
	}
	if labelsMatch(task, []string{"branch:feat/x", "role:verifier"}) {
		t.Error("mismatched filter should not match")
	}
	if labelsMatch(task, []string{"missing:key"}) {
		t.Error("absent label should not match")
	}

	// Nil labels map.
	nilTask := &Task{}
	if labelsMatch(nilTask, []string{"branch:feat/x"}) {
		t.Error("nil Labels should not match any filter")
	}
}
