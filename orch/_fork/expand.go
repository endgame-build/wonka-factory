package orch

import (
	"fmt"
	"path/filepath"
	"strings"
)

// --- Well-Formedness Validation ---

// WFCViolation describes a single well-formedness constraint violation.
type WFCViolation struct {
	Constraint string // e.g., "WFC-01"
	Message    string
}

// WFCError contains one or more well-formedness constraint violations.
type WFCError struct {
	Violations []WFCViolation
}

func (e *WFCError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d well-formedness violations:", len(e.Violations))
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "\n  [%s] %s", v.Constraint, v.Message)
	}
	return b.String()
}

// allAgents returns all agents across all phases.
func allAgents(p *Pipeline) []AgentDef {
	var agents []AgentDef
	for _, ph := range p.Phases {
		agents = append(agents, ph.Agents...)
	}
	return agents
}

// ValidateWFC checks all 12 well-formedness constraints (WFC-01..12).
// Returns nil if the pipeline is well-formed.
func ValidateWFC(p *Pipeline) error {
	var violations []WFCViolation
	add := func(id, msg string) {
		violations = append(violations, WFCViolation{Constraint: id, Message: msg})
	}

	agents := allAgents(p)

	// WFC-01: Unique agent outputs.
	outputs := make(map[string]string) // output → agent ID
	for _, a := range agents {
		if prev, ok := outputs[a.Output]; ok {
			add("WFC-01", fmt.Sprintf("duplicate output %q: agents %q and %q", a.Output, prev, a.ID))
		}
		outputs[a.Output] = a.ID
	}

	// WFC-02: Phase ordering DAG — inputs reference only outputs from prior phases
	// or, for sequential topology, from earlier agents within the same phase.
	// For parallel/consensus phases, all same-phase outputs are considered available
	// (the orchestrator uses OPS-03 to validate at invocation time).
	available := make(map[string]bool)
	for i, ph := range p.Phases {
		// Add this phase's outputs to available BEFORE checking, since intra-phase
		// references are valid (sequential: structural dependency; parallel/consensus:
		// informational, validated at runtime via OPS-03).
		for _, a := range ph.Agents {
			available[a.Output] = true
		}
		for _, a := range ph.Agents {
			for _, input := range a.Inputs {
				if !available[input] {
					add("WFC-02", fmt.Sprintf("phase %q agent %q input %q not available (phase index %d)", ph.ID, a.ID, input, i))
				}
			}
		}
	}

	// WFC-03: Consensus phases must have ConsensusConfig.
	for _, ph := range p.Phases {
		if ph.Topology == Consensus && ph.Consensus == nil {
			add("WFC-03", fmt.Sprintf("phase %q has topology=consensus but no ConsensusConfig", ph.ID))
		}
	}

	// WFC-04: Non-consensus phases must NOT have ConsensusConfig.
	for _, ph := range p.Phases {
		if ph.Topology != Consensus && ph.Consensus != nil {
			add("WFC-04", fmt.Sprintf("phase %q has topology=%s but has ConsensusConfig", ph.ID, ph.Topology))
		}
	}

	// WFC-05: Threshold ordering — skipped (thresholds not in ConsensusConfig per ZFC).

	// WFC-06: Instance count >= 2.
	for _, ph := range p.Phases {
		if ph.Consensus != nil && ph.Consensus.InstanceCount < 2 {
			add("WFC-06", fmt.Sprintf("phase %q consensus instance_count=%d, must be >= 2", ph.ID, ph.Consensus.InstanceCount))
		}
	}

	// WFC-07: Gate agent membership — gate agent must be in same phase or prior phase.
	agentsByPhaseIndex := make(map[int]map[string]bool)
	for i, ph := range p.Phases {
		agentsByPhaseIndex[i] = make(map[string]bool)
		for _, a := range ph.Agents {
			agentsByPhaseIndex[i][a.ID] = true
		}
	}
	for i, ph := range p.Phases {
		if ph.Gate == nil {
			continue
		}
		found := false
		for j := 0; j <= i; j++ {
			if agentsByPhaseIndex[j][ph.Gate.Agent] {
				found = true
				break
			}
		}
		if !found {
			add("WFC-07", fmt.Sprintf("phase %q gate agent %q not found in this or prior phases", ph.ID, ph.Gate.Agent))
		}
	}

	// WFC-08: Critical agent completeness.
	// Gate agents MUST be marked critical. Non-gate critical agents are accepted
	// as pipeline-author assertions (the orchestrator does not second-guess which
	// non-gate agents are "sole input providers" since that depends on whether
	// downstream agents can tolerate degraded input — a domain judgment call).
	gateAgents := make(map[string]bool)
	for _, ph := range p.Phases {
		if ph.Gate != nil {
			gateAgents[ph.Gate.Agent] = true
		}
	}
	for _, a := range agents {
		if gateAgents[a.ID] && a.Criticality != Critical {
			add("WFC-08", fmt.Sprintf("gate agent %q must be marked critical", a.ID))
		}
	}

	// WFC-09: Unique ID prefixes (for agents that have them — use the 2-char numeric prefix).
	// We use the agent ID itself since prefixes are embedded in IDs.
	idSet := make(map[string]bool)
	for _, a := range agents {
		if idSet[a.ID] {
			add("WFC-09", fmt.Sprintf("duplicate agent ID %q", a.ID))
		}
		idSet[a.ID] = true
	}

	// WFC-10: Gap tolerance bounds: 1 <= gap_tolerance <= count(non-critical agents).
	ncCount := 0
	for _, a := range agents {
		if a.Criticality == NonCritical {
			ncCount++
		}
	}
	if p.GapTolerance < 1 || p.GapTolerance > ncCount {
		add("WFC-10", fmt.Sprintf("gap_tolerance=%d out of bounds [1, %d] (non-critical agent count)", p.GapTolerance, ncCount))
	}

	// WFC-11: Lock path containment (within output_dir).
	if p.Lock.Path != "" && !strings.HasPrefix(p.Lock.Path, p.OutputDir) {
		add("WFC-11", fmt.Sprintf("lock path %q is not within output_dir %q", p.Lock.Path, p.OutputDir))
	}

	// WFC-12: Consensus lock staleness < pipeline lock staleness.
	// Not checked in Phase 1 — consensus lock not yet implemented.

	if len(violations) > 0 {
		return &WFCError{Violations: violations}
	}
	return nil
}

// --- Expansion ---

// Expand transforms a Pipeline definition into a task graph in the Store.
// The function is deterministic: same pipeline → same task graph (EXP-01).
func Expand(p *Pipeline, store Store) error {
	if err := ValidateWFC(p); err != nil {
		return fmt.Errorf("well-formedness validation failed: %w", err)
	}

	// EXP-03: Single root pipeline task.
	root := &Task{
		ID:       p.ID,
		Type:     TypePipeline,
		Status:   StatusOpen,
		Priority: 0,
	}
	if err := store.CreateTask(root); err != nil {
		return fmt.Errorf("create root: %w", err)
	}

	var prevPhaseID string
	for i, ph := range p.Phases {
		// EXP-04: Phase task with parent = root.
		phaseID := p.ID + ":" + ph.ID
		phaseTask := &Task{
			ID:       phaseID,
			ParentID: root.ID,
			Type:     TypePhase,
			Status:   StatusOpen,
			Priority: i,
		}
		if err := store.CreateTask(phaseTask); err != nil {
			return fmt.Errorf("create phase %q: %w", phaseID, err)
		}

		// EXP-05: Phase chaining — each phase depends on the previous.
		if prevPhaseID != "" {
			if err := store.AddDep(phaseID, prevPhaseID); err != nil {
				return fmt.Errorf("chain phase %q→%q: %w", phaseID, prevPhaseID, err)
			}
		}

		// Expand agents per topology.
		switch ph.Topology {
		case Sequential:
			if err := expandSequential(store, ph, phaseID); err != nil {
				return err
			}
		case Parallel:
			if err := expandParallel(store, ph, phaseID); err != nil {
				return err
			}
		case Consensus:
			if err := expandConsensus(store, ph, phaseID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown topology %q in phase %q", ph.Topology, ph.ID)
		}

		prevPhaseID = phaseID
	}

	return nil
}

// newAgentTask creates the common Task struct for an agent definition.
func newAgentTask(agent AgentDef, phaseID string, taskType TaskType, priority int) *Task {
	return &Task{
		ID:       agent.ID,
		ParentID: phaseID,
		Type:     taskType,
		Status:   StatusOpen,
		Priority: priority,
		AgentID:  agent.ID,
		Output:   agent.Output,
	}
}

// expandSequential creates chained agent tasks within a phase (EXP-06).
func expandSequential(store Store, ph Phase, phaseID string) error {
	var prevAgentID string
	for j, agent := range ph.Agents {
		task := newAgentTask(agent, phaseID, TypeAgent, j)
		if err := store.CreateTask(task); err != nil {
			return fmt.Errorf("create agent %q: %w", agent.ID, err)
		}
		if j == 0 {
			if err := store.AddDep(agent.ID, phaseID); err != nil {
				return err
			}
		} else {
			if err := store.AddDep(agent.ID, prevAgentID); err != nil {
				return err
			}
		}
		prevAgentID = agent.ID
	}
	return nil
}

// expandParallel creates independent agent tasks under a phase (EXP-07).
func expandParallel(store Store, ph Phase, phaseID string) error {
	for j, agent := range ph.Agents {
		task := newAgentTask(agent, phaseID, TypeAgent, j)
		if err := store.CreateTask(task); err != nil {
			return fmt.Errorf("create agent %q: %w", agent.ID, err)
		}
		if err := store.AddDep(agent.ID, phaseID); err != nil {
			return err
		}
	}
	return nil
}

// expandConsensus creates instance→merge→verify subgraphs for each agent,
// with batch ordering (EXP-08, EXP-09, EXP-14).
func expandConsensus(store Store, ph Phase, phaseID string) error {
	cc := ph.Consensus
	batchSize := cc.BatchSize
	if batchSize <= 0 {
		batchSize = len(ph.Agents)
	}

	// Partition agents into batches.
	var batches [][]AgentDef
	for i := 0; i < len(ph.Agents); i += batchSize {
		end := i + batchSize
		if end > len(ph.Agents) {
			end = len(ph.Agents)
		}
		batches = append(batches, ph.Agents[i:end])
	}

	// Track verify task IDs from previous batch for batch ordering (EXP-14).
	var prevBatchVerifyIDs []string

	for batchIdx, batch := range batches {
		var currentBatchVerifyIDs []string

		for agentIdx, agent := range batch {
			priority := batchIdx*1000 + agentIdx // keep ordering stable

			// Create N instance tasks.
			var instanceIDs []string
			for k := range cc.InstanceCount {
				suffix := string(rune('A' + k))
				instID := agent.ID + "_" + suffix
				instTask := &Task{
					ID:       instID,
					ParentID: phaseID,
					Type:     TypeConsensusInstance,
					Status:   StatusOpen,
					Priority: priority,
					AgentID:  agent.ID,
					Output:   instanceOutputPath(agent.Output, suffix),
				}
				if err := store.CreateTask(instTask); err != nil {
					return fmt.Errorf("create instance %q: %w", instID, err)
				}

				// Instances in batch 0 depend on phase task.
				// Instances in batch b>0 depend on ALL verify tasks from batch b-1.
				if batchIdx == 0 {
					if err := store.AddDep(instID, phaseID); err != nil {
						return err
					}
				} else {
					for _, prevVerifyID := range prevBatchVerifyIDs {
						if err := store.AddDep(instID, prevVerifyID); err != nil {
							return err
						}
					}
				}
				instanceIDs = append(instanceIDs, instID)
			}

			// Create merge task.
			mergeID := agent.ID + "_merge"
			mergeTask := &Task{
				ID:       mergeID,
				ParentID: phaseID,
				Type:     TypeConsensusMerge,
				Status:   StatusOpen,
				Priority: priority,
				AgentID:  cc.MergeAgent,
				Output:   agent.Output, // merge produces the final output
			}
			if err := store.CreateTask(mergeTask); err != nil {
				return fmt.Errorf("create merge %q: %w", mergeID, err)
			}
			// Merge depends on all instances.
			for _, instID := range instanceIDs {
				if err := store.AddDep(mergeID, instID); err != nil {
					return err
				}
			}

			// Create verify task (EXP-09: always exists in graph).
			verifyID := agent.ID + "_verify"
			verifyTask := &Task{
				ID:       verifyID,
				ParentID: phaseID,
				Type:     TypeConsensusVerify,
				Status:   StatusOpen,
				Priority: priority,
				AgentID:  cc.VerifyAgent,
				Output:   agent.Output, // verify confirms/adjusts the merged output
			}
			if err := store.CreateTask(verifyTask); err != nil {
				return fmt.Errorf("create verify %q: %w", verifyID, err)
			}
			// Verify depends on merge.
			if err := store.AddDep(verifyID, mergeID); err != nil {
				return err
			}

			currentBatchVerifyIDs = append(currentBatchVerifyIDs, verifyID)
		}

		prevBatchVerifyIDs = currentBatchVerifyIDs
	}

	return nil
}

// instanceOutputPath derives the instance output path from the agent's base output.
// E.g., "MANIFEST.md" + "A" → "MANIFEST_A.md"
// instanceOutputPath places consensus instance outputs in a .consensus/
// subdirectory relative to the final merged output. This keeps instance
// artefacts (temporary, intermediate) separate from the canonical deliverables.
//
// Example: "MANIFEST.md" + "A" → ".consensus/MANIFEST_A.md"
//
//	"views/01_FUNCTIONAL.md" + "B" → "views/.consensus/01_FUNCTIONAL_B.md"
func instanceOutputPath(base, suffix string) string {
	dir := filepath.Dir(base)
	file := filepath.Base(base)
	ext := filepath.Ext(file)
	name := strings.TrimSuffix(file, ext)

	consensusDir := filepath.Join(dir, ".consensus")
	return filepath.Join(consensusDir, name+"_"+suffix+ext)
}
