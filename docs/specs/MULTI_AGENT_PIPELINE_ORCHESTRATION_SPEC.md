# Multi-Agent Pipeline Orchestration Specification

**Version:** 1.0.0-draft
**Status:** Working Draft
**Editors:** ENDGAME
**Date:** 2026-03-27

---

## Abstract

This specification defines an architecture for orchestrating autonomous AI reasoning agents through structured, multi-phase pipelines. It addresses two concerns within a single model: (1) an infrastructure layer that manages worker identity, durable state, process supervision, workspace isolation, and merge protocols; and (2) a pipeline execution layer that defines phase topologies, consensus protocols, quality gates, and formal safety properties. The two layers are connected by a deterministic expansion function that transforms pipeline definitions into task graphs within a unified assignment ledger.

## Status of This Document

This document is a Working Draft.

## Notices

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

Non-normative text is marked with *(Non-normative)* or enclosed in a note block.

---

## 1 Introduction

### 1.1 Purpose

This specification defines a model for coordinating processes that are:

- **Opaque** — internal state cannot be inspected by the orchestrator
- **Unreliable** — may crash, loop, or diverge
- **Autonomous** — interpret rather than mechanically execute instructions
- **Heterogeneous** — different implementations with different interfaces

The model separates deterministic infrastructure (process management, state persistence, merge serialization) from nondeterministic cognition (analysis, judgment, artifact generation). It further defines a structured pipeline execution model that composes agents into phased workflows with consensus validation, quality gates, and formal correctness guarantees.

### 1.2 Scope

This specification covers:

- Worker identity, lifecycle, and workspace isolation
- A unified assignment ledger as the single durable store for orchestration state
- Agent-to-orchestrator command interface
- Process supervision and failure recovery
- Workspace merge protocols for integrating agent work products
- Session continuity across context window boundaries
- Pipeline definition: phases, topologies, and consensus configuration
- Deterministic expansion of pipeline definitions into ledger task graphs
- Multi-instance consensus with finding classification and verification
- Quality gates, session boundaries, and checkpoint/resume
- Error semantics: retry, gap accumulation, and criticality
- Safety and liveness properties

This specification does not cover:

- Internal behavior of any agent implementation
- Domain-specific workflow semantics
- Authentication, authorization, or multi-tenancy
- Network topology or deployment infrastructure
- Inter-agent messaging (see Section 3.3)

### 1.3 Normative References

- RFC 2119: Key words for use in RFCs to Indicate Requirement Levels

### 1.4 Typographical Conventions

Normative requirements carry identifiers in brackets (e.g., `[WKR-01]`). Inference rules use standard notation:

```
  premise₁   premise₂
  ─────────────────────  [RULE-NAME]
       conclusion
```

Read: "if premise₁ and premise₂ hold, then conclusion holds."

---

## 2 Terminology and Definitions

| Term | Definition |
|------|-----------|
| **Agent** | An autonomous reasoning process that interprets instructions, makes judgment calls, and produces artifacts. Opaque to the orchestrator. |
| **Assignment Ledger** | The unified durable store containing the authoritative record of workers, tasks, dependencies, and heartbeats. |
| **Consensus Round** | A sequence of sub-phases (instance, merge, verify) that produces a validated output from multiple independent agent invocations. |
| **Content Merge** | An agent task that combines findings from multiple consensus instances into a single output. Distinct from workspace merge. |
| **Escalation Task** | A standard task with `type=escalation` created to surface a problem for human or triage-worker review. |
| **Finding** | A semantic unit of analysis output: a table row, diagram node, list item, or equivalent. The atomic element of consensus classification. |
| **Finding Group** | A set of matchable findings across instances, classified as unanimous, majority, or unique. |
| **Handoff** | The transfer of working context from a dying session to its successor via a structured workspace file. |
| **Heartbeat** | A timestamped signal written by a session to the ledger to indicate liveness. |
| **Merge Lock** | A mutual exclusion mechanism that serializes the integration of worker changes into the canonical source. |
| **Orchestrator** | The deterministic infrastructure layer that manages process lifecycles and state persistence. Not an agent. |
| **Phase** | An ordered step within a pipeline, containing one or more agent invocations arranged by topology. |
| **Pipeline** | A sequence of phases that transforms inputs into validated outputs. Expanded into a task graph for execution. |
| **Pipeline Expansion** | The deterministic function that transforms a pipeline definition into a task graph in the assignment ledger. |
| **Preset** | A configuration record describing how to launch, detect, and communicate with a specific agent type. |
| **Quality Gate** | A phase-level checkpoint that evaluates an agent's output and may halt pipeline progression. |
| **Session** | A single OS-level execution of an agent process. Ephemeral. Has a finite context window. |
| **Topology** | The execution arrangement of agents within a phase: sequential, parallel, or consensus. |
| **Verification Tag** | An annotation on a consensus finding that tracks its provenance through the verification sub-phase. |
| **Watchdog** | A deterministic process that monitors agent session liveness and restarts dead sessions. Not an agent. |
| **Worker** | A named entity with persistent identity to which work is assigned. A worker's identity outlives any single session. |
| **Workspace** | An isolated filesystem directory in which a single worker operates. |
| **Workspace Merge** | The integration of a worker's filesystem changes into the canonical source via a serialized protocol. Distinct from content merge. |

---

## 3 Conformance

### 3.1 Conformance Targets

This specification defines requirements for two conformance targets:

- **Orchestrator**: The infrastructure component that manages workers, the ledger, process supervision, and pipeline execution.
- **Agent**: Any autonomous process that receives assignments from and reports results to the orchestrator.

### 3.2 Conformance Levels

**Level 1 — Infrastructure.** An orchestrator conforms at Level 1 if it satisfies all MUST requirements in Sections 4 through 11a (inclusive of Section 11a, Failure Recovery). This level is sufficient for task-based orchestration without structured pipelines.

**Level 2 — Pipeline Execution.** An orchestrator conforms at Level 2 if it satisfies Level 1 and all MUST requirements in Sections 12 through 17. This level adds pipeline definitions, expansion, consensus, and error semantics.

**Level 3 — Full.** An orchestrator conforms at Level 3 if it satisfies Level 2 and all MUST requirements in Sections 18 through 21. This level adds formal property guarantees, agent-type abstraction, and observability.

### 3.3 Extension Points

Implementations MAY extend this specification by adopting the following capabilities. Each extension is self-contained.

| Extension | When to Adopt |
|-----------|---------------|
| Inter-agent messaging (durable mail channel) | Workers need to send ad-hoc messages to each other |
| Multi-scope hierarchy (organization + project) | Multiple projects with independent worker pools |
| Ephemeral notification channel | Sub-second latency-sensitive inter-agent coordination |
| Cost tier management (role-to-model mapping) | Budget control across heterogeneous agent types |
| Cognitive supervision hierarchy | Autonomous recovery reasoning without human escalation |
| Conflict-domain partitioning | Multiple workers merging disjoint file sets concurrently |

---

## 4 Design Principles

### 4.1 Separation of Infrastructure and Cognition

`[DSN-01]` The orchestrator MUST NOT perform reasoning, interpretation, or judgment.

`[DSN-02]` The orchestrator MUST NOT contain conditional logic predicated on agent output content, task semantics, or domain-specific heuristics.

`[DSN-03]` The orchestrator MUST NOT parse, interpret, or route based on the content of agent-generated artifacts.

`[DSN-04]` Agents MUST NOT manage their own process lifecycle or the lifecycle of other agents directly. Agents interact with the orchestrator exclusively through the interface defined in Section 7.

`[DSN-05]` When a capability requires judgment, it MUST be delegated to an agent or a human operator — not implemented as orchestrator logic.

`[DSN-06]` The orchestrator MUST NOT hardcode behavioral thresholds that determine agent lifecycle actions (e.g., "restart after N minutes idle"). The orchestrator MAY detect and expose quantitative signals (e.g., "heartbeat age is N minutes"). The decision to act on those signals belongs to agents or human operators.

**Compliance test:** For any proposed orchestrator logic, apply: *"Can this be expressed as a deterministic function with no judgment calls?"* If yes, it belongs in the orchestrator. If no, it belongs in an agent.

### 4.2 Single Durable Store

`[DSN-07]` All durable orchestration state — tasks, workers, dependencies, and heartbeats — MUST reside in the assignment ledger (Section 6). There MUST NOT be separate heartbeat files, message stores, or workflow state trackers that serve as sources of truth.

> *Note (non-normative):* Projection caches that mirror ledger state for performance are permitted, provided the ledger remains authoritative and the cache is regenerable from the ledger at any time.

### 4.3 No Abstraction Without Duplication

`[DSN-08]` If a concept can be modeled as a task in the ledger, it MUST NOT receive its own abstraction. Pipeline phases are tasks. Escalations are tasks. Consensus rounds are task subgraphs.

**Simplicity test:** For any proposed new abstraction, apply: *"Can this be modeled as a task in the ledger? If yes, don't create a new abstraction."*

### 4.4 Separation of Workspace Merge and Content Merge

`[DSN-09]` The specification distinguishes two merge operations. **Workspace merge** (Section 10) integrates a worker's filesystem changes into the canonical source. **Content merge** (Section 16) combines findings from multiple consensus instances into a single output. These are independent operations. A content merge is an agent task assigned to a worker; upon completion, its output is integrated via workspace merge like any other task result.

---

## 5 Worker Identity Model

### 5.1 Identity Properties

Each worker MUST have the following properties:

| Property | Durability | Description |
|----------|-----------|-------------|
| `name` | Permanent | A stable, human-readable identifier unique within the project. |
| `assignment` | Durable | A record in the assignment ledger linking the worker to zero or one active task. |
| `workspace` | Durable | An isolated filesystem directory that persists across sessions. |
| `history` | Durable | An ordered record of past assignments. |
| `session` | Ephemeral | Zero or one active OS process. |

### 5.2 Identity Invariants

`[WKR-01]` A worker's identity MUST persist independently of any session. Session termination MUST NOT destroy the worker's name, assignment, workspace, or history.

`[WKR-02]` There MUST be at most one active session for a given worker at any time.

`[WKR-03]` A worker's assignment MUST be recoverable from the assignment ledger alone, without access to the session's internal state.

### 5.3 Lifecycle States

```
             allocate              start session
  (none) ──────────► IDLE ──────────────────► ACTIVE
                      ▲                         │
                      │      session dies        │
                      │   (crash, exit, kill)    │
                      ◄─────────────────────────┘
                      │
                      │  deallocate
                      └──────────► (none)
```

`[WKR-04]` A worker in IDLE state MUST have no active session but MAY have an assignment and workspace.

`[WKR-05]` A worker in ACTIVE state MUST have exactly one active session.

`[WKR-06]` Transition from ACTIVE to IDLE MUST NOT modify the assignment ledger. The assignment persists so a new session can recover it.

### 5.4 Worker Pool Reuse

`[WKR-07]` When new work arrives, the orchestrator SHOULD reuse an IDLE worker before allocating a new one.

`[WKR-08]` Reuse MUST reset the workspace to a clean state before assigning new work. Reuse MUST preserve the worker's name and history chain. This applies only when assigning *new* work. Session cycling for continuity on the *same* task (Section 11) MUST NOT reset the workspace.

`[WKR-09]` Before resetting a workspace for reuse, the orchestrator MUST verify that the workspace contains no unmerged changes. If the workspace is dirty, the orchestrator MUST create an escalation task and MUST NOT reset the workspace.

### 5.5 Worker Deallocation

`[WKR-10]` Deallocation MUST be triggered by explicit action. The orchestrator MUST NOT deallocate workers implicitly on a timer.

`[WKR-11]` A worker with an active assignment MUST NOT be deallocated. The assignment must be completed, failed, or reassigned first.

`[WKR-12]` Upon deallocation, the orchestrator MUST release the worker's name for future allocation, remove the workspace, and retain the history record.

---

## 6 Assignment Ledger

### 6.1 Requirements

`[LDG-01]` The assignment ledger MUST be a durable store that survives orchestrator restarts and agent crashes.

`[LDG-02]` The ledger MUST be the single source of truth for all orchestration state.

### 6.2 Entity Schema

The ledger MUST contain at minimum the following entity types:

**Workers:**

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique identifier. |
| `status` | enum | `idle` or `active`. |
| `last_heartbeat` | timestamp | When the worker last signaled liveness. |
| `heartbeat_state` | enum | Agent-reported state: `working`, `idle`, `exiting`. |
| `current_task_id` | string? | Active assignment, or null if unassigned. |
| `session_pid` | integer? | OS process ID of active session. |
| `session_started_at` | timestamp? | When the current session was spawned. |

**Tasks:**

| Field | Type | Description |
|-------|------|-------------|
| `task_id` | string | Unique identifier. |
| `parent_task_id` | string? | Parent task, or null for top-level. |
| `type` | string | Task type tag (e.g., `work`, `escalation`, `phase`, `agent`, `consensus-instance`, `consensus-merge`, `consensus-verify`, `boundary-gate`). Used for filtering and routing, not for orchestrator logic. |
| `status` | enum | `open`, `assigned`, `in_progress`, `completed`, `failed`, `deferred`, `overridden`. Terminal statuses: `completed`, `failed`, `overridden`. |
| `assignee` | string? | Worker name, or null if unassigned. |
| `priority` | integer | Dispatch priority. Lower values = higher priority. |
| `body` | text? | Task description, instructions, or context. Maximum 32 KB. |
| `result` | text? | Agent-provided result data on completion. Maximum 32 KB. Opaque to the orchestrator. |
| `metadata` | map? | Structured key-value data for task-type-specific properties. Opaque to the orchestrator. |
| `created_at` | timestamp | When the task was created. |
| `updated_at` | timestamp | When the record was last modified. |

**Task Dependencies:**

| Field | Type | Description |
|-------|------|-------------|
| `task_id` | string | The blocked task. |
| `depends_on` | string | The blocking task. |

**Worker History:**

| Field | Type | Description |
|-------|------|-------------|
| `worker_name` | string | The worker this record belongs to. |
| `task_id` | string | The task that was assigned. |
| `started_at` | timestamp | When the assignment began. |
| `ended_at` | timestamp | When the assignment ended. |
| `outcome` | enum | `completed`, `failed`, `reassigned`. |

### 6.3 Task Graphs

`[LDG-03]` A task with a non-null `parent_task_id` is a child task. A parent task's status is derived per Section 6.6.

`[LDG-04]` A task whose `depends_on` entries include tasks that have not all reached a terminal status MUST NOT be dispatched. Only tasks whose blockers have all reached a terminal status are eligible for assignment.

`[LDG-05]` A `deferred` task is non-terminal: it may be resumed later. A dependency on a deferred task remains unresolved.

`[LDG-06]` The orchestrator MUST reject dependency edges that would create a cycle. Cycle detection MUST be performed at dependency creation time, not at dispatch time.

`[LDG-07]` Among tasks of equal priority that are ready for dispatch, the orchestrator SHOULD use a deterministic tiebreaker (e.g., lexicographic task ID order).

`[LDG-07a]` A task MAY produce output artifacts before reaching a terminal status. Dependency resolution operates on task status, not artifact presence. However, agents MAY consume peer artifacts opportunistically when they are available.

`[LDG-07b]` An agent MUST NOT depend on having received a message or notification to learn its assignment. The ledger is sufficient.

### 6.4 Assignment Protocol

`[LDG-08]` To assign a task to a worker, the orchestrator MUST atomically set `status=assigned` and `assignee=<worker_name>`.

`[LDG-09]` Assignment MUST be idempotent.

`[LDG-10]` Assignment operations MUST be serialized to prevent two dispatchers from assigning different tasks to the same worker, or the same task to different workers.

### 6.5 Concurrency Control

`[LDG-11]` Serialization MUST use a mechanism appropriate to the multi-process environment (e.g., database transactions, advisory file locks). In-process mutexes are insufficient when dispatchers run as separate OS processes.

`[LDG-12]` State files written by the orchestrator SHOULD use atomic writes (write to temporary file, then rename) to prevent corruption from crashes mid-write.

### 6.6 Task Lifecycle

`[LDG-13]` Tasks MAY be created by operators, agents (via the command interface), or external systems.

`[LDG-14]` Newly created tasks MUST have `status=open` and `assignee=null`.

`[LDG-14a]` A task transitions from `assigned` to `in_progress` when the assigned worker's session begins executing it. This transition SHOULD be recorded by the agent (via the command interface) or by the orchestrator upon session startup confirmation.

`[LDG-15]` A task MAY be reassigned from one worker to another. Reassignment MUST atomically update the `assignee` field. Reassignment MUST NOT be performed while the current assignee has an active session working on the task.

`[LDG-16]` A parent task's status MUST be updated by the orchestrator whenever a child task reaches a terminal status. This is an event-driven write, not a computed view.

`[LDG-17]` A parent task transitions to `completed` when all children have reached a terminal status and none have `failed`.

`[LDG-18]` A parent task transitions to `failed` when any child has `failed` and no non-terminal children remain.

`[LDG-19]` While any children remain non-terminal, the parent MUST remain in its current status.

`[LDG-20]` When a child task fails, the orchestrator MUST NOT automatically fail the parent task or its siblings. An escalation task SHOULD be created for review.

---

## 7 Agent-to-Orchestrator Interface

### 7.1 Identity Injection

`[ITF-01]` The orchestrator MUST inject the following environment variables into every agent session at spawn time:

| Variable | Description |
|----------|-------------|
| `ORCH_WORKER_NAME` | The worker's persistent name. |
| `ORCH_PROJECT` | The project this worker operates in. |
| `ORCH_ROLE` | The worker's full address (project + worker name). |
| `ORCH_WORKSPACE` | Absolute path to the worker's isolated workspace. |
| `ORCH_BRANCH` | The version control branch for this worker's changes. |
| `ORCH_RUN_ID` | Unique identifier for this session (for telemetry correlation). |

`[ITF-02]` Environment variables MUST be the sole mechanism for injecting identity context.

### 7.2 Command Interface

`[ITF-03]` The orchestrator MUST provide a command interface available in the agent's execution environment that implements at minimum:

| Command | Semantics |
|---------|-----------|
| `prime` | Return the worker's current assignment and handoff file content (if present). |
| `done` | Signal task completion. Sets task status to `completed`, performs synchronous workspace merge (Section 10), and returns the result. |
| `done --handoff` | Signal session handoff: task continues, session ends. Does NOT change task status or trigger merge. |
| `fail [--reason <msg>]` | Signal task failure. Sets task status to `failed`. |
| `task create <body> [--type <type>] [--parent <id>] [--depends-on <id>] [--priority <n>]` | Create a new task in the ledger. Returns the new task ID. |
| `task reassign <id> <worker>` | Reassign a task to a different worker. |
| `status [--worker <name>]` | Return lifecycle state, task state, and child task statuses. |
| `heartbeat` | Write a heartbeat to the ledger. |
| `escalate <message>` | Create an escalation task. Convenience for `task create --type escalation`. |
| `dashboard` | Return a read-only summary of system state. |
| `version` | Return the orchestrator's protocol version. |

`[ITF-04]` All commands MUST be callable from any agent that can execute shell commands. The interface MUST NOT require any language-specific SDK.

`[ITF-04a]` Commands that return data (e.g., `prime`, `status`, `dashboard`) SHOULD support a structured output flag (e.g., `--json`) alongside a human-readable default.

`[ITF-05]` The command interface MUST be the sole mechanism for agents to interact with the orchestrator. Agents MUST NOT access the ledger directly.

### 7.3 Exit Code Semantics

`[ITF-06]` Commands MUST use the following exit codes:

| Code | Meaning |
|------|---------|
| `0` | Success. |
| `1` | General failure. |
| `2` | Ledger unreachable (transient — retry appropriate). |
| `3` | Invalid request (permanent — do not retry). |
| `4` | Conflict (merge conflict, lock timeout, or task already assigned). |

### 7.4 Instruction File

`[ITF-07]` The orchestrator SHOULD write a per-workspace instruction file that teaches the agent the communication protocol, available commands, and behavioral expectations.

`[ITF-08]` The instruction file MUST be regenerated on each workspace reset to reflect current protocol versions.

`[ITF-09]` The instruction file SHOULD declare the orchestrator's protocol version so agents can detect version mismatches.

### 7.5 Protocol Versioning

`[ITF-10]` Implementations SHOULD expose their protocol version through the command interface (`version` command) and in the instruction file.

`[ITF-11]` The protocol version SHOULD follow Semantic Versioning: breaking changes to MUST requirements increment the major version; new SHOULD/MAY features increment the minor version.

---

## 8 Supervision

### 8.1 Watchdog Process

`[SUP-01]` The orchestrator MUST include a watchdog: a deterministic process (not an agent) that runs on a fixed cycle.

`[SUP-02]` On each cycle, the watchdog MUST verify that all expected agent sessions are alive and restart dead sessions mechanically.

`[SUP-03]` The watchdog MUST NOT decide *whether* to restart a dead session. It always restarts unless the circuit breaker has tripped.

`[SUP-04]` The watchdog MUST NOT change task status. Session restart is a process-level operation orthogonal to task lifecycle.

### 8.2 Circuit Breaker

`[SUP-05]` Auto-respawn MUST be bounded by a circuit breaker. After N consecutive rapid failures of the same worker (RECOMMENDED: N = 3), the watchdog MUST suspend restart attempts and create an escalation task.

`[SUP-06]` "Rapid failure" means the session exited within a configurable window of its start time (RECOMMENDED: 60 seconds).

### 8.3 Peer Observation and Escalation

`[SUP-07]` Any agent MAY query the ledger (via `status --worker <name>`) to observe a peer worker's heartbeat freshness and task status.

`[SUP-07a]` An agent that detects a stuck peer (stale heartbeat combined with no task progress) SHOULD create an escalation task.

`[SUP-08]` Escalation tasks are standard tasks with `type=escalation`. The orchestrator MUST NOT interpret their content.

### 8.4 Systemic Failure Detection

`[SUP-09]` The watchdog MUST track the count of escalation tasks created within a rolling time window (RECOMMENDED: 30 minutes).

`[SUP-10]` When the escalation count exceeds a configurable threshold (RECOMMENDED: 5), the watchdog MUST alert the operator.

---

## 9 Workspace Isolation

`[WSP-01]` Each worker MUST operate in a dedicated workspace directory that no other worker can write to.

`[WSP-02]` Workspaces MUST be copy-on-write derivatives of a shared canonical source (e.g., version control worktrees, filesystem snapshots). Full duplication SHOULD be avoided.

`[WSP-03]` Workers MUST NOT share a mutable workspace. Concurrent modification of shared files across workers MUST be prevented by the isolation mechanism, not by agent cooperation.

---

## 10 Workspace Merge Protocol

### 10.1 Synchronous Merge Semantics

`[MRG-01]` The `done` command is synchronous: the orchestrator performs the workspace merge inline and returns the result via exit code.

### 10.2 Merge Procedure

`[MRG-02]` The merge protocol operates as follows:

1. Set task status to `completed` in the ledger. Append a worker history record.
2. Acquire the project-wide merge lock.
3. Rebase the worker's branch onto the current canonical head.
4. Run validation checks if configured. Validation is infrastructure-level (deterministic), not agent-level.
5. Fast-forward merge into the canonical branch.
6. Release the merge lock.
7. Return exit code `0` to the calling agent.

`[MRG-03]` If the merge fails (steps 3-5), the task status MUST be reverted to `in_progress`.

`[MRG-04]` The merge lock MUST enforce mutual exclusion: only one merge operation may be in-flight at any time.

### 10.3 Conflict Handling

`[MRG-05]` If the rebase produces conflicts, the orchestrator MUST release the merge lock and return exit code `4`. The agent is responsible for resolving the conflict and calling `done` again.

`[MRG-05a]` If validation checks fail (step 4 of the merge procedure), the orchestrator MUST release the merge lock and return exit code `1`. The agent is responsible for fixing the validation issue.

`[MRG-06]` The merge lock MUST have a configurable timeout (RECOMMENDED: 5 minutes). If the lock cannot be acquired within the timeout, `done` MUST return exit code `4`.

`[MRG-07]` The merge protocol MUST NOT silently drop failed merge attempts.

### 10.4 Non-Standard Workspaces

`[MRG-08]` Implementations using non-git workspace isolation MUST provide equivalent semantics: mutual exclusion, conflict detection, validation, and failure signaling via exit codes.

### 10.5 Distinction from Content Merge

> *Note (non-normative):* Content merge (Section 16) is an entirely separate operation: an agent task that combines findings from multiple consensus instances into a single output file. It operates at the content level and has no interaction with the workspace merge protocol. When a content merge agent completes its work, it calls `done` like any other agent, which triggers the workspace merge protocol to integrate the merged output file.

---

## 11 Session Continuity

### 11.1 Problem Statement

Agent sessions have finite context windows. When accumulated context approaches capacity, the session must be replaced with a fresh one without losing progress.

### 11.2 Handoff Mechanism

`[CTY-01]` The orchestrator MUST support a handoff mechanism that transfers working context from an outgoing session to its successor via a structured workspace file.

`[CTY-02]` The outgoing session writes a handoff file to the workspace root before exiting. The incoming session discovers this file via the `prime` command.

### 11.3 Handoff Schema

`[CTY-03]` The handoff file MUST conform to a structured schema containing at minimum:

| Field | Type | Description |
|-------|------|-------------|
| `version` | integer | Schema version. |
| `reason` | enum | `context_pressure`, `explicit`, `crash_recovery`. |
| `task_id` | string | Current task ID. |
| `summary` | text | Free-text summary of work done and remaining. |
| `decisions_made` | text[] | Decisions and their rationale. |
| `approaches_rejected` | text[] | Approaches tried and why they were abandoned. |
| `open_questions` | text[] | Unresolved questions for the successor. |
| `workspace_state` | enum | `clean`, `dirty`, `conflicted`. |
| `files_modified` | string[] | Paths relative to workspace root. |
| `session_number` | integer | Monotonically incremented by each successor. |
| `created_at` | timestamp | ISO 8601. |

`[CTY-04]` The `decisions_made` and `approaches_rejected` fields are REQUIRED when `reason` is `context_pressure` or `explicit`.

### 11.4 Handoff Triggers

`[CTY-05]` Handoff MAY be triggered by: (a) the agent calling `done --handoff` when it detects context pressure, (b) an infrastructure hook before context truncation, or (c) the watchdog restarting a dead session.

### 11.5 Continuity Invariants

`[CTY-06]` A handoff MUST NOT change the worker's assignment. The task remains assigned throughout the transition.

`[CTY-07]` Multiple consecutive handoffs for the same assignment MUST be supported. There is no limit on session cycling for a single task.

### 11.6 Handoff Cleanup

`[CTY-08]` Each handoff overwrites the previous handoff file. To preserve handoff history, the orchestrator SHOULD append each handoff to a history file before overwriting. The history file is informational and MUST NOT be required for session recovery.

`[CTY-09]` When a task completes (via `done`) or fails (via `fail`), the orchestrator SHOULD delete the handoff file from the workspace. The history file SHOULD be retained until workspace reset.

---

## 11a Failure Recovery

### 11a.1 Admission Control

`[RCV-01]` Before spawning a new session, the orchestrator MUST verify that infrastructure dependencies (ledger, process host, disk) have sufficient capacity.

`[RCV-02]` The orchestrator SHOULD enforce a configurable maximum number of concurrent active workers to prevent resource exhaustion.

### 11a.2 Assignment Durability

`[RCV-03]` A session crash MUST NOT cause assignment loss. The ledger record MUST persist through any number of session restarts.

`[RCV-04]` A new session for the same worker MUST be able to recover the assignment from the ledger without external intervention.

### 11a.3 Orphan Detection

`[RCV-05]` The orchestrator MUST provide a mechanism to detect orphaned processes — sessions whose parent management structures no longer reference them.

`[RCV-06]` Orphan cleanup MUST verify process identity (e.g., PID combined with `session_started_at`) before termination to prevent killing unrelated processes that reused a PID.

### 11a.4 Graceful Degradation

`[RCV-07]` If the ledger becomes unavailable, active workers SHOULD continue operating on their current task. The workspace is local; work in progress is not lost.

`[RCV-08]` During a ledger outage, agents SHOULD queue heartbeat writes locally. When the ledger recovers, agents MUST flush queued data.

`[RCV-09]` During a ledger outage, the watchdog MUST enter a degraded mode: process liveness checks continue, but ledger-dependent operations (assignment, dispatch, escalation tracking) are suspended.

`[RCV-10]` The watchdog SHOULD use exponential backoff when polling for ledger recovery. After a configurable maximum retry count, the watchdog MUST alert the operator.

---

## 12 Pipeline Model

This section defines the structural types for pipeline definitions. A pipeline is a declarative description of a multi-phase analysis workflow. It is inert until expanded into a task graph (Section 13).

### 12.1 Pipeline Definition

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier. |
| `output_dir` | path | Root directory for all pipeline outputs. |
| `prerequisites` | path[] | Input artifacts that must exist before the pipeline can start. |
| `gap_tolerance` | integer | Maximum non-critical agent failures before forced abort. |
| `phases` | Phase[] | Ordered sequence of phases. |
| `lock` | LockConfig | Pipeline-level concurrency protection. Enforces safety property S7 (Section 18). |

**LockConfig:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | path | `{output_dir}/.in-progress.lock` | Lock file location. |
| `staleness_threshold` | duration | 4 hours | Age after which a lock is considered stale and reclaimable. |
| `retry_count` | integer | 3 | Attempts to acquire the lock before reporting contention. |
| `retry_delay` | duration | 100 ms | Delay between acquisition attempts. |

### 12.2 Phase Definition

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier within the pipeline. |
| `topology` | enum | `sequential`, `parallel`, or `consensus`. |
| `boundary` | enum | `hard`, `soft`, or `none`. |
| `agents` | Agent[] | Ordered list of agent definitions. |
| `gate` | QualityGate? | Optional quality gate evaluated after all agents complete. |
| `consensus` | ConsensusConfig? | REQUIRED if topology is `consensus`; MUST be absent otherwise. |

### 12.3 Agent Definition

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier within the pipeline. |
| `model` | enum | `opus`, `sonnet`, or `haiku`. |
| `tools` | string set | Set of tool names available to the agent. |
| `skills` | string set | Set of skill names injected into the agent's context. |
| `criticality` | enum | `critical` or `non_critical`. |
| `inputs` | path[] | Artifacts this agent reads. |
| `output` | path | The single artifact this agent writes. |
| `format` | enum | `md`, `jsonl`, `json`, or `yaml`. |
| `id_prefix` | string? | Namespace prefix for identifiers this agent creates. |
| `max_turns` | integer? | Context budget limit. |

### 12.4 Topology Enumeration

| Value | Semantics |
|-------|-----------|
| `sequential` | Agents execute one after another in declaration order. Each agent's output is available to the next. |
| `parallel` | Agents execute concurrently with no inter-agent dependencies within the phase. |
| `consensus` | Each agent undergoes a consensus round: N independent instances, content merge, conditional verification. |

### 12.5 Boundary Enumeration

| Value | Semantics |
|-------|-----------|
| `hard` | Execution MUST pause after this phase. Requires explicit re-invocation to continue. |
| `soft` | Execution SHOULD pause if context budget is low. MAY continue if sufficient context remains. |
| `none` | Execution proceeds to the next phase without interruption. |

### 12.6 Consensus Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `instance_count` | integer | 3 | Number of independent instances per agent. MUST be ≥ 2. |
| `batch_size` | integer | 3 | Maximum number of concurrent consensus rounds within a phase. |
| `merge_agent` | string | — | Agent identifier for the content merge sub-phase. |
| `verify_agent` | string | — | Agent identifier for the verification sub-phase. |
| `thresholds.high` | rate | 85.0 | Above this rate, verification is skipped. |
| `thresholds.low` | rate | 30.0 | Below this rate, all findings are marked disputed. |
| `thresholds.similarity` | real ∈ [0,1] | 0.80 | Minimum similarity for finding matching. |

### 12.7 Quality Gate Definition

| Field | Type | Description |
|-------|------|-------------|
| `agent` | string | The agent whose output is evaluated. |
| `halt` | boolean | If true, a failing gate halts the pipeline until human override. |

---

## 13 Pipeline-to-Ledger Expansion

### 13.1 Expansion as Deterministic Function

`[EXP-01]` The orchestrator MUST expand a pipeline definition into a task graph in the assignment ledger before execution begins. Expansion is a pure, deterministic function of the pipeline definition: given the same input, it produces the same task graph. No judgment is involved.

`[EXP-02]` The orchestrator MUST validate all well-formedness constraints (Section 14) before expansion. Expansion of a non-well-formed pipeline MUST be rejected.

### 13.2 Pipeline to Root Task

`[EXP-03]` A pipeline MUST expand to a single root parent task with `type=pipeline`. The pipeline definition is recorded in the task's `body` or `metadata`.

### 13.3 Phase to Task Group

`[EXP-04]` Each phase MUST expand to a phase task with `type=phase` and `parent_task_id` pointing to the pipeline root task.

`[EXP-05]` Phase tasks MUST have a `depends_on` edge to the preceding phase task, enforcing sequential phase ordering. The first phase has no phase-level dependency.

### 13.4 Agent to Leaf Task

`[EXP-06]` For **sequential** topology: each agent expands to a leaf task with `type=agent`. Agent tasks are chained: agent *j* depends on agent *j−1* within the same phase.

`[EXP-07]` For **parallel** topology: each agent expands to a leaf task with `type=agent`. Agent tasks have no inter-agent dependencies. All become ready for dispatch simultaneously when the phase's dependencies are satisfied.

### 13.5 Consensus Round to Instance-Merge-Verify Tasks

`[EXP-08]` For **consensus** topology, each agent *a* in the phase expands to a consensus round consisting of:

- **N instance tasks** (`type=consensus-instance`), one per instance suffix, with no inter-instance dependencies.
- **One merge task** (`type=consensus-merge`) that depends on all N instance tasks.
- **One verify task** (`type=consensus-verify`) that depends on the merge task.

`[EXP-09]` The verify task MUST always exist in the task graph. Whether verification actually runs is determined by rule-based conditionals within the verify agent's execution (consensus rate vs. thresholds, per Section 16.6). The orchestrator does not inspect merge output to decide whether to create the verify task.

### 13.6 Gate as Phase Completion Constraint

`[EXP-10]` A quality gate is modeled through the phase task's parent-child status derivation (Section 6.6). The gate agent is one of the leaf tasks within the phase. If the gate agent's task status is `failed` and `halt=true`, the phase task's status becomes `failed`, blocking the next phase via its dependency edge.

`[EXP-11]` To advance past a halting gate failure, a human or triage worker sets the gate agent's task status to `overridden`. Since `overridden` is a terminal status, the phase task can then complete, unblocking the next phase.

### 13.7 Boundary as Deferred Gate Task

`[EXP-12]` A phase with `boundary=hard` MUST include an additional task with `type=boundary-gate` and initial `status=deferred`. The next phase's phase task MUST depend on this boundary-gate task. Since `deferred` is non-terminal, the dependency blocks until a human or external system transitions the boundary-gate to `completed`.

`[EXP-13]` A phase with `boundary=soft` MUST include a boundary-gate task that the orchestrator automatically transitions to `completed` if a context budget check indicates sufficient capacity. If capacity is insufficient, the task remains `deferred` until re-invocation.

### 13.8 Batch Ordering

`[EXP-14]` When a consensus phase has more agents than `batch_size`, rounds are partitioned into batches. All tasks in batch *b+1* MUST depend on the verify tasks of all rounds in batch *b*. This serializes batches while allowing concurrency within each batch.

---

## 14 Well-Formedness Constraints

A pipeline *P* is well-formed iff all of the following hold. The orchestrator MUST verify these before expansion.

`[WFC-01]` **Unique agent outputs.** No two agents in a pipeline write to the same path.

`[WFC-02]` **Phase ordering forms a DAG.** Every agent input references only pipeline prerequisites or outputs from agents in earlier phases.

`[WFC-03]` **Consensus requires configuration.** Every phase with `topology=consensus` MUST have a non-absent `consensus` field.

`[WFC-04]` **Singleton forbids configuration.** Every phase with topology other than `consensus` MUST have an absent `consensus` field.

`[WFC-05]` **Threshold ordering.** For every consensus configuration: `0 ≤ low < high ≤ 100` and `0 < similarity ≤ 1`.

`[WFC-06]` **Instance count bounds.** For every consensus configuration: `instance_count ≥ 2`.

`[WFC-07]` **Gate agent membership.** A gate's agent MUST belong to the same phase or an earlier phase.

`[WFC-08]` **Critical agent completeness.** An agent MUST be classified as `critical` if and only if: (a) its output is the sole input dependency for another agent, or (b) it is a gate agent.

`[WFC-09]` **Unique identifier prefixes.** No two agents with non-absent `id_prefix` values share the same prefix.

`[WFC-10]` **Gap tolerance bounds.** `gap_tolerance` MUST be ≥ 1 and ≤ the count of non-critical agents.

`[WFC-11]` **Lock path containment.** Any pipeline-level lock file MUST reside within the output directory.

`[WFC-12]` **Consensus lock threshold ordering.** Consensus phases that use a separate finer-grained lock to protect concurrent state updates (e.g., round status tracking) MUST configure a staleness threshold strictly less than the pipeline-level lock staleness threshold. RECOMMENDED: 15 minutes for consensus state locks vs. 4 hours for the pipeline lock.

---

## 15 Operational Semantics

### 15.1 Dispatch Loop

`[OPS-01]` The orchestrator's dispatch loop MUST:

1. Query the ledger for tasks with `status=open` whose dependencies have all reached a terminal status.
2. Query the ledger for workers with `status=idle`.
3. For each ready task (sorted by priority, with deterministic tiebreaker), assign it to an available idle worker.
4. Repeat.

> *Note (non-normative):* The dispatch loop does not need to understand phase topology. Expansion (Section 13) has already encoded topology semantics into dependency edges. Sequential agents chain dependencies. Parallel agents share no dependencies. Consensus rounds express the instance→merge→verify pipeline through dependencies.

### 15.2 Phase Execution as Dependency Resolution

`[OPS-02]` Phase progression MUST be driven entirely by task dependency resolution. When all leaf tasks under a phase task reach terminal status, the phase task transitions per Section 6.6. The next phase's tasks become eligible for dispatch when their dependency on the previous phase task is resolved.

### 15.3 Agent Invocation and Output Validation

`[OPS-03]` Before invoking an agent, the orchestrator MUST validate that all declared inputs exist and are non-empty.

`[OPS-04]` After an agent completes, the orchestrator MUST validate the output:

| Format | Validation |
|--------|-----------|
| `md` | File exists, size > 100 bytes, contains a markdown header. |
| `jsonl` | File exists, first line is parseable, last line indicates completion. |
| `json` | File exists, content parses as valid JSON. |
| `yaml` | File exists, content parses as valid YAML. |

### 15.4 Gate Evaluation

```
  gate absent
  ──────────────────  [NO-GATE]
  phase proceeds

  gate present    agent completed successfully
  ──────────────────  [GATE-PASS]
  phase proceeds

  gate present    agent failed    halt = false
  ──────────────────  [GATE-WARN]
  warning recorded, phase proceeds

  gate present    agent failed    halt = true
  ──────────────────  [GATE-HALT]
  phase blocked — requires human override (status → overridden)
```

### 15.5 Session Boundary Interaction

`[OPS-05]` Phase boundaries and session handoffs compose as follows: a worker assigned an agent task may undergo any number of session restarts (via handoff, Section 11) during that task's execution. The phase boundary determines what happens *after* the phase completes, not during individual task execution.

```
  boundary = hard    phase complete
  ──────────────────  [BOUNDARY-HARD]
  boundary-gate task remains deferred → pipeline pauses

  boundary = soft    phase complete    context budget sufficient
  ──────────────────  [BOUNDARY-SOFT-CONTINUE]
  boundary-gate task → completed → next phase proceeds

  boundary = soft    phase complete    context budget insufficient
  ──────────────────  [BOUNDARY-SOFT-STOP]
  boundary-gate task remains deferred → pipeline pauses

  boundary = none    phase complete
  ──────────────────  [BOUNDARY-NONE]
  next phase proceeds immediately
```

### 15.6 Resume from Persisted State

`[OPS-06]` On re-invocation, the orchestrator MUST reconstruct pipeline state by querying the ledger for the pipeline's task graph. Completed phases (all leaf tasks terminal) are skipped. The first phase with non-terminal leaf tasks is the resume point.

`[OPS-07]` The orchestrator MUST reconcile ledger state against filesystem state on resume. If an agent's output file exists and is valid but the task status is not `completed`, the orchestrator MUST correct the task status to `completed`. If a task status is `completed` but the output file is absent or corrupt, the orchestrator MUST reset the task status.

### 15.7 Context Budget

`[OPS-08]` Agent invocations SHOULD be bounded by a configurable turn limit to prevent context window overflow.

`[OPS-09]` If an agent fails with a context overflow signal, the retry protocol (Section 17.1) SHOULD reduce the turn budget to 60-75% of the original.

### 15.8 Finalization

`[OPS-10]` When a pipeline reaches a terminal state (all phases complete, or gap tolerance exceeded), the orchestrator MUST:

1. Run a writing quality pass on all markdown output files, applying prose standards (active voice, omit needless words, positive form, specific language). This pass edits files in-place, preserving content structure and identifiers. Failure is non-blocking.
2. Archive the audit trail to a permanent, timestamped file.
3. Generate a summary of execution results.
4. Clean up transient artifacts if the pipeline completed fully. If the pipeline terminated due to gap tolerance, transient artifacts MUST be retained for potential resume.

`[OPS-11]` Finalization steps are best-effort: failure in any step MUST NOT prevent subsequent steps.

### 15.9 Audit Trail

`[OPS-12]` The orchestrator MUST emit structured audit events throughout execution. Events MUST NOT be stored in the assignment ledger.

`[OPS-13]` Mandatory emission points:

| Event | When |
|-------|------|
| Phase start | A phase begins execution. |
| Agent start | An agent is invoked. |
| Agent complete | An agent finishes (success or failure). |
| Consensus merge | A content merge completed with consensus rate. |
| Consensus verify | A verification completed with verdicts. |
| Decision | An architectural or classification decision within an agent. |
| Gate result | A quality gate is evaluated. |
| Gap recorded | A non-critical failure is accepted. |
| Pipeline complete | Terminal state reached. |

`[OPS-14]` Agents SHOULD embed decision records within their output files using a structured marker format. These markers are extractable by audit aggregation and are informational — they do not affect execution semantics.

---

## 16 Consensus Protocol

### 16.1 Finding Similarity

Two findings *f* and *g* from different instances are matchable iff they share an exact identifier match or their normalized similarity meets the threshold:

```
matchable(f, g) ≡ exact_id_match(f, g) ∨ similarity(f, g) ≥ threshold
```

Similarity is computed as normalized edit distance:

```
similarity(f, g) ≡ 1 − edit_distance(normalize(f), normalize(g)) / max(|f|, |g|)
```

> *Note (non-normative):* The `matchable` relation is reflexive and symmetric but NOT transitive. This specification resolves the non-transitivity problem through anchor-based matching (Section 16.2).

### 16.2 Finding Groups

`[CON-01]` The content merge agent MUST construct finding groups using a greedy matching algorithm anchored on a canonical instance. The anchor instance is determined by a fixed ordering of instance suffixes (e.g., alphabetical). For each anchor finding, the merge agent scans unmatched findings in other instances for the best match above the similarity threshold.

`[CON-02]` Unmatched findings from non-anchor instances become singleton groups.

`[CON-03]` Matching MUST be deterministic: given the same set of instance outputs, the same finding groups are produced regardless of argument order.

### 16.3 Consensus Classification

Each finding group receives a classification:

| Classification | Condition |
|---------------|-----------|
| **Unanimous** | All N instances contributed a finding. |
| **Majority** | More than 1 but fewer than N instances contributed. |
| **Unique** | Exactly 1 instance contributed. |

> *Note (non-normative):* For N=2 (degraded mode), there is no majority category. Findings are either unanimous (2) or unique (1).

### 16.4 Consensus Rate

```
rate = |{groups classified unanimous or majority}| / |{all groups}| × 100
```

`[CON-04]` If no findings exist across any instance (`|groups| = 0`), the rate is 0 and the orchestrator MUST signal this condition and halt for human decision (retry or abort).

### 16.5 Attribute Resolution

`[CON-05]` When instances agree on a finding's existence but disagree on an attribute value, the merge agent MUST apply majority voting. If no majority exists, the anchor instance's value is selected.

### 16.6 Round Semantics

A consensus round proceeds through three sub-phases:

**Sub-phase 1 — Instances:**

```
  instance_count = N
  ──────────────────  [INST-LAUNCH]
  Launch N instances concurrently.
  Let S = count of successful instances.

  S = N     → mode = normal
  S = N−1   → mode = degraded
  S < N−1 and critical   → retry failed instances, then re-evaluate
  S < N−1 and non-critical → mode = failed (record gap)
```

**Sub-phase 2 — Content Merge:**

```
  mode ∈ {normal, degraded}
  all successful instances complete
  ──────────────────  [MERGE]
  Invoke merge agent with successful instance outputs.
  Record consensus rate and unique finding count.
```

**Sub-phase 3 — Verification (conditional):**

```
  unique_count > 0    low ≤ rate < high
  ──────────────────  [VERIFY]
  Invoke verify agent to evaluate unique findings.

  rate ≥ high
  ──────────────────  [VERIFY-SKIP-HIGH]
  Verification skipped — high consensus.

  unique_count = 0
  ──────────────────  [VERIFY-SKIP-NONE]
  Verification skipped — no unique findings.

  rate < low
  ──────────────────  [VERIFY-DISPUTED]
  All findings marked disputed.
  Requires human acknowledgment before pipeline advances.
```

### 16.7 Batched Consensus

`[CON-06]` When a phase contains multiple agents under consensus topology, rounds execute in parallel up to the `batch_size`. Batches are processed sequentially: batch *b+1* starts only after batch *b* completes.

`[CON-07]` The total number of concurrent agent invocations within a batch is `|batch| × instance_count`.

### 16.8 Verification Tag Lifecycle

Unique findings carry verification tags that track provenance:

| Tag | Meaning |
|-----|---------|
| `unverified` | Assigned at merge to all unique findings. |
| `verified_unique` | Verifier confirmed the finding. |
| (removed) | Verifier rejected the finding — it is deleted from the output. |
| (unchanged) | Verifier returned uncertain — finding remains `unverified`. |
| `disputed` | Consensus rate below low threshold — immutable until human review. |
| `verification_timeout` | Verifier did not complete. |

`[CON-08]` Tags are informational annotations. Systems consuming pipeline outputs MUST NOT treat tagged findings differently from untagged findings for functional purposes. Tags exist for human reviewers assessing confidence.

### 16.9 Degraded Mode

`[CON-09]` Degraded mode (N−1 successful instances) is tracked per consensus round, not per pipeline. One round operating in degraded mode does not affect other rounds.

`[CON-10]` For N=2 (degraded from N=3), if the two instances disagree on every finding, all findings are unique, the consensus rate is 0%, and the `[VERIFY-DISPUTED]` rule applies.

---

## 17 Error Semantics

### 17.1 Retry Protocol

`[ERR-01]` When a critical agent fails and retries remain (RECOMMENDED maximum: 2), the orchestrator MUST create a new retry task with a scaled timeout (RECOMMENDED: base × (1.0 + 0.5 × attempt_number)).

`[ERR-02]` Retry task creation is a deterministic response to a task status change. It does not require judgment and belongs in the orchestrator.

`[ERR-03]` When all retries are exhausted for a critical agent, the orchestrator MUST halt with an actionable error message.

### 17.2 Watchdog-Retry Orthogonality

`[ERR-04]` The watchdog (Section 8) and the retry protocol operate at different levels and MUST NOT interfere:

| Mechanism | Scope | Trigger | Action | Effect on Task Status |
|-----------|-------|---------|--------|-----------------------|
| Watchdog | Session | Process death | Restart session | None — task remains `in_progress` |
| Retry | Task | Task failure (invalid output) | Create new attempt task | Original task `failed`, new task `open` |

`[ERR-05]` A session restart by the watchdog MUST NOT consume a retry attempt. A task retry MUST NOT restart a session.

### 17.3 Gap Accumulation

`[ERR-06]` Non-critical agent failures are recorded as gaps. Gaps accumulate monotonically across sessions.

`[ERR-07]` When the gap count reaches the pipeline's `gap_tolerance`, the orchestrator MUST abort.

### 17.4 Criticality Assignment Rules

`[ERR-08]` Criticality assignment MUST follow the rule defined in `[WFC-08]`. All agents not meeting the criticality criteria SHOULD be classified as `non_critical`.

---

## 18 Safety Properties

These properties MUST hold for all conforming implementations. Each property includes an enforcement sketch.

### 18.1 Monotonic Progress (S1)

Pipeline checkpoint state never regresses across sessions.

```
∀ session s₁ < s₂ : phase_index(s₁) ≤ phase_index(s₂)
```

*Enforcement:* Each session either advances the phase index (on success) or leaves it unchanged (on failure/gap). No operation decrements the index.

### 18.2 Instance Independence (S2)

Within a consensus round, no instance reads another instance's output.

```
∀ instances i, j in the same round where i ≠ j :
  output(i) ∉ inputs(j) ∧ output(j) ∉ inputs(i)
```

*Enforcement:* Instances write to distinct paths and read only from prior-phase merged outputs.

### 18.3 Content Merge Determinism (S3)

The merge function produces identical output given the same set of instance outputs, regardless of argument order.

*Enforcement:* The anchor instance is determined by a fixed ordering of suffixes. The greedy matching algorithm iterates the anchor's findings in document order.

*Caveat:* Merge determinism holds for a fixed similarity threshold. If the threshold changes between runs, groupings may differ.

### 18.4 No Silent Capability Loss (S4)

A pipeline that consumes functional capability identifiers in its prerequisites preserves all of them in its output.

```
functional_ids(prerequisites) ⊆ functional_ids(output)
```

*Enforcement:* Preservation gates are quality gates with `halt=true`. Only human override can accept capability loss.

### 18.5 Bounded Degradation (S5)

A pipeline produces output only if the gap count stays below the tolerance.

```
output_produced ⟹ |gaps| < gap_tolerance
```

### 18.6 Gate Authority (S6)

No agent or orchestrator may advance past a halting gate without human authorization.

```
gate.halt = true ∧ gate failed ⟹ pipeline blocked until human override
```

### 18.7 Pipeline Exclusion (S7)

At most one orchestrator instance executes a given pipeline at any time.

`[SPR-07]` The pipeline lock MUST be released on ALL exit paths — success, failure, abort, and interrupt. The orchestrator MUST register a cleanup handler at startup to guarantee release on abnormal termination.

*Enforcement:* Pipeline-level lock with exclusive-create semantics. Staleness threshold provides liveness for crashed orchestrators.

### 18.8 Cleanup Completeness (S8)

On successful pipeline completion, no transient artifacts remain (or cleanup failures are warned).

### 18.9 Assignment Durability (S9)

A session crash MUST NOT cause assignment loss. The ledger record persists through any number of session restarts. A new session for the same worker recovers the assignment from the ledger without external intervention.

### 18.10 Workspace Isolation Enforcement (S10)

No two workers can write to the same workspace simultaneously.

### 18.11 Watchdog-Retry Non-Interference (S11)

Session restarts (watchdog) and task retries (retry protocol) are orthogonal operations that never conflict or double-count.

---

## 19 Liveness Properties

### 19.1 Eventual Termination (L1)

Given a finite target and finite retry budget, every pipeline terminates.

```
∀ pipeline P, finite target T : ∃ session s : pipeline reaches terminal state
```

*Proof sketch:* Each phase either completes or exhausts retries. Both outcomes reduce remaining work. Phase count and retry count are finite.

### 19.2 Consensus Convergence (L2)

For any non-empty set of findings, the consensus protocol produces a classification in bounded steps.

*Bound:* Steps ≤ |anchor findings| × max(|other instance findings|).

### 19.3 Lock Release (L3)

Every acquired lock is eventually released, even on abnormal termination.

```
∀ lock acquisition at t₁ : ∃ t₂ > t₁ : lock is released
where t₂ − t₁ ≤ staleness_threshold
```

*Enforcement:* The orchestrator registers a cleanup handler on startup. If it crashes, the staleness threshold guarantees reclaimability.

---

## 20 Agent-Type Abstraction

### 20.1 Preset Registry

`[ATA-01]` The orchestrator MUST maintain a preset registry that maps agent type names to launch configurations.

`[ATA-02]` Each preset MUST define at minimum:

| Field | Description |
|-------|-------------|
| `command` | The binary to invoke. |
| `args` | Default arguments. |
| `process_names` | Process names for liveness checks. |
| `readiness_mode` | How to detect session readiness: `prompt_detection`, `delay_based`, or `api_check`. |
| `hooks_provider` | Which hook/settings format this agent type uses. |
| `prompt_mode` | How to deliver prompts: `arg`, `stdin`, or `none`. |
| `env` | Agent-specific environment variables. |

`[ATA-03]` Adding support for a new agent type MUST NOT require orchestrator source code changes. A new preset entry MUST be sufficient.

### 20.2 Per-Role Overrides

`[ATA-04]` The orchestrator SHOULD support per-role agent type overrides (e.g., different models for triage workers vs. standard workers).

`[ATA-05]` Per-project overrides SHOULD take precedence over system-wide defaults.

---

## 21 Observability

### 21.1 Heartbeat Protocol

`[OBS-01]` Active sessions SHOULD write a heartbeat to the ledger at a regular interval (RECOMMENDED: every 30-60 seconds).

`[OBS-01a]` A heartbeat write MUST update the `last_heartbeat` and `heartbeat_state` fields in the worker record.

`[OBS-02]` A heartbeat that has not been updated within a configurable staleness threshold (RECOMMENDED: 3 minutes) is considered stale. Staleness is an observation — the infrastructure MUST expose it but MUST NOT act on it unilaterally. Peer agents consume this signal and decide what to do.

### 21.2 Telemetry Correlation

`[OBS-03]` Each session MUST be assigned a unique run identifier at spawn time, propagated via environment variable to enable correlation of all telemetry events from that session.

### 21.3 Operational Dashboard

`[OBS-04]` The orchestrator SHOULD expose a read-only system state view including at minimum: worker states and heartbeat ages, task graph status, escalation count, and merge lock status.

---

## Appendix A Pipeline Archetypes (Non-Normative)

Pipelines instantiate five structural archetypes. An archetype constrains certain properties of the `Pipeline` type.

### A.1 Consensus Analysis

The dominant archetype. A consensus scout phase, followed by batched consensus view phases, sequential quality assessment, parallel synthesis, and singleton evaluation.

```
[consensus scout] → [batched consensus views]* → [sequential quality]
    → [parallel synthesis] → [singleton evaluation]
```

### A.2 Sequential Consensus

Each consensus round depends on the prior round's merged output. Used when analysis is inherently sequential (e.g., each phase refines the previous).

```
[consensus phase₁] → [consensus phase₂] → ... → [singleton synthesis]
```

### A.3 Singleton Synthesis

No consensus phases. Consumes outputs from a prior pipeline. Contains at least one halting gate.

```
[singleton scout] → [parallel batch₁] → [parallel batch₂] → [gate] → [synthesis]
```

### A.4 Sequential Planning

Heavy inter-phase data dependencies. Sequential agent chains with a halting gate at the end.

```
[singleton scout] → [sequential a₁ → a₂ → ... → aₙ] → [synthesis] → [halting gate]
```

### A.5 Hybrid

Transitions from consensus phases to singleton phases within a single pipeline.

---

## Appendix B Model Selection Heuristics (Non-Normative)

| Task Characteristic | Recommended Model |
|--------------------|-------------------|
| Cross-domain reasoning (inputs from ≥ 3 views), semantic transformation, foundation/overview production | Most capable (`opus`) |
| Binary pass/fail validation, pattern-matching integrity checks | Fastest (`haiku`) |
| Standard analysis, documentation, history, validation | Balanced (`sonnet`) |

---

## Appendix C Session Count Estimation (Non-Normative)

For capacity planning:

| Phase Topology | Estimated Sessions |
|---------------|-------------------|
| Consensus (hard boundary) | Per batch: instances session + merge session + verify session (conditional, ~30% frequency) |
| Parallel | 1 |
| Sequential | ⌈agents / agents_per_session⌉ |

---

## Appendix D Decision Authority Matrix (Non-Normative)

| Operation | Authority | Mechanism |
|-----------|-----------|-----------|
| Retire or add capabilities | Human | Manual review |
| Override halting gate | Human | Set task status to `overridden` |
| Accept disputed findings | Human | Acknowledgment at session boundary |
| Exceed gap tolerance | Human | Override prompt |
| Change technology stack target | Human | Target specification edit |
| Force-abort running pipeline | Human | Interrupt signal |
| All other execution decisions | Orchestrator | Automatic per this spec |

---

## Appendix E Anti-Patterns (Non-Normative)

| Anti-Pattern | Violation | Consequence |
|-------------|-----------|-------------|
| **Smart orchestrator** — infrastructure decides retry/skip/escalate based on output content | DSN-01, DSN-02 | Heuristics break when agent behavior changes |
| **Output parsing** — orchestrator reads agent output to decide next action | DSN-03 | Fragile coupling to output format |
| **Shared workspace** — agents edit the same files | WSP-03 | Merge conflicts, race conditions, corrupted state |
| **Task-only assignment** — work conveyed only in task body with no durable record | WKR-03 | Crash loses assignment |
| **Silent workspace purge** — workspace reset without checking for unmerged changes | WKR-09 | Unmerged work destroyed |
| **Unbounded escalation** — escalation tasks accumulate with no alerting threshold | SUP-09 | System silently degrades |
| **Prose-only handoff** — handoff context is unstructured free text | CTY-03 | Successor cannot reliably parse decisions or open questions |
| **Ledger bypass** — agent writes directly to ledger storage | ITF-05 | Breaks concurrency control |
| **Ledger-as-cache** — high-frequency ephemeral data stored in the ledger | DSN-07 | Overloads the durable store |
| **Agent-specific orchestrator code** — `if agent == "X" then ...` | ATA-03 | Every new agent type requires orchestrator changes |
| **Conflating merge types** — treating content merge and workspace merge as a single operation | DSN-09 | Incorrect locking, incorrect retry semantics |

