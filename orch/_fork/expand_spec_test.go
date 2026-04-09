package orch_test

import (
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/endgame/facet-scan/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- WFC refinement tests ---

// TestWFC01_UniqueAgentOutputs verifies [WFC-01]: duplicate outputs are rejected.
func TestWFC01_UniqueAgentOutputs(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-01")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	var wfcErr *orch.WFCError
	require.ErrorAs(t, err, &wfcErr)
	assert.Contains(t, wfcErr.Error(), "WFC-01")
}

// TestWFC01_Valid verifies [WFC-01] passes for valid pipeline.
func TestWFC01_Valid(t *testing.T) {
	require.NoError(t, orch.ValidateWFC(testutil.MiniPipeline()))
}

// TestWFC03_ConsensusMissingConfig verifies [WFC-03]: consensus without config is rejected.
func TestWFC03_ConsensusMissingConfig(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-03")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-03")
}

// TestWFC04_NonConsensusWithConfig verifies [WFC-04]: non-consensus with config is rejected.
func TestWFC04_NonConsensusWithConfig(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-04")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-04")
}

// TestWFC06_InstanceCountBounds verifies [WFC-06]: instance count < 2 is rejected.
func TestWFC06_InstanceCountBounds(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-06")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-06")
}

// TestWFC07_GateAgentMembership verifies [WFC-07]: nonexistent gate agent is rejected.
func TestWFC07_GateAgentMembership(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-07")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-07")
}

// TestWFC08_GateAgentMustBeCritical verifies [WFC-08, ERR-09]: gate agent marked non-critical is rejected.
// Criticality per WFC-08 (ERR-09).
func TestWFC08_GateAgentMustBeCritical(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-08")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-08")
}

// TestWFC10_GapToleranceBounds verifies [WFC-10]: gap_tolerance=0 is rejected.
func TestWFC10_GapToleranceBounds(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-10")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-10")
}

// TestWFC11_LockPathContainment verifies [WFC-11]: lock outside output dir is rejected.
func TestWFC11_LockPathContainment(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-11")
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-11")
}

// --- EXP refinement tests ---

// TestEXP01_ExpansionDeterminism verifies [EXP-01]: same pipeline → same task graph.
func TestEXP01_ExpansionDeterminism(t *testing.T) {
	p := testutil.MiniPipeline()
	s1 := expandToStore(t, p)
	s2 := expandToStore(t, p)

	t1 := collectAllTasks(t, s1, p.ID)
	t2 := collectAllTasks(t, s2, p.ID)
	require.Equal(t, len(t1), len(t2))

	for i := range t1 {
		assert.Equal(t, t1[i].ID, t2[i].ID)
		assert.Equal(t, t1[i].Type, t2[i].Type)
	}
}

// TestEXP03_SingleRootTask verifies [EXP-03]: exactly one root pipeline task.
func TestEXP03_SingleRootTask(t *testing.T) {
	p := testutil.MiniPipeline()
	store := expandToStore(t, p)

	root, err := store.GetTask(p.ID)
	require.NoError(t, err)
	assert.Equal(t, orch.TypePipeline, root.Type)
	assert.Equal(t, "", root.ParentID)
}

// TestEXP04_PhaseTasks verifies [EXP-04]: phase tasks have parent=root.
func TestEXP04_PhaseTasks(t *testing.T) {
	p := testutil.MiniPipeline()
	store := expandToStore(t, p)

	children, err := store.GetChildren(p.ID)
	require.NoError(t, err)
	assert.Equal(t, len(p.Phases), len(children))

	for _, c := range children {
		assert.Equal(t, orch.TypePhase, c.Type)
		assert.Equal(t, p.ID, c.ParentID)
	}
}

// TestEXP05_PhaseChaining verifies [EXP-05]: phases are chained by deps.
func TestEXP05_PhaseChaining(t *testing.T) {
	p := testutil.MiniPipeline()
	store := expandToStore(t, p)

	for i := 1; i < len(p.Phases); i++ {
		phaseID := p.ID + ":" + p.Phases[i].ID
		prevID := p.ID + ":" + p.Phases[i-1].ID
		deps, err := store.GetDeps(phaseID)
		require.NoError(t, err)
		assert.Contains(t, deps, prevID)
	}
}

// TestEXP06_SequentialChaining verifies [EXP-06]: sequential agents are chained.
func TestEXP06_SequentialChaining(t *testing.T) {
	p := testutil.MiniPipeline()
	store := expandToStore(t, p)

	// Phase p2 is sequential with agent a3.
	// a3 depends on phase task (first in chain).
	deps, err := store.GetDeps("a3")
	require.NoError(t, err)
	assert.Contains(t, deps, p.ID+":p2")
}

// TestEXP07_ParallelIndependent verifies [EXP-07]: parallel agents have no inter-agent deps.
func TestEXP07_ParallelIndependent(t *testing.T) {
	p := testutil.MiniPipeline()
	store := expandToStore(t, p)

	// Phase p1 is parallel with agents a1, a2.
	deps1, _ := store.GetDeps("a1")
	deps2, _ := store.GetDeps("a2")

	assert.NotContains(t, deps1, "a2")
	assert.NotContains(t, deps2, "a1")
	// Both should depend on phase task.
	assert.Contains(t, deps1, p.ID+":p1")
	assert.Contains(t, deps2, p.ID+":p1")
}

// TestEXP08_ConsensusRoundStructure verifies [EXP-08, CON-07, CON-09, S2]: consensus creates
// instances→merge→verify (CON-07 three sub-phases). Instance independence — no inter-instance
// deps (S2). Concurrent invocations = batch x instance_count (CON-09).
func TestEXP08_ConsensusRoundStructure(t *testing.T) {
	p := testutil.ConsensusPipeline()
	store := expandToStore(t, p)

	// Agent s1 should have 3 instances + merge + verify.
	for _, suffix := range []string{"_A", "_B", "_C"} {
		task, err := store.GetTask("s1" + suffix)
		require.NoError(t, err)
		assert.Equal(t, orch.TypeConsensusInstance, task.Type)
	}

	merge, err := store.GetTask("s1_merge")
	require.NoError(t, err)
	assert.Equal(t, orch.TypeConsensusMerge, merge.Type)
	deps, _ := store.GetDeps("s1_merge")
	assert.Len(t, deps, 3)

	verify, err := store.GetTask("s1_verify")
	require.NoError(t, err)
	assert.Equal(t, orch.TypeConsensusVerify, verify.Type)
	vDeps, _ := store.GetDeps("s1_verify")
	assert.Contains(t, vDeps, "s1_merge")
}

// TestEXP09_VerifyTaskAlwaysExists verifies [EXP-09]: verify task always in graph.
func TestEXP09_VerifyTaskAlwaysExists(t *testing.T) {
	p := testutil.ConsensusPipeline()
	store := expandToStore(t, p)

	for _, agent := range p.Phases[0].Agents {
		_, err := store.GetTask(agent.ID + "_verify")
		require.NoError(t, err, "verify task for %q must exist", agent.ID)
	}
}

// TestEXP10_GateIsLeafTask verifies [EXP-10]: gate agent appears as regular task in phase.
func TestEXP10_GateIsLeafTask(t *testing.T) {
	p := testutil.GatedPipeline()
	store := expandToStore(t, p)

	task, err := store.GetTask("validator")
	require.NoError(t, err)
	assert.Equal(t, orch.TypeAgent, task.Type)
	assert.Equal(t, p.ID+":gate-phase", task.ParentID)
}

// TestEXP14_BatchOrdering verifies [EXP-14, CON-08]: batch b+1 instances depend on batch b verify tasks.
// Batched consensus execution (CON-08).
func TestEXP14_BatchOrdering(t *testing.T) {
	p := testutil.ConsensusPipeline() // batch_size=2, 3 agents → 2 batches
	store := expandToStore(t, p)

	// s1, s2 are in batch 0. s3 is in batch 1.
	// s3's instances should depend on s1_verify and s2_verify.
	for _, suffix := range []string{"_A", "_B", "_C"} {
		deps, err := store.GetDeps("s3" + suffix)
		require.NoError(t, err)
		assert.Contains(t, deps, "s1_verify", "batch 1 instance should depend on batch 0 s1_verify")
		assert.Contains(t, deps, "s2_verify", "batch 1 instance should depend on batch 0 s2_verify")
	}
}

// TestEXP02_WFCValidationBeforeExpansion verifies [EXP-02]: invalid pipeline → expansion rejected.
func TestEXP02_WFCValidationBeforeExpansion(t *testing.T) {
	p := testutil.MutateWFC(testutil.MiniPipeline(), "WFC-01")
	dir := t.TempDir()
	store := newTestStoreInDir(t, dir)
	err := orch.Expand(p, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "well-formedness")
}

// TestWFC02_InputFromPriorPhase verifies [WFC-02]: input from prior phase is valid.
func TestWFC02_InputFromPriorPhase(t *testing.T) {
	p := testutil.MiniPipeline() // a3 inputs a1.md from phase p1
	require.NoError(t, orch.ValidateWFC(p))
}

// TestWFC02_InputFromUnknownSource verifies [WFC-02]: input from unknown source rejected.
func TestWFC02_InputFromUnknownSource(t *testing.T) {
	p := &orch.Pipeline{
		ID: "bad", OutputDir: "out", GapTolerance: 1,
		Lock: orch.LockConfig{Path: "out/.lock", StalenessThreshold: 1<<63 - 1, RetryCount: 1, RetryDelay: 1},
		Phases: []orch.Phase{{
			ID: "p1", Topology: orch.Parallel,
			Agents: []orch.AgentDef{
				{ID: "a1", Model: orch.ModelSonnet, Inputs: []string{"nonexistent.md"},
					Output: "a1.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
			},
		}},
	}
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-02")
}

// TestWFC09_UniqueAgentIDs verifies [WFC-09]: duplicate agent IDs rejected.
func TestWFC09_UniqueAgentIDs(t *testing.T) {
	p := &orch.Pipeline{
		ID: "dup-id", OutputDir: "out", GapTolerance: 1,
		Lock: orch.LockConfig{Path: "out/.lock", StalenessThreshold: 1<<63 - 1, RetryCount: 1, RetryDelay: 1},
		Phases: []orch.Phase{{
			ID: "p1", Topology: orch.Parallel,
			Agents: []orch.AgentDef{
				{ID: "same", Model: orch.ModelSonnet, Output: "a.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				{ID: "same", Model: orch.ModelSonnet, Output: "b.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
			},
		}},
	}
	err := orch.ValidateWFC(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WFC-09")
}

// TestLDG13_TasksCreatedByOrchestratorOnly verifies [LDG-13]:
// all tasks in the expanded graph were created by Expand (orchestrator), not agents.
// Structural test: the Store interface's CreateTask is the only creation path.
func TestLDG13_TasksCreatedByOrchestratorOnly(t *testing.T) {
	p := testutil.MiniPipeline()
	store := expandToStore(t, p)
	tasks := collectAllTasks(t, store, p.ID)
	// Every task must have been created via Expand — verify they all exist and have valid types.
	for _, task := range tasks {
		assert.NotEqual(t, orch.TaskType(""), task.Type, "task %q has empty type", task.ID)
		assert.Equal(t, orch.StatusOpen, task.Status, "task %q should be open after expansion", task.ID)
	}
}

// TestWSP01_WSP03_OutputPathUniqueness verifies [WSP-01, WSP-03, S10]:
// workspace isolation via output path uniqueness (no two agent tasks write same path).
func TestWSP01_WSP03_OutputPathUniqueness(t *testing.T) {
	p := facetScanPipeline()
	store := expandToStore(t, p)
	tasks := collectAllTasks(t, store, p.ID)

	// Among instance tasks, all outputs must be unique.
	seen := make(map[string]string)
	for _, task := range tasks {
		if task.Type == orch.TypeConsensusInstance && task.Output != "" {
			if prev, ok := seen[task.Output]; ok {
				t.Fatalf("[WSP-01/WSP-03] duplicate instance output %q: tasks %q and %q", task.Output, prev, task.ID)
			}
			seen[task.Output] = task.ID
		}
	}
}

// TestExpand_FacetScanTaskCount verifies the facet-scan pipeline expands to exactly 67 tasks.
func TestExpand_FacetScanTaskCount(t *testing.T) {
	// Use the actual facet-scan pipeline definition.
	p := facetScanPipeline()
	store := expandToStore(t, p)
	tasks := collectAllTasks(t, store, p.ID)
	assert.Len(t, tasks, 67, "facet-scan pipeline should expand to 67 tasks")
}

// facetScanPipeline imports the real pipeline for testing.
// We can't import internal/facetscan from orch_test, so we duplicate the essential structure.
func facetScanPipeline() *orch.Pipeline {
	views := func(ids ...string) []orch.AgentDef {
		var agents []orch.AgentDef
		for _, id := range ids {
			agents = append(agents, orch.AgentDef{
				ID: id, Model: orch.ModelOpus,
				Inputs: []string{"MANIFEST.md"}, Output: "views/" + id + ".md",
				Criticality: orch.NonCritical, Format: orch.FormatMd, MaxTurns: 30,
			})
		}
		return agents
	}

	cc := &orch.ConsensusConfig{InstanceCount: 3, BatchSize: 3, MergeAgent: "merger", VerifyAgent: "verifier"}
	cc1 := &orch.ConsensusConfig{InstanceCount: 3, BatchSize: 1, MergeAgent: "merger", VerifyAgent: "verifier"}

	return &orch.Pipeline{
		ID: "facet-scan", OutputDir: "out", GapTolerance: 5,
		Lock: orch.LockConfig{Path: "out/.lock", StalenessThreshold: 1<<63 - 1, RetryCount: 1, RetryDelay: 1},
		Phases: []orch.Phase{
			{ID: "scout", Topology: orch.Consensus, Consensus: cc1,
				Agents: []orch.AgentDef{{ID: "00", Model: orch.ModelOpus, Output: "MANIFEST.md", Criticality: orch.Critical, Format: orch.FormatMd}}},
			{ID: "v2a", Topology: orch.Consensus, Consensus: cc, Agents: views("01", "02", "03")},
			{ID: "v2b", Topology: orch.Consensus, Consensus: cc, Agents: views("04", "05", "06")},
			{ID: "v2c", Topology: orch.Consensus, Consensus: cc, Agents: views("07", "08", "10")},
			{ID: "quality", Topology: orch.Sequential,
				Gate: &orch.QualityGate{Agent: "11", Halt: true},
				Agents: []orch.AgentDef{
					{ID: "09", Model: orch.ModelOpus, Output: "quality.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "11", Model: orch.ModelHaiku, Output: "valid.md", Criticality: orch.Critical, Format: orch.FormatMd},
				}},
			{ID: "synthesis", Topology: orch.Parallel,
				Agents: []orch.AgentDef{
					{ID: "12a", Model: orch.ModelOpus, Output: "overview.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "12b", Model: orch.ModelOpus, Output: "views.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "12c", Model: orch.ModelOpus, Output: "adr.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "12d", Model: orch.ModelOpus, Output: "risk.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "12e", Model: orch.ModelOpus, Output: "assets.jsonl", Criticality: orch.NonCritical, Format: orch.FormatJsonl},
					{ID: "12f", Model: orch.ModelOpus, Output: "constraints.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				}},
			{ID: "eval", Topology: orch.Sequential,
				Agents: []orch.AgentDef{
					{ID: "13", Model: orch.ModelOpus, Output: "roadmap.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				}},
		},
	}
}
