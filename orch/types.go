// Package orch provides the DAG-driven orchestrator library for BVV
// (Build-Verify-Validate) autonomous agent workflows.
//
// This file defines the core type system consumed by Engine, Dispatcher,
// Store implementations, and the CLI. Types are deliberately minimal per
// BVV-DSN-04 (phase-agnostic orchestration) — the orchestrator never reads
// task semantics, only metadata carried via Labels.
package orch

import "time"

// --- Enumerations (typed string consts) ---

// TaskStatus represents the lifecycle state of a task per BVV spec §5.1a.
type TaskStatus string

const (
	StatusOpen       TaskStatus = "open"
	StatusAssigned   TaskStatus = "assigned"
	StatusInProgress TaskStatus = "in_progress"
	StatusCompleted  TaskStatus = "completed"
	StatusFailed     TaskStatus = "failed"
	StatusBlocked    TaskStatus = "blocked" // BVV addition — terminal for orchestrator dispatch
)

// Terminal reports whether the status is a terminal state.
// BVV spec §5.1a: {completed, failed, blocked} are terminal.
// BVV-S-02 (terminal irreversibility) depends on this classification.
func (s TaskStatus) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusBlocked
}

// Criticality represents whether a task's failure is lifecycle-terminal.
// BVV-ERR-03 (critical failures abort immediately) vs. BVV-ERR-04
// (non-critical failures accumulate into gap tolerance).
//
// Spec adaptation: BVV spec §11.3 uses a "critical:true" label. This
// implementation uses key "criticality" with value "critical"/"non_critical"
// to align with the typed Criticality enum and avoid the ambiguous
// Labels["critical"] == "critical" pattern.
type Criticality string

const (
	Critical    Criticality = "critical"
	NonCritical Criticality = "non_critical"
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
// BVV-DSP-16: beads (Dolt-backed) is the default; fs is the fallback.
type LedgerKind string

const (
	LedgerFS    LedgerKind = "fs"
	LedgerBeads LedgerKind = "beads"
)

// --- Configuration types ---

// LockConfig configures the exclusive per-branch lifecycle lock.
// BVV-S-01 (lifecycle exclusion), BVV-ERR-06 (lock acquisition),
// BVV-ERR-10a (lock release), BVV-L-02 (lock liveness).
type LockConfig struct {
	Path               string
	StalenessThreshold time.Duration
	RetryCount         int
	RetryDelay         time.Duration
}

// Label key constants for domain metadata stored in Task.Labels.
// BVV-DSN-04: the orchestrator is phase-agnostic — role, branch, and
// criticality are carried as labels, not typed struct fields.
const (
	LabelRole        = "role"
	LabelBranch      = "branch"
	LabelCriticality = "criticality"
)

// --- Runtime types ---

// Task is the BVV unit of work. Role, branch, and criticality live in Labels
// (BVV-DSN-04: phase-agnostic orchestration — the orchestrator never reads
// task semantics, only metadata carried in labels).
type Task struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Status    TaskStatus        `json:"status"`
	Assignee  string            `json:"assignee,omitempty"`
	Priority  int               `json:"priority"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Role returns the role tag from labels. Empty if unset.
// BVV-AI-02: the role label drives instruction file and preset selection.
func (t *Task) Role() string { return t.Labels[LabelRole] }

// Branch returns the lifecycle branch from labels. Empty if unset.
// Used for per-branch lifecycle scoping (BVV-S-01).
func (t *Task) Branch() string { return t.Labels[LabelBranch] }

// IsCritical reports whether this task is BVV-critical. Non-critical
// failures accumulate into gap tolerance (BVV-ERR-04); critical failures
// abort the lifecycle immediately (BVV-ERR-03).
func (t *Task) IsCritical() bool { return t.Labels[LabelCriticality] == string(Critical) }

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

// RoleConfig binds a role tag to an instruction file and launch preset.
// The orchestrator looks up the role from a task's Labels[LabelRole] and
// routes to the matching RoleConfig. BVV-AI-02, BVV-DSP-03.
type RoleConfig struct {
	InstructionFile string  // path to OOMPA.md / LOOMPA.md / CHARLIE.md / etc.
	Preset          *Preset // launch command, flags, model
}

// LifecycleConfig is the per-branch runtime configuration assembled by the
// CLI and consumed by Engine.Run / Engine.Resume. BVV spec §8, §11.
type LifecycleConfig struct {
	Branch       string
	GapTolerance int                   // BVV-ERR-04
	MaxRetries   int                   // BVV-ERR-01
	MaxHandoffs  int                   // BVV-DSP-14, BVV-L-04
	BaseTimeout  time.Duration         // BVV-ERR-02a
	Lock         LockConfig            // per-branch exclusive lifecycle lock; see lock.go (Phase 3)
	Roles        map[string]RoleConfig // role tag → binding
}

// --- Agent outcome (exit code protocol) ---

// AgentOutcome represents the result of an agent invocation, determined
// solely by the agent's exit code. The orchestrator never inspects agent
// output content (ZFC / BVV-DSN-04).
//
// Values are in 1:1 correspondence with agent exit codes 0–3; see
// BVVTaskMachine.tla for the formal state-machine model.
type AgentOutcome string

const (
	// OutcomeSuccess — exit 0: task completed successfully (BVV-DSP-03).
	OutcomeSuccess AgentOutcome = "success"
	// OutcomeFailure — exit 1: retryable failure (BVV-ERR-01).
	OutcomeFailure AgentOutcome = "failure"
	// OutcomeBlocked — exit 2: terminal, non-retryable (BVV spec §5.1a).
	OutcomeBlocked AgentOutcome = "blocked"
	// OutcomeHandoff — exit 3: new session for same task (BVV-DSP-14, BVV-L-04).
	OutcomeHandoff AgentOutcome = "handoff"
)

// String returns the outcome label for logging and event serialization.
func (o AgentOutcome) String() string { return string(o) }
