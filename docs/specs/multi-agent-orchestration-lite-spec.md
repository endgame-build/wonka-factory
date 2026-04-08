# Multi-Agent Orchestration Architecture Specification (Lite Profile)

**Version:** 0.2.0-draft
**Status:** Draft
**Authors:** ENDGAME
**Date:** 2026-03-26
**Profile of:** Multi-Agent Orchestration Architecture Specification v0.2.0

---

## 1. Introduction

### 1.1 Purpose

This specification defines a simplified architecture for orchestrating autonomous AI reasoning agents. It addresses the coordination of processes that are opaque (internal state cannot be inspected), unreliable (may crash, loop, or diverge), autonomous (interpret rather than execute instructions), and heterogeneous (different implementations with different interfaces).

### 1.2 Scope

This specification covers:
- The separation of deterministic infrastructure from nondeterministic cognition
- Work assignment, lifecycle management, and failure recovery
- Task dependencies and task graph dispatch
- Session continuity across context window boundaries
- Workspace isolation and merge-back protocols
- Agent-type abstraction and extensibility
- Concurrency control and graceful degradation

This specification does not cover:
- The internal behavior of any specific agent implementation
- Domain-specific workflow semantics (e.g., what constitutes "good code")
- Authentication, authorization, or multi-tenancy
- Network topology or deployment infrastructure beyond process hosting
- Inter-agent messaging (see Section 15.2 for extension path)
- Multi-scope organizational hierarchy (see Section 15.2 for extension path)
- Ephemeral notification channels (see Section 15.2 for extension path)
- Cost tier management (see Section 15.2 for extension path)

### 1.3 Relationship to Full Specification

This document is a **profile** of the Multi-Agent Orchestration Architecture Specification v0.2.0 (hereafter "the full spec"). It is not a fork. Every normative requirement in this profile is derived from or compatible with the full spec. Where this profile simplifies, it names what was removed and provides an upgrade path through extension points (Section 15.2).

An implementation that conforms to this profile can adopt full-spec sections incrementally without rewriting its core.

### 1.4 Conventions

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

### 1.5 Definitions

| Term | Definition |
|------|-----------|
| **Agent** | An autonomous reasoning process that interprets instructions, makes judgment calls, and produces artifacts. Opaque to the orchestrator. |
| **Session** | A single OS-level execution of an agent process. Ephemeral. Has a finite context window. |
| **Worker** | A named entity with persistent identity to which work is assigned. A worker's identity outlives any single session. |
| **Orchestrator** | The deterministic infrastructure layer that manages process lifecycles and state persistence. |
| **Watchdog** | A deterministic process that monitors agent liveness and restarts dead sessions. Not an agent. |
| **Assignment Ledger** | A unified durable store containing the authoritative record of workers, tasks, and heartbeats. |
| **Heartbeat** | A timestamped signal written by a session to the ledger to indicate liveness. |
| **Workspace** | An isolated filesystem directory in which a single worker operates. |
| **Preset** | A configuration record describing how to launch, detect, and communicate with a specific agent type. |
| **Task Graph** | A set of tasks with dependency edges. Replaces workflow templates and bundles from the full spec. |
| **Handoff** | The transfer of working context from a dying session to its successor via a workspace file. |
| **Escalation Task** | A standard task with `type=escalation` created to surface a problem for human or triage-worker review. |
| **Merge Lock** | A mutual exclusion mechanism that serializes the integration of worker changes into the canonical source. |

---

## 2. Design Rationale

### 2.1 Principles

**One store.** The assignment ledger holds all durable state: tasks, workers, and heartbeats. There are no separate heartbeat files, message stores, or workflow state trackers.

**Zero communication channels.** Workers do not message each other. They discover work via the ledger (`orch prime`), report results via the ledger (`orch done`, `orch fail`), and escalate problems via the ledger (`orch escalate`). The ledger is the sole coordination mechanism.

**One merge mechanism.** A mutex around git operations. No separate merge queue abstraction.

**No abstraction without duplication.** If a concept can be modeled as a task in the ledger, it does not get its own abstraction. Workflow phases are tasks. Bundles are parent tasks. Escalations are tasks.

### 2.2 Simplicity Test

For any proposed new abstraction, apply:

> *"Can this be modeled as a task in the ledger? If yes, don't create a new abstraction."*

### 2.3 Architecture Overview

```
┌───────────────────────────────────────────────────────┐
│                     WATCHDOG                           │
│  (deterministic process — restarts the dead,           │
│   counts escalations, alerts operator)                 │
└──────────────────┬────────────────────────────────────┘
                   │ restarts dead sessions
         ┌─────────┴──────────┐
         ▼                    ▼
   ┌───────────┐        ┌───────────┐
   │ Worker A  │        │ Worker B  │
   │           │        │           │
   │ worktree  │        │ worktree  │
   │ .handoff  │        │ .handoff  │
   └─────┬─────┘        └─────┬─────┘
         │                     │
         │  orch prime/done    │
         │  orch status/fail   │
         ▼                     ▼
   ┌───────────────────────────────────┐
   │        ASSIGNMENT LEDGER          │
   │  (workers, tasks, dependencies    │
   │   — single durable store)         │
   └──────────────┬────────────────────┘
                  │ orch done (synchronous merge)
                  ▼
   ┌───────────────────────────────────┐
   │         MERGE LOCK (mutex)        │
   │  acquire → rebase → test → merge  │
   └──────────────┬────────────────────┘
                  ▼
           canonical branch
```

Workers never communicate directly. All coordination flows through the ledger.

### 2.4 Trade-offs Accepted

| Trade-off | Impact | Mitigation |
|-----------|--------|------------|
| No inter-agent messaging | Workers cannot send ad-hoc messages to each other | All coordination is task-based; adopt full spec Section 5 for messaging |
| No autonomous cognitive supervision | Escalation goes to humans or triage workers, not self-healing supervisor agents | Simpler to operate; adopt full spec Section 7 when autonomous recovery is needed |
| Ledger is single point of failure | All durable state in one store | Simpler to back up, replicate, and monitor than multiple stores; workers continue locally during outages (Section 12.5) |
| Merge protocol assumes git | Non-git workspaces need custom merge lock implementation | Git worktrees are the dominant pattern; spec provides equivalent-semantics escape hatch |
| No multi-project scope hierarchy | Workers and tasks share a flat namespace | Sufficient for single-team use; adopt full spec Section 14 for multi-project |

### 2.5 Upgrade Path

Each extension point in Section 15.2 maps to a specific full-spec section. Adopting an extension does not require rewriting core sections of this profile.

---

## 3. Zero Framework Cognition

### 3.1 Statement

The orchestrator MUST NOT perform reasoning, interpretation, or judgment. Agents MUST NOT perform process management, merge operations, or lifecycle coordination.

### 3.2 Normative Requirements

**3.2.1** The orchestrator MUST NOT contain conditional logic predicated on agent output content, task semantics, or domain-specific heuristics.

**3.2.2** The orchestrator MUST NOT hardcode behavioral thresholds that determine agent lifecycle actions (e.g., "restart after N minutes idle"). The orchestrator MAY detect and expose quantitative signals (e.g., "heartbeat age is N minutes"). The decision to act on those signals belongs to agents or human operators.

**3.2.3** The orchestrator MUST NOT parse, interpret, or route based on the content of agent-generated artifacts (stdout, files, logs).

**3.2.4** Agents MUST NOT manage their own process lifecycle or the lifecycle of other agents directly. Agents interact with the orchestrator exclusively through the interfaces defined in Section 6.

**3.2.5** When a capability requires judgment, it MUST be delegated to an agent or a human operator — not implemented as orchestrator logic.

### 3.3 Compliance Test

For any proposed orchestrator logic, apply:

> *"Can this be expressed as a deterministic function with no judgment calls?"*

If yes, it belongs in the orchestrator. If no, it belongs in an agent.

---

## 4. Worker Identity Model

### 4.1 Identity Structure

Each worker MUST have:

| Property | Durability | Description |
|----------|-----------|-------------|
| `name` | Permanent | A stable, human-readable identifier unique within the project. |
| `assignment` | Durable | A record in the assignment ledger linking the worker to zero or one active task. |
| `workspace` | Durable | An isolated filesystem directory that persists across sessions. |
| `history` | Durable | An ordered record of past assignments. |
| `session` | Ephemeral | Zero or one active OS process. |

### 4.2 Identity Invariants

**4.2.1** A worker's identity MUST persist independently of any session. Session termination MUST NOT destroy the worker's name, assignment, workspace, or history.

**4.2.2** There MUST be at most one active session for a given worker at any time.

**4.2.3** A worker's assignment MUST be recoverable from the assignment ledger alone, without access to the session's internal state.

### 4.3 Lifecycle States

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

**4.3.1** A worker in IDLE state MUST have no active session but MAY have an assignment and workspace.

**4.3.2** A worker in ACTIVE state MUST have exactly one active session.

**4.3.3** Transition from ACTIVE to IDLE MUST NOT modify the assignment ledger. The assignment persists so a new session can recover it.

### 4.4 Worker Pool Reuse

**4.4.1** When new work arrives, the orchestrator SHOULD attempt to reuse an IDLE worker before allocating a new one.

**4.4.2** Reuse MUST reset the workspace to a clean state (e.g., `git reset --hard`, directory purge) before assigning new work. This applies only when assigning *new* work to a reused worker. Session cycling for continuity on the *same* task (Section 10) MUST NOT reset the workspace.

**4.4.3** Reuse MUST preserve the worker's name and history chain.

**4.4.4** Before resetting a workspace for reuse, the orchestrator MUST verify that the workspace contains no unmerged changes. If the workspace is dirty (uncommitted changes, unmerged branch), the orchestrator MUST create an escalation task (Section 7.2) and MUST NOT reset the workspace.

### 4.5 Worker Deallocation

**4.5.1** An IDLE worker MAY be deallocated to reclaim resources (workspace disk, name pool slot).

**4.5.2** Deallocation MUST be triggered by explicit action (operator command or capacity-based eviction policy). The orchestrator MUST NOT deallocate workers implicitly on a timer.

**4.5.3** Upon deallocation, the orchestrator MUST release the worker's name for future allocation, remove the workspace, and retain the history record.

**4.5.4** A worker with an active assignment MUST NOT be deallocated. The assignment must be completed, failed, or reassigned before deallocation.

---

## 5. Assignment Ledger

### 5.1 Unified Schema

The assignment ledger MUST be a durable store (database, append-only log, or equivalent) that survives orchestrator restarts and agent crashes. The ledger is the **single source of truth** for all orchestration state.

The ledger MUST contain at minimum the following entity types:

**Workers:**

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique identifier for the worker. |
| `status` | enum | One of: `idle`, `active`. |
| `last_heartbeat` | timestamp | When the worker last signaled liveness. |
| `heartbeat_state` | enum | Agent-reported state: `working`, `idle`, `exiting`. |
| `current_task_id` | string \| null | Active assignment, or null if unassigned. |
| `session_pid` | integer \| null | OS process ID of active session. |
| `session_started_at` | timestamp \| null | When the current session was spawned. |

**Worker History:**

| Field | Type | Description |
|-------|------|-------------|
| `worker_name` | string | The worker this record belongs to. |
| `task_id` | string | The task that was assigned. |
| `started_at` | timestamp | When the assignment began. |
| `ended_at` | timestamp | When the assignment ended (completion, failure, or reassignment). |
| `outcome` | enum | One of: `completed`, `failed`, `reassigned`. |

**Tasks:**

| Field | Type | Description |
|-------|------|-------------|
| `task_id` | string | Unique identifier for the task. |
| `parent_task_id` | string \| null | Parent task, or null for top-level tasks. |
| `type` | string | Task type tag (e.g., `work`, `escalation`, `triage`). Used for filtering and routing, not for orchestrator logic. |
| `status` | enum | One of: `open`, `assigned`, `in_progress`, `completed`, `failed`, `deferred`. Terminal statuses are `completed` and `failed`. |
| `assignee` | string \| null | Worker name, or null if unassigned. |
| `priority` | integer | Dispatch priority. Lower values indicate higher priority. |
| `body` | text \| null | Task description, instructions, or context. Maximum 32 KB. Null for tasks where the ledger entry is sufficient. |
| `created_at` | timestamp | When the task was created. |
| `updated_at` | timestamp | When the record was last modified. |

**Task Dependencies:**

| Field | Type | Description |
|-------|------|-------------|
| `task_id` | string | The blocked task. |
| `depends_on` | string | The blocking task. |

### 5.2 Task Graphs

**5.2.1** A task with a non-null `parent_task_id` is a **child task**. A parent task's status is derived — see Section 5.8.

**5.2.2** Task dependencies define execution ordering. A task whose `depends_on` entries include tasks that have not all reached a terminal status MUST NOT be dispatched to a worker. Only tasks whose blockers have all reached a terminal status are eligible for assignment.

**5.2.3** A `deferred` task is non-terminal: it may be resumed later. A dependency on a deferred task remains unresolved. This prevents downstream work from proceeding on incomplete prerequisites.

**5.2.4** The orchestrator MUST reject dependency edges that would create a cycle. Cycle detection MUST be performed at dependency creation time, not at dispatch time.

**5.2.5** Among tasks of equal priority that are ready for dispatch, the orchestrator SHOULD use a deterministic tiebreaker (e.g., lexicographic task ID order).

**5.2.6** A task MAY produce output artifacts (files, records) before reaching a terminal status. Dependency resolution operates on task status, not artifact presence. However, agents MAY consume peer artifacts opportunistically when they are available.

**5.2.7** When a child task fails, the orchestrator MUST NOT automatically fail the parent task or its siblings. An escalation task (Section 7.2) SHOULD be created for human or triage-worker review.

### 5.3 Assignment Protocol

**5.3.1** To assign a task to a worker, the orchestrator MUST atomically set `status=assigned` and `assignee=<worker_name>` in the ledger.

**5.3.2** Assignment MUST be idempotent. Repeating the same assignment operation MUST produce the same ledger state.

**5.3.3** The orchestrator SHOULD verify assignment success by reading the record back after writing.

### 5.4 Agent Discovery of Assignment

**5.4.1** On session startup, an agent MUST be able to discover its current assignment by querying the ledger through the CLI (`orch prime`).

**5.4.2** The agent MUST NOT depend on having received a message or notification to learn its assignment. The ledger is sufficient.

### 5.5 Concurrency Control

**5.5.1** Assignment operations MUST be serialized to prevent race conditions where two dispatchers assign different tasks to the same worker, or the same task to different workers.

**5.5.2** Serialization MUST use a mechanism appropriate to the multi-process environment (e.g., database transactions, advisory file locks). In-process mutexes are insufficient when dispatchers run as separate OS processes.

**5.5.3** State files written by the orchestrator (lock files, scheduler state) SHOULD use atomic writes (write to temporary file, then rename) to prevent corruption from crashes mid-write.

### 5.6 Task Creation

**5.6.1** Tasks MAY be created by operators, agents (via `orch task create`), or external systems writing to the ledger through the orchestrator's API.

**5.6.2** `orch escalate <message>` is a convenience command that creates a task with `type=escalation` and the message as the `body`. It is equivalent to `orch task create <message> --type escalation`.

**5.6.3** Newly created tasks MUST have `status=open` and `assignee=null`. The orchestrator's dispatch loop is responsible for assignment.

### 5.7 Task Reassignment

**5.7.1** A task MAY be reassigned from one worker to another via `orch task reassign`. Reassignment MUST atomically update the `assignee` field in the ledger.

**5.7.2** Reassignment MUST NOT be performed while the current assignee has an active session working on the task. The task must first be released (via `orch fail`, `orch done --handoff`, or session death).

**5.7.3** When a task is reassigned, the new assignee discovers the task via `orch prime`. The handoff file (if present in the previous worker's workspace) is NOT automatically transferred — the new assignee starts from the ledger state and task body.

### 5.8 Parent Task Status

**5.8.1** A parent task's status MUST be updated by the orchestrator whenever a child task reaches a terminal status. This is an event-driven write, not a computed view.

**5.8.2** A parent task transitions to `completed` when all children have reached a terminal status and none have `failed`.

**5.8.3** A parent task transitions to `failed` when any child has `failed` and no non-terminal children remain (i.e., the failure was not recovered by a sibling or retry).

**5.8.4** While any children remain non-terminal, the parent MUST remain in its current status regardless of individual child outcomes.

---

## 6. Agent-to-Orchestrator Interface

### 6.1 Environment Variables

**6.1.1** The orchestrator MUST inject the following environment variables into every agent session at spawn time:

| Variable | Description |
|----------|-------------|
| `ORCH_WORKER_NAME` | The worker's persistent name. |
| `ORCH_PROJECT` | The project this worker operates in. |
| `ORCH_ROLE` | The worker's full address. |
| `ORCH_WORKSPACE` | Absolute path to the worker's isolated workspace. |
| `ORCH_BRANCH` | The version control branch for this worker's changes (if applicable). |
| `ORCH_RUN_ID` | Unique identifier for this session (for telemetry correlation). |

**6.1.2** Environment variables MUST be the sole mechanism for injecting identity context. The orchestrator MUST NOT depend on agents parsing configuration files to discover their identity.

### 6.2 CLI Interface

**6.2.1** The orchestrator MUST provide a CLI binary (or set of commands) available in the agent's `PATH` that implements at minimum the following operations:

| Command | Semantics |
|---------|-----------|
| `orch prime` | Return the worker's current assignment and handoff file (if present). |
| `orch done` | Signal task completion. Sets task status to `completed`, performs synchronous merge (Section 9), and returns the result. |
| `orch done --handoff` | Signal session handoff: task continues, session ends. Does NOT change task status or trigger merge. |
| `orch fail [--reason <message>]` | Signal task failure. Sets task status to `failed` with the reason in the task's `body`. |
| `orch task create <body> [--type <type>] [--parent <task_id>] [--depends-on <task_id>] [--priority <n>]` | Create a new task in the ledger. Returns the new task ID. |
| `orch task reassign <task_id> <worker_name>` | Reassign a task to a different worker. See Section 5.7. |
| `orch status [--worker <name>]` | Return lifecycle state, task state, and child task statuses. Without `--worker`, returns the calling worker's state. With `--worker`, returns the named worker's state (for peer observation, Section 7.2). |
| `orch heartbeat` | Write a heartbeat to the ledger. Intended to be called by a hook or cron, not manually. |
| `orch escalate <message>` | Create an escalation task (type=`escalation`) in the ledger. Convenience for `orch task create`. |
| `orch dashboard` | Return a read-only summary of system state: all workers, task counts, escalation count, merge lock status. |
| `orch version` | Return the orchestrator's protocol version. |

**6.2.2** All CLI commands MUST be callable from any agent that can execute shell commands. The CLI MUST NOT require any language-specific SDK, library import, or API client.

**6.2.3** The CLI interface MUST be the sole mechanism for agents to interact with the orchestrator. Agents MUST NOT access the assignment ledger directly, manage process host sessions, or write to orchestrator-internal files.

**6.2.4** CLI commands MUST use the following exit codes:

| Code | Meaning |
|------|---------|
| `0` | Success. |
| `1` | General failure. |
| `2` | Ledger unreachable (transient — retry is appropriate). |
| `3` | Invalid request (permanent — do not retry). |
| `4` | Conflict (merge conflict, lock timeout, or task already assigned). |

**6.2.5** CLI commands that return data (e.g., `orch prime`, `orch status`) SHOULD support a `--json` flag for structured output alongside a human-readable default.

### 6.3 Instruction File

**6.3.1** The orchestrator SHOULD write a per-workspace instruction file (e.g., `AGENT.md`) that teaches the agent the communication protocol, available commands, and behavioral expectations.

**6.3.2** The instruction file MUST be regenerated on each workspace reset (Section 4.4.2) to reflect current protocol versions.

**6.3.3** The instruction file SHOULD declare the orchestrator's protocol version so agents can detect version mismatches.

---

## 7. Supervision

### 7.1 Watchdog Process

**7.1.1** The orchestrator MUST include a watchdog: a deterministic process (not an agent) that runs on a fixed cycle.

**7.1.2** On each cycle, the watchdog MUST:
- Verify that all expected agent sessions are alive (process check: PID exists, process name matches).
- Restart dead agent sessions mechanically (no judgment about *why* they died).

**7.1.3** The watchdog MUST NOT decide *whether* to restart a dead session. It always restarts unless the circuit breaker (7.1.4) has tripped.

**7.1.4** Auto-respawn MUST be bounded by a circuit breaker. After N consecutive rapid failures of the same worker (where N is configurable, RECOMMENDED: 3), the watchdog MUST suspend restart attempts for that worker and create an escalation task.

**7.1.5** "Rapid failure" means the session exited within a configurable window of its start time (RECOMMENDED: 60 seconds).

### 7.2 Peer Observation and Escalation

**7.2.1** Any agent MAY query the ledger (via `orch status --worker <name>`) to observe a peer worker's heartbeat freshness and task status.

**7.2.2** An agent that detects a stuck peer (stale heartbeat combined with no task progress) SHOULD call `orch escalate` to create an escalation task in the ledger.

**7.2.3** Escalation tasks are standard tasks (Section 5.1) with `type=escalation`. They MAY be assigned to a configured triage worker or left unassigned for human pickup.

**7.2.4** The orchestrator MUST NOT interpret the content of escalation tasks. It dispatches them like any other task. (ZFC-compliant.)

### 7.3 Systemic Failure Detection

**7.3.1** The watchdog MUST track the count of escalation tasks created within a rolling time window (configurable, RECOMMENDED: 30 minutes).

**7.3.2** When the escalation count exceeds a configurable threshold (RECOMMENDED: 5), the watchdog MUST alert the operator through a configured channel (log entry, webhook, email, or equivalent).

**7.3.3** The watchdog counts escalations mechanically. It MUST NOT interpret their content, classify their severity, or decide recovery actions. (ZFC-compliant.)

### 7.4 Limitations

This profile does not provide autonomous cognitive supervision. There is no dedicated supervisor or monitor agent tier. Escalation routes to human operators or triage workers, not to self-healing agents.

For autonomous recovery reasoning, adopt the full spec's supervision hierarchy (full spec Section 7). See Section 15.2, extension point 5.

---

## 8. Workspace Isolation

### 8.1 Requirements

**8.1.1** Each worker MUST operate in a dedicated workspace directory that no other worker can write to.

**8.1.2** Workspaces MUST be copy-on-write derivatives of a shared canonical source (e.g., git worktrees, filesystem snapshots, container volumes). Full duplication SHOULD be avoided for resource efficiency.

**8.1.3** Workers MUST NOT share a mutable workspace. Concurrent modification of shared files across workers MUST be prevented by the isolation mechanism, not by agent cooperation.

### 8.2 Merge-Back Protocol

**8.2.1** The orchestrator MUST define a merge-back protocol for integrating worker changes into the canonical source.

**8.2.2** The merge-back protocol MUST be invoked explicitly (by the agent calling `orch done`) — never implicitly by the orchestrator on a timer or threshold.

**8.2.3** Merge conflicts that require semantic judgment MUST be surfaced to the originating worker for resolution. The orchestrator MUST NOT auto-resolve such conflicts. Mechanical operations (e.g., deterministic rebase as described in Section 9.1) are not considered auto-resolution.

---

## 9. Merge Protocol

### 9.1 Mechanism

**9.1.1** The orchestrator MUST provide a merge protocol that serializes the integration of worker changes into the canonical source.

**9.1.2** `orch done` is **synchronous**: when an agent calls `orch done`, the orchestrator performs the merge inline and returns the result via exit code. The agent receives immediate feedback without polling or notification.

**9.1.3** The merge protocol operates as follows:
1. Set task status to `completed` in the ledger. Append a worker history record.
2. Acquire the project-wide merge lock (advisory file lock, database advisory lock, or equivalent).
3. Rebase the worker's branch onto the current canonical head.
4. Run validation checks (build, test) if configured. Validation is infrastructure-level (deterministic), not agent-level (cognitive).
5. Fast-forward merge into the canonical branch.
6. Release the merge lock.
7. Return exit code `0` to the calling agent.

**9.1.4** If the merge fails (steps 3-5), the task status MUST be reverted to `in_progress`. The task is not complete until its changes are integrated.

**9.1.5** The merge lock MUST enforce mutual exclusion: only one merge operation may be in-flight at any time.

### 9.2 Conflict Handling

**9.2.1** If the rebase produces conflicts, the orchestrator MUST release the merge lock and return exit code `4` to the calling agent. The agent's task remains `in_progress` — the agent is responsible for resolving the conflict and calling `orch done` again.

**9.2.2** If validation fails, the orchestrator MUST release the merge lock and return exit code `1` to the calling agent. The agent is responsible for fixing the validation issue.

**9.2.3** The merge lock MUST have a configurable timeout (RECOMMENDED: 5 minutes). If the lock cannot be acquired within the timeout, `orch done` MUST return exit code `4`. The agent SHOULD retry after a delay.

**9.2.4** The merge protocol MUST NOT silently drop failed merge attempts. Every failure MUST be reflected in the exit code returned to the calling agent.

### 9.3 Non-Git Workspaces

**9.3.1** This section assumes git workspaces. Implementations using non-git workspace isolation MUST provide equivalent semantics: mutual exclusion, conflict detection, validation, and failure signaling via exit codes.

### 9.4 Limitations

This profile supports serial merge processing only. For parallel merges with conflict-domain partitioning, see Section 15.2, extension point 6.

---

## 10. Session Continuity

### 10.1 Problem Statement

Agent sessions have finite context windows. When accumulated context approaches capacity, the session must be replaced with a fresh one without losing the worker's progress or working state. Session continuity is a first-class architectural concern, not an edge case.

### 10.2 Handoff File

**10.2.1** The orchestrator MUST support a handoff mechanism that transfers working context from an outgoing session to its successor via a workspace file.

**10.2.2** The outgoing session writes a handoff file (`.handoff.json`) to the workspace root before exiting. The incoming session discovers this file via `orch prime`, which checks both the ledger assignment and the workspace handoff file.

**10.2.3** The handoff file MUST conform to the following schema:

```json
{
  "version": 1,
  "reason": "context_pressure | explicit | crash_recovery",
  "task_id": "<current task ID>",
  "summary": "<free-text summary of work done and work remaining>",
  "decisions_made": [
    "<description of a decision and its rationale>"
  ],
  "approaches_rejected": [
    "<description of an approach that was tried and why it was abandoned>"
  ],
  "open_questions": [
    "<unresolved question that the successor should address>"
  ],
  "workspace_state": "clean | dirty | conflicted",
  "files_modified": [
    "<path relative to workspace root>"
  ],
  "session_number": 1,
  "created_at": "<ISO 8601 timestamp>"
}
```

**10.2.4** The `decisions_made` and `approaches_rejected` fields are REQUIRED when `reason` is `context_pressure` or `explicit`. They MAY be empty when `reason` is `crash_recovery` (since the outgoing session may not have had the opportunity to write them).

**10.2.5** The `session_number` field MUST be monotonically incremented by each successor session. The incoming session reads the previous value and writes `session_number + 1` in any subsequent handoff.

**10.2.6** Each handoff overwrites the previous `.handoff.json` file. To preserve handoff history, the orchestrator SHOULD append each handoff to a `.handoff-history.jsonl` file (one JSON object per line) in the workspace before overwriting `.handoff.json`. The history file is informational and MUST NOT be required for session recovery.

**10.2.7** When a task completes (`orch done` without `--handoff`) or fails (`orch fail`), the orchestrator SHOULD delete `.handoff.json` from the workspace. The history file (`.handoff-history.jsonl`) SHOULD be retained until workspace reset.

### 10.3 Handoff Triggers

**10.3.1** Handoff MAY be triggered by:
- (a) The agent writing `.handoff.json` and calling `orch done --handoff` when it detects context pressure.
- (b) An infrastructure hook (e.g., a pre-compaction event) that invokes handoff automatically before the session's context is truncated.
- (c) The watchdog restarting a dead session (in which case `.handoff.json` may be absent or incomplete, and `reason` is `crash_recovery`).

**10.3.2** Regardless of trigger, the outcome is the same: the session writes a handoff file (if able), the session terminates, and the watchdog starts a new session for the same worker. The incoming session discovers context via `orch prime`.

### 10.4 Continuity Invariants

**10.4.1** Assignment durability (Section 5.4) ensures the incoming session can rediscover its task. The handoff file provides the additional *cognitive* context that the ledger alone cannot capture.

**10.4.2** A handoff MUST NOT change the worker's assignment. The task remains assigned throughout the transition.

**10.4.3** Multiple consecutive handoffs for the same assignment MUST be supported. There is no limit on how many times a worker may cycle sessions for a single task.

---

## 11. Agent-Type Abstraction

### 11.1 Preset Registry

**11.1.1** The orchestrator MUST maintain a preset registry that maps agent type names to launch configurations.

**11.1.2** Each preset MUST define:

| Field | Type | Description |
|-------|------|-------------|
| `command` | string | The CLI binary to invoke. |
| `args` | string[] | Default command-line arguments. |
| `process_names` | string[] | Process names to match in liveness checks. |
| `readiness_mode` | enum | How to detect session readiness: `prompt_detection`, `delay_based`, `api_check`. |
| `hooks_provider` | string | Which hook/settings format this agent uses. |
| `prompt_mode` | enum | How to deliver prompts: `arg` (command-line), `stdin`, `none`. |
| `env` | map | Agent-specific environment variables. |

**11.1.3** Adding support for a new agent type MUST NOT require orchestrator source code changes. A new preset entry MUST be sufficient.

### 11.2 Per-Role Overrides

**11.2.1** The orchestrator SHOULD support per-role agent type overrides (e.g., triage workers use one model, standard workers use another).

**11.2.2** Per-project overrides SHOULD take precedence over system-wide defaults.

---

## 12. Failure Recovery

### 12.1 Auto-Respawn

**12.1.1** The watchdog MUST support auto-respawn as defined in Section 7.1.

**12.1.2** Auto-respawn MUST be bounded by the circuit breaker defined in Section 7.1.4.

### 12.2 Admission Control

**12.2.1** Before spawning a new session, the orchestrator MUST verify that infrastructure dependencies (ledger, process host, disk) have sufficient capacity.

**12.2.2** The orchestrator SHOULD enforce a configurable maximum number of concurrent active workers to prevent resource exhaustion.

### 12.3 Assignment Durability

**12.3.1** A session crash MUST NOT cause assignment loss. The ledger record MUST persist through any number of session restarts.

**12.3.2** A new session for the same worker MUST be able to recover the assignment from the ledger without external intervention.

### 12.4 Orphan Detection

**12.4.1** The orchestrator MUST provide a mechanism to detect orphaned processes — sessions whose parent management structures no longer reference them.

**12.4.2** Orphan cleanup MUST verify process identity (e.g., PID + `session_started_at`) before termination to prevent killing unrelated processes that reused a PID.

### 12.5 Graceful Degradation

**12.5.1** If the ledger becomes unavailable, active workers SHOULD continue operating on their current task. The workspace is local; work in progress is not lost.

**12.5.2** During a ledger outage, agents SHOULD queue heartbeat writes locally. When the ledger recovers, agents MUST flush queued data.

**12.5.3** During a ledger outage, the watchdog MUST enter a degraded mode: process liveness checks continue, but ledger-dependent operations (assignment, dispatch, escalation tracking) are suspended.

**12.5.4** The watchdog SHOULD use exponential backoff when polling for ledger recovery. After a configurable maximum retry count, the watchdog MUST alert the operator.

---

## 13. Observability

### 13.1 Heartbeat Protocol

**13.1.1** Active sessions SHOULD write a heartbeat to the ledger at a regular interval (RECOMMENDED: every 30-60 seconds) using `orch heartbeat` or equivalent.

**13.1.2** A heartbeat write MUST update the `last_heartbeat` and `heartbeat_state` fields in the workers table.

**13.1.3** A heartbeat that has not been updated within a configurable staleness threshold (RECOMMENDED: 3 minutes) is considered stale.

**13.1.4** Staleness is an **observation** — the infrastructure MUST expose it (via `orch status`), but MUST NOT act on it unilaterally. Peer agents consume this signal and decide what to do (Section 7.2).

### 13.2 Telemetry Correlation

**13.2.1** Each session MUST be assigned a unique run identifier at spawn time.

**13.2.2** The run identifier MUST be propagated as an environment variable (`ORCH_RUN_ID`, Section 6.1.1) to enable correlation of all telemetry events originating from that session.

### 13.3 Operational Dashboard

**13.3.1** The orchestrator SHOULD expose a read-only view of system state through a CLI command (`orch dashboard`) or JSON endpoint. This view SHOULD include at minimum:
- Worker states and heartbeat ages.
- Task graph status (open, in-progress, completed, failed counts).
- Escalation task count within the rolling window.
- Merge lock holder (if any) and hold duration.

---

## 14. Anti-Patterns

This section is non-normative. It enumerates known architectural violations and their consequences.

| Anti-Pattern | Violation | Consequence |
|-------------|-----------|-------------|
| **Smart orchestrator** — infrastructure decides retry/skip/escalate | ZFC S3.2.2 | Heuristics break when agent behavior changes; threshold tuning becomes perpetual maintenance |
| **Output parsing** — orchestrator reads agent stdout to decide next action | ZFC S3.2.3 | Fragile coupling to agent output format; breaks on model updates |
| **Shared workspace** — agents edit the same files | Isolation S8.1.3 | Merge conflicts, race conditions, corrupted state |
| **Task-only assignment** — work conveyed only in task body with no durable assignment record | Ledger S5.4.2 | Crash loses assignment; agent restarts with no memory of responsibility |
| **Silent workspace purge** — workspace reset without checking for unmerged changes | Guard S4.4.4 | Unmerged work destroyed; agent's completed task lost on reuse |
| **Unbounded escalation** — escalation tasks accumulate with no alerting threshold | Supervision S7.3 | System silently degrades; operator unaware of systemic failure |
| **Prose-only handoff** — handoff context is unstructured free text | Continuity S10.2.3 | Successor session cannot reliably parse decisions, rejected approaches, or open questions |
| **Ledger bypass** — agent writes directly to ledger storage instead of using CLI | Interface S6.2.3 | Breaks concurrency control, bypasses validation, creates invisible state changes |
| **Ledger-as-cache** — high-frequency ephemeral data (logs, traces) stored in ledger | Design S2.1 | Overloads the single durable store; degrades task tracking performance |
| **Agent-specific orchestrator code** — `if agent == "X" then ...` | Abstraction S11.1.3 | Every new agent type requires orchestrator code changes |

---

## 15. Conformance

### 15.1 Conformance Levels

**Minimal conformance.** An implementation minimally conforms if it satisfies all MUST requirements in Sections 3-8 (ZFC, identity, ledger, interface, supervision, workspace isolation). This level is suitable for prototypes and single-worker deployments that do not require merge protocols, session continuity, or observability.

**Full conformance.** An implementation fully conforms to this profile if it satisfies all MUST and MUST NOT requirements in Sections 3-13.

### 15.2 Extension Points

Implementations MAY extend this profile by adopting the following capabilities from the full specification. Each extension is self-contained and does not require changes to the core sections of this profile.

| Extension | Full Spec Section | When to Adopt |
|-----------|------------------|---------------|
| 1. Inter-agent messaging (durable mail channel) | Full spec S5 | Workers need to send ad-hoc messages to each other |
| 2. Multi-scope hierarchy (organization + project) | Full spec S14 | Multiple projects with independent worker pools and configuration |
| 3. Nudge channel (ephemeral, sub-second notifications) | Full spec S5.3 | Latency-sensitive inter-agent coordination |
| 4. Cost tiers (role-to-model-class mapping) | Full spec S11.3 | Budget control across heterogeneous agent types |
| 5. Cognitive supervision hierarchy (Tier 1/2 agents) | Full spec S7 | Autonomous recovery reasoning without human escalation |
| 6. Conflict-domain partitioning for parallel merges | (novel extension) | Multiple workers merging disjoint file sets concurrently |

### 15.3 Protocol Versioning

**15.3.1** Implementations SHOULD expose their protocol version through the CLI (`orch version` or equivalent) and in the instruction file (Section 6.3.3).

**15.3.2** The protocol version follows Semantic Versioning: breaking changes to MUST requirements increment the major version; new SHOULD/MAY features increment the minor version.

---

## Appendix A: Reference Implementation Mapping

The following table maps specification concepts to one possible implementation for illustrative purposes. It is non-normative.

| Spec Concept | Reference Implementation |
|-------------|------------------------|
| Orchestrator | Go or Python CLI binary |
| Watchdog | Daemon process with fixed-cycle heartbeat check |
| Assignment Ledger | SQLite database (single-node) or Dolt (collaborative) |
| Worker | Named agent with themed name pool |
| Workspace | git worktree per worker |
| Instruction File | Per-workspace `AGENT.md` |
| Heartbeat | Row in workers table, updated via `orch heartbeat` |
| Handoff | `.handoff.json` file in workspace root |
| Merge Lock | Advisory file lock (`flock`) or database advisory lock |
| Preset Registry | JSON configuration file |
| Escalation | Standard task with `type=escalation` |
| Operational Dashboard | `orch dashboard --json` piped to a TUI or web viewer |

---

## Appendix B: Comparison to Full Specification

The following table maps each simplification in this profile to its full-spec counterpart, the replacement mechanism, and the trade-off accepted.

| Full Spec Feature | Full Spec Section | Lite Replacement | Trade-off |
|-------------------|------------------|-----------------|-----------|
| 4-tier supervision hierarchy | S7 | Watchdog + peer escalation (S7) | No autonomous recovery reasoning; escalation requires human or triage worker |
| Mail channel (durable messaging) | S5 | Eliminated; all coordination via ledger tasks and `orch done` exit codes | No inter-agent messaging; merge results returned synchronously |
| Nudge channel (ephemeral) | S5.3 | Eliminated | No sub-second notifications |
| Separate heartbeat files | S16.1 | Heartbeat column in ledger (S13.1) | Heartbeat writes depend on ledger availability |
| Workflow templates | S8 | Task graphs with parent/child + dependencies (S5.2) | No named "phases" as a distinct concept; phases are tasks |
| Bundles (meta-tasks) | S13 | Parent tasks with children (S5.2) | No dedicated sequential-dispatch abstraction |
| Merge queue | S10 | Synchronous git mutex via `orch done` (S9) | Serial only; agent blocks during merge; no queue state visibility |
| Multi-scope hierarchy | S14 | Flat project namespace | Single project per orchestrator instance |
| Cost tiers | S11.3 | Deferred to extension | No declarative model-class budgeting |
| Handoff via self-addressed mail | S12.2 | Handoff via workspace file (S10.2) | Handoff doesn't flow through any messaging system |

**Upgrade path:** When a limitation in this profile becomes a bottleneck, adopt the corresponding extension point from Section 15.2. Each extension adds capability without requiring rewrites to the core sections (3-8).
