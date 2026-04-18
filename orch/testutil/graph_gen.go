package testutil

import (
	"fmt"

	"github.com/endgame/wonka-factory/orch"
	"pgregory.net/rapid"
)

// RandomDAG generates a random acyclic task graph for property-based testing.
// Tasks have random roles from {builder, verifier}, random criticality, and
// edges only from higher to lower index (guarantees acyclicity).
//
// The generated graph has 2-20 tasks, all on the given branch.
// Returns tasks in creation order (topological order by construction).
func RandomDAG(t *rapid.T, store orch.Store, branch string) []*orch.Task {
	n := rapid.IntRange(2, 20).Draw(t, "numTasks")
	// Typed Role slice prevents this generator from fabricating role values
	// outside the closed set — exactly the property TG-07 relies on. Using
	// bare strings here would let a typo slip past the compile-time closed
	// set ({RolePlanner, ...}) the orchestrator enforces elsewhere.
	roles := []orch.Role{orch.RoleBuilder, orch.RoleVerifier}
	crits := []string{string(orch.Critical), string(orch.NonCritical)}

	tasks := make([]*orch.Task, n)
	for i := 0; i < n; i++ {
		role := rapid.SampledFrom(roles).Draw(t, fmt.Sprintf("role_%d", i))
		crit := rapid.SampledFrom(crits).Draw(t, fmt.Sprintf("crit_%d", i))

		task := &orch.Task{
			ID:       fmt.Sprintf("g-%d", i),
			Title:    fmt.Sprintf("gen task %d", i),
			Status:   orch.StatusOpen,
			Priority: rapid.IntRange(0, 5).Draw(t, fmt.Sprintf("prio_%d", i)),
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        string(role),
				orch.LabelCriticality: crit,
			},
		}
		if err := store.CreateTask(task); err != nil {
			t.Fatalf("create task g-%d: %v", i, err)
		}
		tasks[i] = task
	}

	// Add random edges — only from higher index to lower (acyclic by construction).
	for i := 1; i < n; i++ {
		// Each task has a chance of depending on earlier tasks.
		numDeps := rapid.IntRange(0, min(i, 3)).Draw(t, fmt.Sprintf("ndeps_%d", i))
		for d := 0; d < numDeps; d++ {
			depIdx := rapid.IntRange(0, i-1).Draw(t, fmt.Sprintf("dep_%d_%d", i, d))
			// Best-effort — AddDep is idempotent.
			_ = store.AddDep(tasks[i].ID, tasks[depIdx].ID)
		}
	}

	return tasks
}
