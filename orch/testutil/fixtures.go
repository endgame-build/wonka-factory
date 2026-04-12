package testutil

import (
	"fmt"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/require"
)

// LinearGraph creates a chain of n tasks: t-0 → t-1 → ... → t-(n-1).
// Each task depends on the previous one. All tasks share the given branch and role.
func LinearGraph(t *testing.T, store orch.Store, branch, role string, n int) []*orch.Task {
	t.Helper()
	tasks := make([]*orch.Task, n)
	for i := 0; i < n; i++ {
		task := &orch.Task{
			ID:       fmt.Sprintf("t-%d", i),
			Title:    fmt.Sprintf("task %d", i),
			Status:   orch.StatusOpen,
			Priority: i,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        role,
				orch.LabelCriticality: string(orch.NonCritical),
			},
		}
		require.NoError(t, store.CreateTask(task))
		if i > 0 {
			require.NoError(t, store.AddDep(task.ID, tasks[i-1].ID))
		}
		tasks[i] = task
	}
	return tasks
}

// DiamondGraph creates A → {B, C} → D. All tasks share branch and role.
func DiamondGraph(t *testing.T, store orch.Store, branch, role string) []*orch.Task {
	t.Helper()
	ids := []string{"A", "B", "C", "D"}
	tasks := make([]*orch.Task, len(ids))
	for i, id := range ids {
		tasks[i] = &orch.Task{
			ID:       id,
			Title:    "task " + id,
			Status:   orch.StatusOpen,
			Priority: i,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        role,
				orch.LabelCriticality: string(orch.NonCritical),
			},
		}
		require.NoError(t, store.CreateTask(tasks[i]))
	}
	// B depends on A, C depends on A.
	require.NoError(t, store.AddDep("B", "A"))
	require.NoError(t, store.AddDep("C", "A"))
	// D depends on B and C.
	require.NoError(t, store.AddDep("D", "B"))
	require.NoError(t, store.AddDep("D", "C"))
	return tasks
}

// ParallelGraph creates n independent tasks with no dependencies.
func ParallelGraph(t *testing.T, store orch.Store, branch, role string, n int) []*orch.Task {
	t.Helper()
	tasks := make([]*orch.Task, n)
	for i := 0; i < n; i++ {
		tasks[i] = &orch.Task{
			ID:       fmt.Sprintf("p-%d", i),
			Title:    fmt.Sprintf("parallel task %d", i),
			Status:   orch.StatusOpen,
			Priority: 0,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        role,
				orch.LabelCriticality: string(orch.NonCritical),
			},
		}
		require.NoError(t, store.CreateTask(tasks[i]))
	}
	return tasks
}

// MixedCriticalityGraph creates 3 independent tasks: first is critical,
// the other two are non-critical.
func MixedCriticalityGraph(t *testing.T, store orch.Store, branch, role string) []*orch.Task {
	t.Helper()
	tasks := make([]*orch.Task, 3)
	for i := 0; i < 3; i++ {
		crit := string(orch.NonCritical)
		if i == 0 {
			crit = string(orch.Critical)
		}
		tasks[i] = &orch.Task{
			ID:       fmt.Sprintf("m-%d", i),
			Title:    fmt.Sprintf("mixed task %d", i),
			Status:   orch.StatusOpen,
			Priority: 0,
			Labels: map[string]string{
				orch.LabelBranch:      branch,
				orch.LabelRole:        role,
				orch.LabelCriticality: crit,
			},
		}
		require.NoError(t, store.CreateTask(tasks[i]))
	}
	return tasks
}
