package orch_test

import (
	"os"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/endgame/facet-scan/orch/testutil"
	"pgregory.net/rapid"
)

// newPropStore creates a Store in dir using the default backend (Beads with FS
// fallback). For use inside rapid.Check closures where *testing.T is unavailable.
func newPropStore(dir string) (orch.Store, error) {
	return orch.NewStore("", dir)
}

func expandToStore(t *testing.T, p *orch.Pipeline) orch.Store {
	t.Helper()
	store := newTestStoreInDir(t, t.TempDir())
	if err := orch.Expand(p, store); err != nil {
		t.Fatalf("expand: %v", err)
	}
	return store
}

func collectAllTasks(t *testing.T, store orch.Store, rootID string) []*orch.Task {
	t.Helper()
	tasks, err := collectAllTasksE(store, rootID)
	if err != nil {
		t.Fatalf("collectAllTasks: %v", err)
	}
	return tasks
}

// collectAllTasksFromStore is safe to call from rapid.Check (no *testing.T needed).
func collectAllTasksFromStore(store orch.Store, rootID string) []*orch.Task {
	tasks, _ := collectAllTasksE(store, rootID)
	return tasks
}

func collectAllTasksE(store orch.Store, rootID string) ([]*orch.Task, error) {
	root, err := store.GetTask(rootID)
	if err != nil {
		return nil, err
	}
	var all []*orch.Task
	var walk func(task *orch.Task)
	walk = func(task *orch.Task) {
		all = append(all, task)
		children, _ := store.GetChildren(task.ID)
		for _, c := range children {
			walk(c)
		}
	}
	walk(root)
	return all, nil
}

// TestProp_ExpansionProducesDAG verifies that expansion never creates cycles.
func TestProp_ExpansionProducesDAG(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := testutil.RandomWellFormedPipeline(t)
		dir, err := os.MkdirTemp("", "dag-test-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)

		store, err := newPropStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := orch.Expand(p, store); err != nil {
			t.Fatal(err)
		}

		// Verify no cycles: DFS from every node with shared visited + recursion stack.
		tasks := collectAllTasksFromStore(store, p.ID)
		visited := make(map[string]bool)
		for _, task := range tasks {
			recStack := make(map[string]bool)
			if hasCycle(store, task.ID, visited, recStack) {
				t.Fatalf("cycle detected involving task %q", task.ID)
			}
		}
	})
}

// hasCycle detects cycles via DFS with a recursion stack.
// `visited` tracks globally processed nodes; `recStack` tracks the current DFS path.
func hasCycle(store orch.Store, taskID string, visited, recStack map[string]bool) bool {
	if recStack[taskID] {
		return true // back-edge: cycle found
	}
	if visited[taskID] {
		return false // already fully explored, no cycle through here
	}
	visited[taskID] = true
	recStack[taskID] = true
	deps, _ := store.GetDeps(taskID)
	for _, dep := range deps {
		if hasCycle(store, dep, visited, recStack) {
			return true
		}
	}
	recStack[taskID] = false
	return false
}

// TestProp_ConsensusStructure verifies each consensus agent has exactly
// N instances + 1 merge + 1 verify.
func TestProp_ConsensusStructure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := testutil.RandomWellFormedPipeline(t)
		dir, err := os.MkdirTemp("", "cons-test-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)

		store, err := newPropStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := orch.Expand(p, store); err != nil {
			t.Fatal(err)
		}

		for _, ph := range p.Phases {
			if ph.Topology != orch.Consensus {
				continue
			}
			for _, agent := range ph.Agents {
				phaseID := p.ID + ":" + ph.ID
				children, _ := store.GetChildren(phaseID)

				instances := 0
				merges := 0
				verifies := 0
				for _, c := range children {
					if c.AgentID != agent.ID && c.AgentID != ph.Consensus.MergeAgent && c.AgentID != ph.Consensus.VerifyAgent {
						continue
					}
					switch c.Type { //nolint:exhaustive // only consensus types relevant
					case orch.TypeConsensusInstance:
						if c.AgentID == agent.ID {
							instances++
						}
					case orch.TypeConsensusMerge:
						if c.ID == agent.ID+"_merge" {
							merges++
						}
					case orch.TypeConsensusVerify:
						if c.ID == agent.ID+"_verify" {
							verifies++
						}
					}
				}
				if instances != ph.Consensus.InstanceCount {
					t.Fatalf("agent %q: expected %d instances, got %d", agent.ID, ph.Consensus.InstanceCount, instances)
				}
				if merges != 1 {
					t.Fatalf("agent %q: expected 1 merge, got %d", agent.ID, merges)
				}
				if verifies != 1 {
					t.Fatalf("agent %q: expected 1 verify, got %d", agent.ID, verifies)
				}
			}
		}
	})
}

// TestProp_OutputUniqueness verifies no two leaf tasks share an output path.
func TestProp_OutputUniqueness(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := testutil.RandomWellFormedPipeline(t)
		dir, err := os.MkdirTemp("", "uniq-test-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)

		store, err := newPropStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := orch.Expand(p, store); err != nil {
			t.Fatal(err)
		}

		tasks := collectAllTasksFromStore(store, p.ID)
		seen := make(map[string]string)
		for _, task := range tasks {
			if task.Output == "" {
				continue
			}
			// Merge and verify may share the agent's output path — that's by design.
			// Only check instance tasks for uniqueness among themselves.
			if task.Type == orch.TypeConsensusInstance {
				if prev, ok := seen[task.Output]; ok {
					t.Fatalf("duplicate instance output %q: tasks %q and %q", task.Output, prev, task.ID)
				}
				seen[task.Output] = task.ID
			}
		}
	})
}

// TestProp_ExpansionDeterminism verifies that expanding the same pipeline twice
// produces identical task graphs.
func TestProp_ExpansionDeterminism(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := testutil.RandomWellFormedPipeline(t)

		dir1, _ := os.MkdirTemp("", "det1-*")
		defer os.RemoveAll(dir1)
		dir2, _ := os.MkdirTemp("", "det2-*")
		defer os.RemoveAll(dir2)

		store1, err1 := newPropStore(dir1)
		if err1 != nil {
			t.Fatal(err1)
		}
		defer store1.Close()
		store2, err2 := newPropStore(dir2)
		if err2 != nil {
			t.Fatal(err2)
		}
		defer store2.Close()

		if err := orch.Expand(p, store1); err != nil {
			t.Fatal(err)
		}
		if err := orch.Expand(p, store2); err != nil {
			t.Fatal(err)
		}

		tasks1 := collectAllTasksFromStore(store1, p.ID)
		tasks2 := collectAllTasksFromStore(store2, p.ID)

		if len(tasks1) != len(tasks2) {
			t.Fatalf("determinism: task count %d != %d", len(tasks1), len(tasks2))
		}

		for i := range tasks1 {
			if tasks1[i].ID != tasks2[i].ID {
				t.Fatalf("determinism: task[%d] ID %q != %q", i, tasks1[i].ID, tasks2[i].ID)
			}
			if tasks1[i].Type != tasks2[i].Type {
				t.Fatalf("determinism: task[%d] type %q != %q", i, tasks1[i].Type, tasks2[i].Type)
			}
			deps1, _ := store1.GetDeps(tasks1[i].ID)
			deps2, _ := store2.GetDeps(tasks2[i].ID)
			if len(deps1) != len(deps2) {
				t.Fatalf("determinism: task %q dep count %d != %d", tasks1[i].ID, len(deps1), len(deps2))
			}
		}
	})
}

// TestProp_PhaseChaining verifies phase tasks form a linear dependency chain.
func TestProp_PhaseChaining(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := testutil.RandomWellFormedPipeline(t)
		dir, _ := os.MkdirTemp("", "chain-test-*")
		defer os.RemoveAll(dir)

		store, storeErr := newPropStore(dir)
		if storeErr != nil {
			t.Fatal(storeErr)
		}
		defer store.Close()
		if err := orch.Expand(p, store); err != nil {
			t.Fatal(err)
		}

		for i := 1; i < len(p.Phases); i++ {
			phaseID := p.ID + ":" + p.Phases[i].ID
			prevID := p.ID + ":" + p.Phases[i-1].ID
			deps, _ := store.GetDeps(phaseID)
			found := false
			for _, d := range deps {
				if d == prevID {
					found = true
				}
			}
			if !found {
				t.Fatalf("phase %q does not depend on %q", phaseID, prevID)
			}
		}
	})
}

// TestProp_TaskCountFormula verifies the task count matches the expected formula:
// 1 (root) + |phases| + sum(agent_tasks_per_phase)
// where agent_tasks = |agents| for seq/par, |agents|*(instances+2) for consensus.
func TestProp_TaskCountFormula(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := testutil.RandomWellFormedPipeline(t)
		dir, err := os.MkdirTemp("", "count-test-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)

		store, err := newPropStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := orch.Expand(p, store); err != nil {
			t.Fatal(err)
		}

		expected := 1 + len(p.Phases) // root + phases
		for _, ph := range p.Phases {
			switch ph.Topology {
			case orch.Sequential, orch.Parallel:
				expected += len(ph.Agents)
			case orch.Consensus:
				expected += len(ph.Agents) * (ph.Consensus.InstanceCount + 2) // instances + merge + verify
			}
		}

		tasks := collectAllTasksFromStore(store, p.ID)
		if len(tasks) != expected {
			t.Fatalf("task count: got %d, expected %d (formula)", len(tasks), expected)
		}
	})
}

// TestProp_WFCRejection verifies that each type of WFC violation is caught.
func TestProp_WFCRejection(t *testing.T) {
	violations := []string{"WFC-01", "WFC-03", "WFC-04", "WFC-06", "WFC-07", "WFC-08", "WFC-10", "WFC-11"}

	for _, v := range violations {
		v := v
		t.Run(v, func(t *testing.T) {
			base := testutil.MiniPipeline()
			mutated := testutil.MutateWFC(base, v)
			err := orch.ValidateWFC(mutated)
			if err == nil {
				t.Fatalf("expected WFC violation %s but validation passed", v)
			}
		})
	}
}
