// Package orch provides a reusable orchestrator library for multi-agent pipelines.
package orch

import "time"

// --- Enumerations (typed string consts) ---

// TaskStatus represents the lifecycle state of a task.
// Only 5 statuses: deferred and overridden are excluded per conformance profile PC-01/PC-06.
type TaskStatus string

const (
	StatusOpen       TaskStatus = "open"
	StatusAssigned   TaskStatus = "assigned"
	StatusInProgress TaskStatus = "in_progress"
	StatusCompleted  TaskStatus = "completed"
	StatusFailed     TaskStatus = "failed"
)

// Terminal returns true if the status is a terminal state (completed or failed).
func (s TaskStatus) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed
}

// TaskType represents what kind of task this is in the expanded graph.
// work, escalation, and boundary_gate are excluded per conformance profile.
type TaskType string

const (
	TypePipeline          TaskType = "pipeline"
	TypePhase             TaskType = "phase"
	TypeAgent             TaskType = "agent"
	TypeConsensusInstance TaskType = "consensus_instance"
	TypeConsensusMerge    TaskType = "consensus_merge"
	TypeConsensusVerify   TaskType = "consensus_verify"
)

// Topology represents how agents within a phase are structured.
type Topology string

const (
	Sequential Topology = "sequential"
	Parallel   Topology = "parallel"
	Consensus  Topology = "consensus"
)

// Criticality represents whether an agent's failure is pipeline-terminal.
type Criticality string

const (
	Critical    Criticality = "critical"
	NonCritical Criticality = "non_critical"
)

// Format represents the expected output format of an agent.
type Format string

const (
	FormatMd    Format = "md"
	FormatJsonl Format = "jsonl"
	FormatJson  Format = "json"
	FormatYaml  Format = "yaml"
)

// WorkerStatus represents the lifecycle state of a worker.
type WorkerStatus string

const (
	WorkerIdle   WorkerStatus = "idle"
	WorkerActive WorkerStatus = "active"
)

// Model represents which AI model to use for an agent.
type Model string

const (
	ModelOpus   Model = "opus"
	ModelSonnet Model = "sonnet"
	ModelHaiku  Model = "haiku"
)

// LedgerKind selects the backing store implementation.
type LedgerKind string

const (
	LedgerFS    LedgerKind = "fs"
	LedgerBeads LedgerKind = "beads"
)

// --- Pipeline Definition Types ---

// Pipeline is the top-level definition of a multi-agent pipeline.
type Pipeline struct {
	ID           string
	OutputDir    string
	GapTolerance int
	Phases       []Phase
	Lock         LockConfig
}

// Phase defines a single phase within a pipeline.
type Phase struct {
	ID        string
	Topology  Topology
	Agents    []AgentDef
	Gate      *QualityGate     // nil if no gate
	Consensus *ConsensusConfig // nil unless topology == Consensus
}

// AgentDef defines a single agent within a phase.
type AgentDef struct {
	ID          string
	Model       Model
	Inputs      []string
	Output      string
	Criticality Criticality
	Format      Format
	MaxTurns    int
}

// ConsensusConfig configures the consensus protocol for a phase.
// Thresholds (high, low, similarity) are agent-domain metadata and
// not included here — the orchestrator never reads them (ZFC).
type ConsensusConfig struct {
	InstanceCount int
	BatchSize     int
	MergeAgent    string
	VerifyAgent   string
}

// QualityGate defines a quality gate within a phase.
type QualityGate struct {
	Agent string // agent ID whose completion is evaluated
	Halt  bool   // if true, failure blocks the pipeline (pipeline-terminal per PC-07)
}

// LockConfig configures the exclusive pipeline lock.
type LockConfig struct {
	Path               string
	StalenessThreshold time.Duration
	RetryCount         int
	RetryDelay         time.Duration
}

// --- Runtime Types ---

// Task represents a single unit of work in the expanded task graph.
type Task struct {
	ID        string     `json:"id"`
	ParentID  string     `json:"parent_id,omitempty"`
	Type      TaskType   `json:"type"`
	Status    TaskStatus `json:"status"`
	Assignee  string     `json:"assignee,omitempty"`
	Priority  int        `json:"priority"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`

	// AgentID links back to the AgentDef this task was expanded from.
	// Empty for pipeline and phase tasks.
	AgentID string `json:"agent_id,omitempty"`

	// Output is the expected output path for leaf tasks (agent, consensus_*).
	// Copied from AgentDef.Output during expansion.
	Output string `json:"output,omitempty"`
}

// Worker represents a process slot that executes agent tasks.
type Worker struct {
	Name             string       `json:"name"`
	Status           WorkerStatus `json:"status"`
	CurrentTaskID    string       `json:"current_task_id,omitempty"`
	SessionPID       int          `json:"session_pid,omitempty"`
	SessionStartedAt time.Time    `json:"session_started_at,omitempty"`
}

// Preset describes how to launch, detect, and communicate with a specific agent type.
type Preset struct {
	Name         string
	Command      string
	Args         []string
	ProcessNames []string
	PromptFlag   string
	AgentFlag    string
	PluginFlag   string
	// SystemPromptFlag injects the agent definition body as a system prompt
	// (e.g. "--append-system-prompt"). SpawnSession reads the agent's .md file
	// from the plugin directory, strips YAML frontmatter, and passes the content
	// via this flag. Empty means no system prompt injection.
	SystemPromptFlag string
	// ModelFlag overrides the model selection (e.g. "--model"). The model name
	// comes from the agent definition frontmatter. Empty means no override.
	ModelFlag string
	Env       map[string]string
	// TextFilter is a jq expression for extracting human-readable text from
	// stream-json output. When set, BuildShellCommand pipes output through
	// tee (raw JSONL to .stdout) and jq (filtered text to .txt). Empty means
	// no filtering (plain stdout capture).
	TextFilter string
}

// BuildAgentIndex creates a lookup map from agentID → AgentDef for all agents
// in a pipeline. Used by Dispatcher and Watchdog to resolve task.AgentID.
//
// Includes both phase-level agents and consensus-specific agents (MergeAgent,
// VerifyAgent) referenced by ConsensusConfig. Consensus agents inherit Format
// from the first agent in the phase (they operate on the same output format).
func BuildAgentIndex(p *Pipeline) map[string]AgentDef {
	idx := make(map[string]AgentDef)
	for _, phase := range p.Phases {
		for _, a := range phase.Agents {
			idx[a.ID] = a
		}
		// Register consensus merge/verify agents if not already present.
		if phase.Consensus != nil {
			// Infer format from the first agent in the phase.
			var format Format
			if len(phase.Agents) > 0 {
				format = phase.Agents[0].Format
			}
			// Merge is critical (produces the deliverable); verify is non-critical
			// (validates the already-merged output — a failed verify is a gap, not a halt).
			critMap := map[string]Criticality{
				phase.Consensus.MergeAgent:  Critical,
				phase.Consensus.VerifyAgent: NonCritical,
			}
			defaults := AgentDef{Model: ModelOpus, Format: format, MaxTurns: 100}
			for _, id := range []string{phase.Consensus.MergeAgent, phase.Consensus.VerifyAgent} {
				if id != "" {
					if _, ok := idx[id]; !ok {
						d := defaults
						d.ID = id
						d.Criticality = critMap[id]
						idx[id] = d
					}
				}
			}
		}
	}
	return idx
}
