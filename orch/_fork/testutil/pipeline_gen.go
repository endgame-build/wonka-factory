// Package testutil provides test helpers for the orch package.
package testutil

import (
	"fmt"
	"time"

	"github.com/endgame/facet-scan/orch"
	"pgregory.net/rapid"
)

// DefaultTestLock returns a LockConfig suitable for unit tests.
func DefaultTestLock() orch.LockConfig {
	return orch.LockConfig{
		Path:               "out/.lock",
		StalenessThreshold: 1 * time.Hour,
		RetryCount:         1,
		RetryDelay:         10 * time.Millisecond,
	}
}

// MiniPipeline returns a small 2-phase pipeline for quick integration tests.
func MiniPipeline() *orch.Pipeline {
	return &orch.Pipeline{
		ID:           "test-mini",
		OutputDir:    "out",
		GapTolerance: 1,
		Lock:         DefaultTestLock(),
		Phases: []orch.Phase{
			{
				ID: "p1", Topology: orch.Parallel,
				Agents: []orch.AgentDef{
					{ID: "a1", Model: orch.ModelSonnet, Output: "a1.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "a2", Model: orch.ModelSonnet, Output: "a2.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				},
			},
			{
				ID: "p2", Topology: orch.Sequential,
				Agents: []orch.AgentDef{
					{ID: "a3", Model: orch.ModelSonnet, Inputs: []string{"a1.md"}, Output: "a3.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				},
			},
		},
	}
}

// ConsensusPipeline returns a pipeline with a consensus phase for testing.
func ConsensusPipeline() *orch.Pipeline {
	return &orch.Pipeline{
		ID:           "test-consensus",
		OutputDir:    "out",
		GapTolerance: 1,
		Lock:         DefaultTestLock(),
		Phases: []orch.Phase{
			{
				ID: "scout", Topology: orch.Consensus,
				Consensus: &orch.ConsensusConfig{
					InstanceCount: 3, BatchSize: 2,
					MergeAgent: "merger", VerifyAgent: "verifier",
				},
				Agents: []orch.AgentDef{
					{ID: "s1", Model: orch.ModelOpus, Output: "s1.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "s2", Model: orch.ModelOpus, Output: "s2.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "s3", Model: orch.ModelOpus, Output: "s3.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				},
			},
		},
	}
}

// RetryPipeline returns a single-phase pipeline with one critical agent and one
// non-critical filler — the minimal setup for testing retry/resume behaviour.
func RetryPipeline() *orch.Pipeline {
	return &orch.Pipeline{
		ID:           "test-retry",
		OutputDir:    "out",
		GapTolerance: 1,
		Lock:         DefaultTestLock(),
		Phases: []orch.Phase{
			{
				ID: "p1", Topology: orch.Parallel,
				Agents: []orch.AgentDef{
					{ID: "critical-agent", Model: orch.ModelSonnet, Output: "c1.md", Criticality: orch.Critical, Format: orch.FormatMd},
					{ID: "filler", Model: orch.ModelSonnet, Output: "f1.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				},
			},
		},
	}
}

// GatedPipeline returns a pipeline with a sequential quality gate.
func GatedPipeline() *orch.Pipeline {
	return &orch.Pipeline{
		ID:           "test-gated",
		OutputDir:    "out",
		GapTolerance: 1,
		Lock:         DefaultTestLock(),
		Phases: []orch.Phase{
			{
				ID: "work", Topology: orch.Parallel,
				Agents: []orch.AgentDef{
					{ID: "w1", Model: orch.ModelSonnet, Output: "w1.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
				},
			},
			{
				ID: "gate-phase", Topology: orch.Sequential,
				Gate: &orch.QualityGate{Agent: "validator", Halt: true},
				Agents: []orch.AgentDef{
					{ID: "checker", Model: orch.ModelSonnet, Inputs: []string{"w1.md"}, Output: "check.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
					{ID: "validator", Model: orch.ModelHaiku, Inputs: []string{"check.md"}, Output: "valid.md", Criticality: orch.Critical, Format: orch.FormatMd},
				},
			},
		},
	}
}

// RandomWellFormedPipeline generates a random well-formed pipeline for property-based testing.
func RandomWellFormedPipeline(t *rapid.T) *orch.Pipeline {
	numPhases := rapid.IntRange(1, 4).Draw(t, "numPhases")

	var phases []orch.Phase
	var availableOutputs []string
	ncCount := 0

	for i := range numPhases {
		topo := rapid.SampledFrom([]orch.Topology{orch.Sequential, orch.Parallel, orch.Consensus}).Draw(t, fmt.Sprintf("topo_%d", i))
		numAgents := rapid.IntRange(1, 3).Draw(t, fmt.Sprintf("agents_%d", i))

		var agents []orch.AgentDef
		for j := range numAgents {
			output := fmt.Sprintf("p%d_a%d.md", i, j)
			var inputs []string
			if len(availableOutputs) > 0 && rapid.Bool().Draw(t, fmt.Sprintf("hasInput_%d_%d", i, j)) {
				idx := rapid.IntRange(0, len(availableOutputs)-1).Draw(t, fmt.Sprintf("inputIdx_%d_%d", i, j))
				inputs = []string{availableOutputs[idx]}
			}
			agents = append(agents, orch.AgentDef{
				ID:          fmt.Sprintf("agent-%d-%d", i, j),
				Model:       orch.ModelSonnet,
				Inputs:      inputs,
				Output:      output,
				Criticality: orch.NonCritical,
				Format:      orch.FormatMd,
				MaxTurns:    10,
			})
			ncCount++
		}

		ph := orch.Phase{
			ID:       fmt.Sprintf("phase-%d", i),
			Topology: topo,
			Agents:   agents,
		}

		if topo == orch.Consensus {
			instanceCount := rapid.IntRange(2, 3).Draw(t, fmt.Sprintf("instances_%d", i))
			batchSize := rapid.IntRange(1, numAgents).Draw(t, fmt.Sprintf("batchSize_%d", i))
			ph.Consensus = &orch.ConsensusConfig{
				InstanceCount: instanceCount,
				BatchSize:     batchSize,
				MergeAgent:    "merger",
				VerifyAgent:   "verifier",
			}
		}

		// Randomly add a quality gate to sequential phases with 2+ agents (WFC-07/08 coverage).
		// Requires 2+ agents so the gate doesn't consume the last non-critical (WFC-10 needs ncCount >= 1).
		if topo == orch.Sequential && numAgents >= 2 && rapid.Bool().Draw(t, fmt.Sprintf("hasGate_%d", i)) {
			gateIdx := numAgents - 1                    // gate is last agent in phase
			agents[gateIdx].Criticality = orch.Critical // WFC-08: gate must be critical
			ncCount--                                   // one less non-critical
			ph.Gate = &orch.QualityGate{Agent: agents[gateIdx].ID, Halt: true}
			ph.Agents = agents // update with modified criticality
		}

		phases = append(phases, ph)
		for _, a := range agents {
			availableOutputs = append(availableOutputs, a.Output)
		}
	}

	if ncCount < 1 {
		ncCount = 1 // ensure at least 1 non-critical for valid gap_tolerance
	}
	gapTol := 1
	if ncCount > 1 {
		gapTol = rapid.IntRange(1, ncCount).Draw(t, "gapTol")
	}

	return &orch.Pipeline{
		ID:           "test-random",
		OutputDir:    "out",
		GapTolerance: gapTol,
		Lock:         DefaultTestLock(),
		Phases:       phases,
	}
}

// MutateWFC takes a well-formed pipeline and introduces a specific WFC violation.
func MutateWFC(p *orch.Pipeline, violation string) *orch.Pipeline {
	// Deep copy (simple: modify in place on a copy of the struct).
	cp := *p
	phases := make([]orch.Phase, len(p.Phases))
	copy(phases, p.Phases)
	cp.Phases = phases

	switch violation {
	case "WFC-01": // duplicate output
		if len(cp.Phases) > 0 && len(cp.Phases[0].Agents) > 0 {
			agents := make([]orch.AgentDef, len(cp.Phases[0].Agents))
			copy(agents, cp.Phases[0].Agents)
			agents = append(agents, orch.AgentDef{
				ID: "dup", Model: orch.ModelSonnet, Output: agents[0].Output,
				Criticality: orch.NonCritical, Format: orch.FormatMd,
			})
			cp.Phases[0].Agents = agents
		}
	case "WFC-03": // consensus without config
		cp.Phases = append(cp.Phases, orch.Phase{
			ID: "bad-consensus", Topology: orch.Consensus,
			Agents: []orch.AgentDef{{ID: "x", Model: orch.ModelSonnet, Output: "x.md", Criticality: orch.NonCritical, Format: orch.FormatMd}},
		})
	case "WFC-04": // non-consensus with config
		if len(cp.Phases) > 0 && cp.Phases[0].Topology != orch.Consensus {
			cp.Phases[0].Consensus = &orch.ConsensusConfig{InstanceCount: 3, BatchSize: 1, MergeAgent: "m", VerifyAgent: "v"}
		}
	case "WFC-06": // instance count < 2
		cp.Phases = append(cp.Phases, orch.Phase{
			ID: "bad-count", Topology: orch.Consensus,
			Consensus: &orch.ConsensusConfig{InstanceCount: 1, BatchSize: 1, MergeAgent: "m", VerifyAgent: "v"},
			Agents:    []orch.AgentDef{{ID: "y", Model: orch.ModelSonnet, Output: "y.md", Criticality: orch.NonCritical, Format: orch.FormatMd}},
		})
	case "WFC-07": // gate agent not found
		cp.Phases = append(cp.Phases, orch.Phase{
			ID: "bad-gate", Topology: orch.Sequential,
			Gate:   &orch.QualityGate{Agent: "nonexistent", Halt: true},
			Agents: []orch.AgentDef{{ID: "z", Model: orch.ModelSonnet, Output: "z.md", Criticality: orch.NonCritical, Format: orch.FormatMd}},
		})
	case "WFC-08": // gate agent not critical
		cp.Phases = append(cp.Phases, orch.Phase{
			ID: "bad-crit", Topology: orch.Sequential,
			Gate: &orch.QualityGate{Agent: "gate-nc", Halt: true},
			Agents: []orch.AgentDef{
				{ID: "gate-nc", Model: orch.ModelSonnet, Output: "gate-nc.md", Criticality: orch.NonCritical, Format: orch.FormatMd},
			},
		})
	case "WFC-10": // gap tolerance out of bounds
		cp.GapTolerance = 0
	case "WFC-11": // lock path outside output dir
		cp.Lock.Path = "/tmp/elsewhere/.lock"
	default:
		panic(fmt.Sprintf("MutateWFC: unknown constraint %q", violation))
	}
	return &cp
}
