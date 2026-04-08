# Build-Verify-Validate Execution Specification

**Version:** 1.0.0-draft
**Status:** Working Draft
**Editors:** ENDGAME
**Date:** 2026-04-04

---

## Abstract

This specification defines an execution model for autonomous software delivery through build-verify-validate agent workflows. It reuses the infrastructure layer from the Multi-Agent Pipeline Orchestration Specification (Sections 4–11a) — worker identity, assignment ledger, supervision, session continuity — and replaces the pipeline execution layer (Sections 12–17) with DAG-driven dispatch, where lifecycle ordering emerges from dependency edges rather than phase structure.

The model separates three concerns: (1) an infrastructure layer managing worker sessions, durable state, and process supervision; (2) a task graph encoding work items and dependencies in the assignment ledger; and (3) agent instruction files defining role-specific behavior independently of the orchestrator.

## Status of This Document

This document is a Working Draft.

## Notices

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

Non-normative text is marked with *(Non-normative)* or enclosed in a note block.

---

## 1 Introduction

### 1.1 Purpose

This specification defines an execution model for coordinating autonomous agents that produce software artifacts — code, tests, migrations, and commits. A planning agent decomposes specifications into tasks, builder agents implement those tasks, verifier agents validate implementations against specifications, and a PR gate controls merge to the canonical branch.

### 1.2 Scope

This specification covers:

- DAG-driven task dispatch from a durable assignment ledger
- Agent identity via instruction files, decoupled from agent runtime
- A two-level task graph: work items (individual tasks) and feature lifecycle (coordination)
- A planning agent that decomposes work packages into build and V&V tasks
- An exit-code-based completion protocol for agent-to-orchestrator signaling
- Role-based routing of tasks to appropriate agent instruction files
- PR-based quality gates as the merge mechanism
- Session protocol with infrastructure handoff and agent-managed memory
- Error semantics: retry, gap accumulation, and escalation
- Safety and liveness properties for DAG-driven execution

This specification excludes:

- Internal behavior of any agent implementation
- Domain-specific build rules, coding conventions, or test strategies
- CI/CD platform specifics (GitHub Actions, GitLab CI, etc.)
- The content of work package specifications
- Authentication, authorization, or multi-tenancy

### 1.3 Relationship to the Orchestration Specification

This specification reuses the infrastructure layer from the Multi-Agent Pipeline Orchestration Specification (hereafter "the orchestration specification"), Sections 4 through 11a, and replaces the pipeline execution layer (Sections 12 through 17) with DAG-driven dispatch suited to software delivery.

The orchestration specification defines two conformance levels: Level 1 (infrastructure) and Level 2 (pipeline execution). This specification conforms at Level 1 and defines its own execution semantics.

### 1.4 Normative References

- RFC 2119: Key words for use in RFCs to Indicate Requirement Levels
- Multi-Agent Pipeline Orchestration Specification, Version 1.0.0-draft (2026-03-27)

### 1.5 Typographical Conventions

Normative requirements carry identifiers in brackets with a `BVV-` prefix (e.g., `[BVV-DSP-01]`). Requirement identifier prefixes:

| Prefix | Domain |
|--------|--------|
| `BVV-DSN` | Design principles |
| `BVV-AI` | Agent identity model |
| `BVV-TG` | Task graph model |
| `BVV-DSP` | Dispatch semantics |
| `BVV-GT` | Gate semantics |
| `BVV-SS` | Session semantics |
| `BVV-ERR` | Error semantics |
| `BVV-S` | Safety properties |
| `BVV-L` | Liveness properties |

---

## 2 Terminology and Definitions

| Term | Definition |
|------|-----------|
| **Builder (Oompa)** | An agent role that implements work items: writes code, tests, migrations, and commits. Operates under OOMPA.md. |
| **Verifier (Loompa)** | An agent role that validates implementations against specifications: traces code, verifies business rules, fixes defects, and commits. Operates under LOOMPA.md. |
| **Planning Agent (Charlie)** | An agent that decomposes a work package into build tasks, V&V tasks, and their dependency graph in the assignment ledger. Operates under CHARLIE.md. |
| **Work Package** | A bundle of specifications — functional, technical, and V&V — that defines a feature or increment. The planning agent's input. |
| **Instruction File** | A markdown document that defines agent identity: phases, decision rules, operating rules, completion protocol, and memory format. Examples: OOMPA.md (builder), LOOMPA.md (verifier). The instruction file IS the agent contract. |
| **Feature Lifecycle** | The coordination graph for a feature branch: plan → build* → V&V* → PR gate. Encoded as tasks and dependency edges in the assignment ledger. |
| **Completion Signal** | A structured exit protocol (exit code + optional stdout tag) communicating task outcome to the orchestrator. The orchestrator's sole input for task result determination. |
| **PR Gate** | A terminal lifecycle task that creates a pull request from the feature branch and blocks on CI status checks. The final quality checkpoint before merge eligibility. |
| **DAG-Driven Dispatch** | A dispatch model where the orchestrator dispatches any ready task from the ledger. Ordering emerges from dependency edges, not phases. |
| **Role Tag** | A metadata label on a task (e.g., `role:builder`, `role:verifier`) that the orchestrator uses to select the appropriate instruction file. |
| **Critical Task** | A task with the `critical:true` label. Failure of a critical task aborts the lifecycle immediately, bypassing gap tolerance. |

Terms from the orchestration specification — Agent, Assignment Ledger, Worker, Session, Workspace, Watchdog, Escalation Task — retain their definitions. **Heartbeat** is defined in the orchestration specification but unused here; liveness detection uses tmux session presence (Section 11a.4).

---

## 3 Conformance

### 3.1 Conformance Targets

This specification defines requirements for one conformance target:

- **Orchestrator (Wonka)**: The infrastructure component that manages workers, the ledger, process supervision, and DAG-driven dispatch.

Agents are not conformance targets. Instruction files define agent behavior, outside this specification's scope.

> *Note (non-normative):* Reference names — Wonka (orchestrator), Oompa (builder), Loompa (verifier), Charlie (planner) — are used in instruction file paths and agent profiles. The specification text uses the generic role terms (orchestrator, builder, verifier, planner) for normative requirements.

### 3.2 Conformance Levels

**Level 1 — Core Dispatch.** An orchestrator conforms at Level 1 if it satisfies all MUST requirements in Sections 4 through 8 (excluding Sections 7.3, 7.4, 7.5, and 8.5), plus Sections 10, 11, 11a, 12, and 13 (including 13.4). This level is sufficient for dispatching tasks from a pre-populated ledger to single-role agents.

**Level 2 — Feature Lifecycle.** An orchestrator conforms at Level 2 if it satisfies Level 1 and all MUST requirements in Sections 7.3, 7.4, 7.5, 8.5, and 9. This level adds planning agent support, lifecycle task creation, task graph validation, parallel workspace management, and PR-based quality gates.

---

## 4 Design Principles

This specification inherits design principles DSN-01 through DSN-09 from the orchestration specification. The following extend them for DAG-driven software delivery:

### 4.1 DAG-Driven Dispatch

`[BVV-DSN-01]` The orchestrator MUST dispatch any task from the ledger whose dependencies have all reached terminal status. Lifecycle ordering — build before verify, verify before merge — MUST be encoded as dependency edges in the task graph, not as orchestrator logic.

> *Note (non-normative):* This replaces the phase-driven dispatch model (orchestration specification, Sections 12–15), where the orchestrator advances through phases sequentially. DAG-driven dispatch eliminates phases — only tasks and edges remain.

### 4.2 Agents Own Iteration

`[BVV-DSN-02]` The orchestrator MUST dispatch exactly one task per worker session. The agent executes one task, signals its outcome via exit code, and the session ends. The orchestrator is the outer loop.

> *Note (non-normative):* This replaces the iteration-loop model where agents run `bd ready` in a loop. The orchestrator selects and assigns tasks; agents execute them.

### 4.3 Two-Layer Memory

`[BVV-DSN-03]` The infrastructure layer MUST manage session handoff (per the orchestration specification CTY-01 through CTY-09). Agents MAY maintain their own cross-session memory (e.g., PROGRESS.md). The orchestrator MUST NOT read, write, or depend on agent memory files.

### 4.4 Phase-Agnostic Orchestration

`[BVV-DSN-04]` The orchestrator is lifecycle-aware but phase-agnostic. The orchestrator knows about branches (lifecycle scoping), gap counts (bounded degradation), and role tags (routing). The orchestrator MUST NOT contain conditional logic that references a specific lifecycle phase (build, verify, gate) by name in dispatch or status decisions. All phase-specific behavior — what a builder does vs. what a verifier does vs. what the gate does — MUST reside in instruction files or the planning agent, not in the orchestrator.

**Compliance test:** For any proposed orchestrator logic, apply: *"Does this logic branch on whether the current task is a build task, a V&V task, or a gate task?"* If yes, it belongs in the planning agent or instruction file, not the orchestrator. Logic that operates on role tags generically (e.g., "route any `role:X` to instruction file X") is compliant. Logic that says "if role is gate, then create PR" is not — that belongs in the gate handler.

---

## 5 Infrastructure Layer

This specification reuses Sections 4 through 11a of the orchestration specification's infrastructure layer. The table below shows which sections apply and how.

### 5.1 Conformance Profile

| Orch Spec Section | Requirement IDs | Status | Notes |
|-------------------|-----------------|--------|-------|
| §4 Design Principles | DSN-01 through DSN-09 | INCLUDED | All apply unchanged. |
| §5 Worker Identity | WKR-01 through WKR-12 | INCLUDED | Workers are reused across tasks on the same branch. |
| §6 Assignment Ledger | LDG-01 through LDG-20, LDG-07a, LDG-07b, LDG-14a | ADAPTED | Beads is the store implementation. See Section 5.1a (Status Adaptation) for semantic modifications. Sub-requirements (LDG-07a artifact semantics, LDG-07b no-notification guarantee, LDG-14a assigned→in_progress transition) apply unchanged. |
| §7 Agent-to-Orchestrator Interface | ITF-01, ITF-02 | INCLUDED | Environment variable injection unchanged. `ORCH_TASK_ID` added (Section 8.4). |
| §7 Agent-to-Orchestrator Interface | ITF-03 through ITF-11, ITF-04a | REPLACED | The command interface (`prime`, `done`, `fail`, `heartbeat`, etc.) is replaced by the exit-code completion protocol (Section 8.3). ITF-04a (structured output flag) does not apply — agents use exit codes, not command return values. |
| §8 Supervision | SUP-01 through SUP-10, SUP-07a | INCLUDED | Watchdog and circuit breaker apply unchanged. SUP-07a (peer stuck detection) is EXCLUDED — liveness uses tmux presence (Section 11a.4), not peer heartbeat checks. |
| §9 Workspace Isolation | WSP-01 through WSP-03 | ADAPTED | See Section 8.5 (Parallel Workspace Strategy). Default mode uses shared repository with DAG-serialized builds. |
| §10 Workspace Merge | MRG-01 through MRG-08, MRG-05a | EXCLUDED | Replaced by PR gate (Section 9). Agents commit directly to feature branches. MRG-05a (validation failure exit code) does not apply — merge is replaced by PR-based flow. |
| §11 Session Continuity | CTY-01 through CTY-09 | INCLUDED | Handoff file mechanism unchanged. Agent memory (PROGRESS.md) is a separate layer (Section 10.2). |
| §11a Failure Recovery | RCV-01 through RCV-10 | INCLUDED | All apply unchanged. |
| §12–17 Pipeline Execution | EXP-*, WFC-*, OPS-*, CON-*, ERR-* | EXCLUDED | Replaced entirely by Sections 6 through 8 of this specification. |
| §18 Safety Properties | S1 through S11 | ADAPTED | S1 (Monotonic Progress) subsumed by BVV-S-02. S2, S3, S4, S8 not applicable (no consensus, no content merge). S5 → BVV-S-07. S6 → BVV-S-06. S7 → BVV-S-01. S9 → BVV-S-08. S10 → BVV-S-09. S11 → BVV-S-10. See Section 12. |
| §19 Liveness Properties | L1 through L3 | ADAPTED | Restated for DAG model in Section 13. |
| §20 Agent-Type Abstraction | ATA-01 through ATA-05 | PARTIAL | ATA-01 through ATA-03 INCLUDED (preset registry, preset fields, no-source-change extensibility). ATA-04 (per-role agent type overrides) INCLUDED — maps to role-to-preset binding (Section 6.3). ATA-05 (per-project overrides) INCLUDED — preset selection via `--agent` flag (Section 6.3, BVV-AI-03). |
| §21 Observability | OBS-01 through OBS-04, OBS-01a | PARTIAL | OBS-01 (heartbeat writes) EXCLUDED — replaced by tmux liveness detection (Section 11a.4). OBS-01a (heartbeat staleness signal) EXCLUDED — same reason. OBS-02 (staleness threshold) EXCLUDED. OBS-03 (run ID propagation) INCLUDED. OBS-04 (system state view) INCLUDED — RECOMMENDED for lifecycle dashboards. Lifecycle event kinds added (Section 10.3). |

### 5.1a Status Adaptation

The orchestration specification defines task statuses `{open, assigned, in_progress, completed, failed, deferred, overridden}` with terminal set `{completed, failed, overridden}`. This specification modifies the status model as follows:

| Adaptation | Detail |
|------------|--------|
| **Added: `blocked`** | Terminal status for dispatch. An agent signals `blocked` via exit code 2. The task is not retried. May be re-opened by human intervention (BVV-S-02a). |
| **Dropped: `deferred`** | The orchestration specification (LDG-05) defines `deferred` as non-terminal — dependencies on deferred tasks remain unresolved. BVV drops `deferred`; `blocked` serves a different purpose (terminal, not resumable by the orchestrator). LDG-05 is inapplicable. |
| **Dropped: `overridden`** | Defined in the orchestration specification (EXP-11) for bypassing halting gate failures. BVV replaces quality gates with the PR gate (Section 9); gate bypass uses human intervention on the gate task directly. EXP-11 is in the EXCLUDED section (§12–17). |
| **LDG-16 through LDG-20: NOT APPLICABLE** | These requirements define parent-child task status derivation (parent transitions based on child statuses). BVV uses flat dependency edges, not parent-child relationships. The orchestrator does not derive parent status from children. |

The BVV task status enum is: `{open, assigned, in_progress, completed, failed, blocked}` with terminal set `{completed, failed, blocked}`.

### 5.2 Ledger Implementation

`[BVV-DSP-16]` The assignment ledger MUST be implemented using Beads (a Dolt-backed task database). The orchestrator interacts with the ledger through the `Store` interface, which provides:

- `ReadyTasks(labels ...string)` — returns tasks where `status=open`, all dependencies have reached terminal status, `assignee` is empty, and all specified labels are present. Lifecycle scoping (BVV-DSP-08) uses this parameter to filter by branch label.
- `Assign(taskID, workerName)` — atomically sets task status to `assigned` and worker's current task
- `CreateTask`, `GetTask`, `UpdateTask` — CRUD operations on task records
- `AddDep`, `GetDeps` — dependency edge management with cycle detection

The `Store` interface is defined in the orchestration specification (Section 6). Beads-specific status mapping:

| Orch Status | Beads Status | Notes |
|-------------|-------------|-------|
| `open` | `open` | Task available for dispatch. |
| `assigned` | `open` + `orch:assigned` label | Assigned but session not yet started. |
| `in_progress` | `open` + `orch:in_progress` label | Agent actively executing. |
| `completed` | `closed` + `orch:completed` label | Task finished successfully. |
| `failed` | `closed` + `orch:failed` label | Task failed after retries exhausted. |
| `blocked` | `blocked` | Agent signaled blocked (exit code 2). Terminal for dispatch; may be re-opened by human intervention. |

---

## 6 Agent Identity Model

### 6.1 Instruction Files

An instruction file is a markdown document defining an agent's identity, behavior, and protocol. The orchestrator injects it as a system prompt when spawning a worker session.

An instruction file MUST contain the following sections:

| Section | Purpose |
|---------|---------|
| Phases | Ordered steps the agent executes per task (e.g., orient → plan → build → verify → report). |
| Decision Rules | Precedence-ordered rules the agent applies when making judgment calls. |
| Operating Rules | Constraints on agent behavior: one task per iteration, file path conventions, commit format. |
| Completion Protocol | How the agent signals task outcome. In this specification: exit codes 0/1/2/3 (Section 8.3). |
| Memory Format | Structure of the agent's cross-session memory file (e.g., PROGRESS.md). |

An instruction file MAY contain:

- **Code Patterns** — implementation sequences, naming conventions, architectural rules
- **Domain Mapping** — how task metadata maps to code locations
- **Pre-flight Checks** — infrastructure validation before task execution (e.g., Docker services running)

#### 6.1.1 Canonical Structure

*(Non-normative)* The RECOMMENDED instruction file structure:

```markdown
# {Role} Agent Instructions

{Preamble: context sources, iteration rules, one-task protocol}

---

## Phase 1: {ORIENT | PRE-FLIGHT}
{How the agent discovers its task and loads context}

## Phase 2: {PLAN | DISCOVER}
{How the agent plans or traces the work}

## Phase 3: {BUILD | VERIFY + FIX}
{The core execution phase}

## Phase 4: {VERIFY | REPORT}
{Self-verification or reporting}

## Phase 5: {REPORT}
{Commit, update ledger, write memory}

---

## Decision Rules
{Precedence-ordered rules, first match wins}

## Code Patterns
{Domain-specific implementation guidance}

## Memory Format
{PROGRESS.md structure}

## Operating Rules
{One task per iteration, file paths, completion protocol}
```

The orchestrator ignores instruction file structure. The structure serves agent comprehension and human maintainability.

`[BVV-AI-01]` The orchestrator MUST inject the instruction file as a system prompt via the preset's `SystemPromptFlag`. The orchestrator MUST NOT modify the instruction file content.

### 6.2 Agent Roles

This specification defines three abstract roles. Each role has required capabilities and permitted exit codes:

| Role | Required Capabilities | Exit Codes |
|------|----------------------|------------|
| **Builder** | Read task from ledger, read specifications, write code, run tests, commit changes, update task status | 0 (done), 1 (fail), 2 (blocked), 3 (handoff) |
| **Verifier** | Read task from ledger, trace code against specifications, verify business rules, fix defects, commit changes, update task status | 0 (done), 1 (fail), 2 (blocked), 3 (handoff) |
| **Planner** | Read work package specifications, create tasks in ledger, set dependency edges | 0 (done), 1 (fail), 2 (blocked) |

> *Note (non-normative):* The planner role omits exit code 3 (handoff) because planning should complete within a single session. If context pressure occurs, the planner signals failure (exit 1) and retries from scratch.

### 6.3 Role-to-Preset Binding

`[BVV-AI-02]` Each role MUST be associated with exactly one instruction file. The orchestrator selects the instruction file based on the task's `role` metadata tag. The gate role is an exception: the gate handler MAY be a deterministic script rather than an agent. When the gate handler is a script, the orchestrator invokes it directly instead of injecting an instruction file. The script MUST conform to the exit code protocol (Section 8.3.1).

> *Note (non-normative, Level 1):* The gate role exception clause references the PR gate (Section 9), which is a Level 2 concept. At Level 1, where no gate role exists, this exception is inoperative — all roles use instruction files.

`[BVV-AI-03]` Multiple presets (e.g., claude-builder, codex-builder) MAY be configured for the same role. Preset selection is an orchestrator configuration concern, not a task metadata concern. The orchestrator SHOULD support a `--agent` flag or equivalent to select the preset at runtime.

The binding chain:

```
task.metadata["role"] → instruction file → preset → tmux session
```

The orchestrator reads the task's `role` label, locates the corresponding instruction file, reads the file (stripping YAML frontmatter), and injects the content as a system prompt when spawning the worker session.

---

## 7 Task Graph Model

### 7.1 Level 1 — Work Items

The assignment ledger contains individual work items as tasks. Each task is a single unit of work executed by one agent session.

`[BVV-TG-01]` The orchestrator MUST NOT create work-item tasks in the ledger. Only the planning agent and human operators create tasks. The orchestrator MAY update task status (`open` → `assigned` → `in_progress` → `completed` | `failed` | `blocked`) and assignee as part of lifecycle management, per LDG-08 through LDG-19 of the orchestration specification.

> *Note (non-normative, Level 1):* The planning agent is a Level 2 concept (Section 7.4). At Level 1, this requirement reduces to: "Only human operators create tasks. The orchestrator dispatches from a pre-populated ledger."

Task lifecycle:

```
                 assign              session starts
  open ──────────────► assigned ──────────────────► in_progress
    ▲                                                  │
    │                                     ┌────────────┼────────────┐
    │                                     │            │            │
    │                                exit 0        exit 1       exit 2
    │                                     │            │            │
    │                                     ▼            ▼            ▼
    │                                completed    (retry?)      blocked
    │                                              │     │
    │                               retries       yes    no
    │                               remain?        │     │
    │                                              │     ▼
    └──────────────────────────────────────────────┘   failed
                    reset to open
```

Terminal statuses: `completed`, `failed`, `blocked`. Terminal tasks are ineligible for dispatch. `blocked` tasks MAY be re-opened by human intervention or a planning agent re-run (see BVV-S-02a).

When an agent exits with code 1 and retries remain, the orchestrator resets the task to `open` — the task never enters a terminal status during this cycle. The intermediate state between exit code 1 and the reset to `open` is transient and internal to the orchestrator; the ledger transitions directly from `in_progress` to `open`.

The orchestrator dispatches tasks using `Store.ReadyTasks()`, which returns tasks where:
- `status = open`
- All dependency predecessors have reached terminal status
- `assignee` is empty

Results are sorted by priority (ascending), then lexicographic task ID for deterministic tiebreaking (per LDG-07).

### 7.2 Level 2 — Feature Lifecycle

A feature lifecycle is a coordination graph encoded as tasks and dependency edges in the ledger. The orchestrator sees only tasks and edges — lifecycles are an emergent structure.

Structure:

```
plan-task → build-task* → V&V-task* → pr-gate-task
```

All lifecycle tasks are standard ledger tasks with role tags:

| Tag | Role | Instruction File |
|-----|------|-----------------|
| `role:planner` | Planner | CHARLIE.md (or equivalent) |
| `role:builder` | Builder | OOMPA.md (or equivalent) |
| `role:verifier` | Verifier | LOOMPA.md (or equivalent) |
| `role:gate` | Gate | Gate handler — agent or deterministic script per BVV-AI-02 |

Dependency edges encode ordering:
- Build tasks depend on the plan task and on their natural predecessors (e.g., migrations before entities)
- V&V tasks depend on the build tasks they validate
- The PR gate task depends on all V&V tasks

*(Non-normative)* Example lifecycle for a feature with work package input:

```
Input: work-packages/feature-x/
  functional-spec.md    (3 capabilities: client CRUD, validation rules, event publishing)
  technical-spec.md     (hexagonal layout, PostgreSQL, domain events via NATS)
  vv-spec.md            (per-capability verification: trace + BR coverage + error paths)

Planner decomposes into:

plan-feature-x              (role:planner, no deps)
build-migrations            (role:builder, depends: plan-feature-x)
build-domain-entities       (role:builder, depends: build-migrations)
build-service-handlers      (role:builder, depends: build-domain-entities)
build-event-publishing      (role:builder, depends: build-service-handlers)
vv-client-crud              (role:verifier, depends: build-service-handlers)
vv-validation-rules         (role:verifier, depends: build-service-handlers)
vv-event-publishing         (role:verifier, depends: build-event-publishing)
pr-gate-feature-x           (role:gate, depends: vv-client-crud, vv-validation-rules, vv-event-publishing)
```

### 7.3 Lifecycle Entry Point

`[BVV-TG-05]` The orchestrator MUST NOT create the initial plan task. Lifecycle initiation is a human or external-system action.

A lifecycle begins when a human or CLI command creates the plan task in the ledger:

```bash
bd create --title "plan-feature-x" \
  --type plan \
  --label "role:planner" \
  --label "branch:feature-x" \
  --description "Work package: work-packages/feature-x/"
```

The plan task's description references the work package path. `ReadyTasks()` surfaces it; the orchestrator dispatches it to a planner worker.

#### 7.3.1 Branch Creation

`[BVV-TG-06]` The planning agent MUST create the feature branch from the target branch (typically `main`) if it does not already exist. Branch creation is part of the planner's execution, not the orchestrator's responsibility.

The plan task's `branch` label determines the branch name. All subsequent lifecycle tasks operate on this branch.

### 7.4 Planning Agent

The planning agent decomposes a work package into an executable task graph.

**Input:** A work package — a bundle of specifications:

- **Functional spec** — capabilities, use cases, acceptance criteria
- **Technical spec** — architecture decisions, technology choices, implementation constraints
- **V&V spec** — verification approach, test strategy, validation criteria per capability

**Output:** Tasks with role tags and dependency edges in the ledger.

**Process:**

1. Parse the work package specifications to identify implementable units
2. Create build tasks (one per implementable unit) with `role:builder` tag. Each task's body contains: target files, success criteria, specification references.
3. Create V&V tasks (one per build task or per capability) with `role:verifier` tag. Each task's body contains: verification criteria, specification references, dependency on corresponding build task(s).
4. Create a PR gate task with `role:gate` tag, depending on all V&V tasks
5. Wire dependency edges: build tasks respect their natural ordering (migrations before entities before services before handlers); V&V tasks depend on their build counterparts

The planning agent runs as the first lifecycle task, dispatched like any other agent. It creates tasks with `bd create --depends-on` edges.

**Priority assignment:** The planning agent SHOULD assign priorities to control dispatch ordering among independent tasks. RECOMMENDED scheme: build tasks get priority by dependency depth (deeper = higher number = dispatched later), V&V tasks inherit their build dependency's priority, and the PR gate gets the highest number. Without explicit priorities, all tasks default to 0 and the lexicographic tiebreaker (LDG-07) determines order.

> *Note (non-normative):* This specification does not define the work package format. Implementations may use markdown, YAML, or structured JSON — any format the planning agent can parse. BVV-TG-04 traceability references may use any stable identifier scheme (section numbers, requirement IDs, heading anchors).

`[BVV-TG-02]` The planning agent MUST be idempotent. Re-running on the same work package MUST produce the same lifecycle graph without creating duplicate tasks. The planning agent detects a previous run by querying the ledger for tasks with the lifecycle's branch label. If matching tasks exist, the planning agent MUST reconcile rather than recreate:

- Tasks in `open` status MAY be updated (e.g., body, dependencies) if the work package has changed.
- Tasks in `in_progress` or `completed` status MUST NOT be modified (see BVV-TG-03).
- Tasks in `failed` or `blocked` status MAY be reset to `open` by the planning agent if the blocking condition has been resolved.
- New tasks MAY be added to the graph (e.g., if the work package expanded). The planning agent MUST wire dependency edges so new tasks integrate with the existing graph.

The branch label scopes work package identity — all tasks sharing the same `branch:<name>` label belong to one lifecycle.

`[BVV-TG-03]` The planning agent MUST NOT modify tasks created by previous planning runs that are already `in_progress` or `completed`.

`[BVV-TG-04]` Each build task MUST reference the specification sections it implements. Each V&V task MUST reference the acceptance or verification criteria it validates. Traceability is the planning agent's responsibility.

`[BVV-TG-11]` If the planning agent fails (exit code 1) after creating some tasks but before completing the graph, the partially-created tasks remain in the ledger. On retry, the planning agent MUST reconcile the partial graph per BVV-TG-02 — it queries existing tasks for the branch label and completes the graph rather than starting from scratch. If the planning agent fails terminally (retries exhausted), the orchestrator creates an escalation task. The partially-created tasks remain in the ledger with status `open` but are not dispatchable because well-formedness validation (BVV-TG-07 through BVV-TG-10) has not passed. A Level 1 orchestrator that skips validation MAY dispatch partial graphs — this is a known risk of Level 1 conformance.

`[BVV-TG-12]` If a lifecycle terminates with the plan task as the only task (planner failed before creating any build/V&V tasks) or with all tasks in terminal failure states, the feature branch — if created — is NOT automatically deleted. Branch cleanup is a human or external-system action, outside the scope of this specification.

### 7.5 Task Graph Well-Formedness

After the planning agent completes, the orchestrator MAY validate the task graph before dispatching build tasks. A well-formed lifecycle graph satisfies these constraints:

`[BVV-TG-07]` Every task in the lifecycle scope MUST have a `role` tag with a value that maps to a configured instruction file. Tasks without a valid role tag are undispatchable.

`[BVV-TG-08]` The task graph MUST be acyclic. This is enforced by the ledger's `AddDep` operation (LDG-06), which rejects edges that create cycles.

`[BVV-TG-09]` The lifecycle MUST contain exactly one task with `role:gate`. This task MUST depend (directly or transitively) on all V&V tasks.

`[BVV-TG-10]` Every task in the lifecycle MUST be reachable from the plan task via dependency edges. Orphan tasks (no path from plan task) indicate a planning error.

> *Note (non-normative):* Well-formedness validation is RECOMMENDED but not required at Level 1. A Level 1 orchestrator dispatching from a pre-populated ledger may skip validation if a trusted source created the task graph. Level 2 SHOULD validate after the planner completes.

---

## 8 Dispatch Semantics

### 8.1 DAG-Driven Dispatch

Each dispatch tick, the orchestrator queries `Store.ReadyTasks()` and assigns ready tasks to idle workers in parallel. The dependency graph drives ordering — no phase advancement logic exists.

`[BVV-DSP-01]` The orchestrator MUST dispatch all ready tasks that have available workers, up to `MaxWorkers`.

`[BVV-DSP-02]` The orchestrator MUST NOT hold ready tasks waiting for other tasks to complete. The only constraint on dispatch is worker availability.

#### 8.1.1 Lifecycle Scoping

The ledger may contain tasks from multiple feature lifecycles. The orchestrator scopes dispatch to a single lifecycle.

`[BVV-DSP-08]` The orchestrator MUST filter `ReadyTasks()` to the lifecycle in scope. Scoping MUST use the task's branch label (e.g., `branch:feature-x`), not content inspection.

> *Note (non-normative, Level 1):* "Lifecycle" is a Level 2 concept (Section 7.2). At Level 1, this requirement applies to any label-based dispatch scope — the orchestrator filters tasks by branch label regardless of whether a formal lifecycle exists.

Implementations MAY scope via:

- (a) A label filter passed to `ReadyTasks()` (e.g., `bd ready --label "branch:feature-x"`)
- (b) One engine instance per lifecycle, each configured with a branch scope

#### 8.1.2 Concurrent Lifecycles

Multiple feature lifecycles MAY execute concurrently. Each lifecycle runs as an independent orchestrator instance with its own lifecycle lock, worker pool, and branch scope.

`[BVV-DSP-12]` Concurrent lifecycle instances MUST use distinct lifecycle locks (scoped by branch name). Worker pools MAY be shared or separate — this is an implementation choice. Shared pools require worker reuse semantics (WKR-07 through WKR-09) to clean workspace state between lifecycles.

> *Note (non-normative, Level 1):* At Level 1, "lifecycle instance" means any dispatch scope filtered by branch label. Distinct locks per branch prevent concurrent orchestrator instances from dispatching the same tasks.

> *Note (non-normative):* The simplest deployment is one orchestrator process per lifecycle, each running `bvv run --branch feature-x`. For higher throughput, a single process may manage multiple lifecycles with separate dispatch loops sharing a worker pool.

#### 8.1.3 Termination

The dispatch loop terminates when ALL tasks in the lifecycle scope have reached terminal status (`completed`, `failed`, or `blocked`) AND no workers have active sessions.

### 8.2 Role-Based Routing

The orchestrator reads the task's `role` metadata tag and routes it to the appropriate instruction file.

`[BVV-DSP-03]` Routing MUST be based on task metadata (`role` tag), never on task content. This is a direct consequence of DSN-01 through DSN-03 (Zero Content Inspection).

`[BVV-DSP-03a]` If a task has an unknown or missing role tag, the orchestrator MUST create an escalation task and MUST NOT attempt to dispatch the task.

### 8.3 Completion Protocol

The orchestrator determines task outcome from the agent process exit code, replacing the command interface (ITF-03 through ITF-05) from the orchestration specification.

> *Note (non-normative):* The promise protocol (`ITERATION_DONE` / `COMPLETE` / `BLOCKED`) was designed for an iteration loop where the orchestrator cannot distinguish task state from branch state. In the one-task-per-session model, the agent signals only task outcome. The ledger provides branch state.

#### 8.3.1 Exit Codes

| Exit Code | Meaning | Orchestrator Action |
|-----------|---------|-------------------|
| `0` | Task succeeded | Set task status to `completed`. Release worker. |
| `1` | Task failed | Invoke retry protocol (Section 11.1). If retries remain, reset task to `open` for re-dispatch. If retries exhausted, set task status to `failed` and create escalation task. |
| `2` | Task blocked | Set task status to `blocked`. Release worker. Record gap (Section 11.3). Do NOT retry. |
| `3` | Session handoff | Task status remains `in_progress`. Orchestrator spawns a new session for the same worker and task (Section 10.1). |

These exit codes are specific to this specification and intentionally differ from the orchestration specification's ITF-06, which defines exit codes for the replaced command interface.

#### 8.3.1a Task Closure Ownership

`[BVV-DSP-09]` The orchestrator is authoritative for task status transitions in the ledger. The orchestrator translates agent exit codes to ledger status updates (Section 8.3.1). If an agent also writes a terminal status (e.g., via `bd close`), the operation is idempotent — the orchestrator's write is authoritative regardless of agent behavior.

> *Note (non-normative):* Existing agents (Oompa, Loompa) close their own tasks via `bd close <id>`. Under orchestrator-managed dispatch, instruction files remove this step. The orchestrator closes the task after observing exit code 0. Beads `close` on an already-closed task is a no-op, so dual writes are harmless. Implementations MAY tolerate agent-side closure for backward compatibility.

#### 8.3.2 Diagnostic Tags

Agents MAY emit structured stdout tags for richer diagnostics:

```
<outcome>DONE reason="All SCs verified, 3 files changed"</outcome>
<outcome>FAIL reason="3x structural failure on migration rollback"</outcome>
<outcome>BLOCKED reason="Dependency mf3-b12 files missing on branch"</outcome>
<outcome>HANDOFF</outcome>
```

These tags are informational. The orchestrator SHOULD capture them in the audit trail but MUST NOT use them for dispatch or status decisions.

`[BVV-DSP-04]` The orchestrator MUST determine task outcome from the process exit code, not from parsing agent output content.

`[BVV-DSP-14]` Exit code `3` (handoff) MUST NOT change the task's ledger status. The task remains `in_progress` and the orchestrator spawns a new session for the same worker and task.

#### 8.3.3 Backward Compatibility

*(Non-normative)* Existing agents using the `<promise>` tag protocol can be wrapped in a shell script translating promise tags to exit codes. The wrapper invokes the agent once per task (per BVV-DSN-02), so both `ITERATION_DONE` and `COMPLETE` map to exit code 0. The one-task-per-session model absorbs the distinction — the agent runs once and exits.

```bash
#!/bin/bash
# wrapper.sh — translates promise tags to exit codes
# Invoked once per task. ITERATION_DONE and COMPLETE both mean
# "the single assigned task succeeded" in the BVV model.
"$@" | tee /dev/stderr > "$OUTPUT"
PROMISE=$(grep -oE '<promise>(COMPLETE|BLOCKED|ITERATION_DONE)</promise>' "$OUTPUT" | tail -1 | sed 's/<[^>]*>//g')
case "$PROMISE" in
  ITERATION_DONE|COMPLETE) exit 0 ;;
  BLOCKED) exit 2 ;;
  *) exit 1 ;;
esac
```

### 8.4 One Task Per Session

`[BVV-DSP-05]` The orchestrator MUST NOT reuse a session for multiple tasks. Each task gets a fresh session.

Session lifecycle:
1. Orchestrator assigns task to worker (`Store.Assign`)
2. Orchestrator spawns a tmux session using the preset for the task's role (per ATA-01 through ATA-03). The preset defines the agent binary, system prompt flag, and launch arguments. The orchestrator reads the instruction file (stripping YAML frontmatter) and passes its content via the preset's `SystemPromptFlag` (e.g., `claude -p --system-prompt-file <path>`). Environment variables `ORCH_TASK_ID` and any preset-defined variables are injected into the session (see Section 8.4.1).
3. Agent reads its assigned task (see Task Discovery below)
4. Agent executes the task
5. Agent exits with exit code 0/1/2/3
6. Orchestrator processes exit code per Section 8.3.1
7. Session ends

#### 8.4.1 Task Discovery

The orchestrator injects the assigned task ID as an environment variable.

`[BVV-DSP-06]` The orchestrator MUST set `ORCH_TASK_ID` in the worker session's environment. The agent reads its task from the ledger: `bd show $ORCH_TASK_ID --json`.

`[BVV-DSP-15]` The orchestrator owns task selection and assignment. The orchestrator MUST assign tasks via `Store.Assign()` and inject the task ID via `ORCH_TASK_ID` (BVV-DSP-06). Task selection is an orchestrator concern.

> *Note (non-normative):* Existing instruction files (OOMPA.md, LOOMPA.md) use `bd ready --json -n 1` for task selection. For orchestrator-managed dispatch, the "select task" step changes to `bd show $ORCH_TASK_ID --json`. The rest of the instruction file (plan, build, verify, report) remains unchanged.

### 8.5 Parallel Workspace Strategy

Build agents modify the same codebase. Parallel dispatch without workspace isolation produces git conflicts.

#### 8.5.1 DAG Serialization (Default)

`[BVV-DSP-07]` When the planning agent creates build tasks, it MUST add dependency edges between tasks that modify overlapping files or packages. Tasks with disjoint file sets MAY be independent (parallel-eligible).

In default mode, the planning agent serializes all build tasks via dependency edges. Parallel dispatch applies only to V&V tasks, which are read-heavy and can run concurrently without conflicts.

#### 8.5.2 Worktree Isolation (Advanced)

For tasks that cannot be serialized via dependencies but might conflict, the orchestrator MAY allocate git worktrees per worker, reusing workspace isolation semantics from the orchestration specification (WSP-01 through WSP-03). Each worker operates in its own worktree and rebases completed work onto the feature branch.

`[BVV-DSP-13]` When worktree merge-back (rebase onto feature branch) fails due to conflicts, the orchestrator MUST treat the task as failed (equivalent to exit code 1) and invoke the retry protocol (Section 11.1). On retry, the orchestrator creates a fresh worktree from the current feature branch HEAD. The agent re-executes the task against the updated branch state. If merge-back fails after retries are exhausted, the orchestrator sets the task to `failed` and creates an escalation task with the conflict details.

V&V tasks run against the committed state of the feature branch (not a worktree), since they primarily read rather than write.

#### 8.5.3 V&V Commit Conflicts

Verifier agents may fix defects and commit changes. When multiple V&V tasks run in parallel, their fix commits can conflict.

`[BVV-DSP-10]` The planning agent MUST add dependency edges between V&V tasks that are likely to modify overlapping files. V&V tasks targeting the same domain or package SHOULD be serialized.

`[BVV-DSP-11]` If parallel V&V tasks produce conflicting commits despite dependency planning, the second commit fails. The agent MUST detect the conflict (git push/commit failure), rebase, resolve, and retry. If resolution fails, the agent signals exit code 1 (failed). The orchestrator retries per Section 11.1.

In default (single-worker) mode, V&V tasks are serialized by worker availability, eliminating conflicts entirely.

---

## 9 PR Gate

The PR gate is the terminal task in a feature lifecycle. It becomes ready when all V&V tasks reach terminal status.

### 9.1 Gate Execution

A gate handler (which MAY be an agent or a deterministic script per BVV-AI-02) executes the following steps:

1. Check predecessor statuses (via ledger query). If any predecessor has status `failed` or `blocked`, skip PR creation and exit with code 1 (per BVV-GT-03)
2. Create a pull request from the feature branch to the target branch (typically `main`)
3. Wait for CI status checks to complete
4. If all checks pass, exit with code 0 (task succeeded)
5. If any check fails, exit with code 1 (task failed)

The orchestrator translates the exit code to a ledger status update per Section 8.3.1, consistent with BVV-DSP-09. On failure, the orchestrator creates an escalation task with the gate handler's diagnostic tags (Section 8.3.2).

`[BVV-GT-01]` The orchestrator MUST NOT merge the pull request automatically. PR creation is the terminal action. Merge requires human approval or a separate automation outside the scope of this specification.

`[BVV-GT-02]` Gate failure on one feature lifecycle MUST NOT block the orchestrator from processing other feature lifecycles running concurrently.

`[BVV-GT-03]` The gate task becomes ready when all its dependency predecessors reach terminal status. If any predecessor has status `failed` or `blocked`, the gate handler MUST NOT create a pull request. Instead, the gate handler MUST set the gate task status to `failed` and create an escalation task listing the failed/blocked predecessors. This is a consequence of BVV-S-07 (Bounded Degradation) applied at the gate level — a PR is only created when all V&V tasks completed successfully or the remaining gaps fall within tolerance.

> *Note (non-normative):* The planning agent controls gate prerequisites via dependency edges. For non-critical V&V tasks, the planning agent MAY omit the dependency edge to the gate, allowing it to proceed without that task. The gap counter (Section 11.3) still tracks the failure.

### 9.2 Gate Failure Recovery

When a PR gate fails (CI checks fail), the escalation task contains the failure details. A human or triage agent may:

- Fix the issue and re-trigger CI (the gate task transitions back to `in_progress` via manual intervention)
- Close the lifecycle by marking the gate task as `blocked`
- Re-run specific V&V tasks to address the failure

---

## 10 Session Protocol

### 10.1 Infrastructure Handoff (Orchestrator-Managed)

Session handoff follows the orchestration specification (CTY-01 through CTY-09). When an agent signals exit code `3` (handoff), the orchestrator:

1. Reads any handoff file the agent wrote to the workspace root
2. Spawns a new session for the same worker and task
3. The new session discovers the handoff file via the instruction file's orient phase

The handoff file schema follows the orchestration specification:

| Field | Type | Description |
|-------|------|-------------|
| `version` | integer | Schema version. |
| `reason` | enum | `context_pressure`, `explicit`, `crash_recovery`. |
| `task_id` | string | Current task ID. |
| `summary` | text | Free-text summary of work done and remaining. |
| `decisions_made` | text[] | Decisions and rationale. |
| `approaches_rejected` | text[] | Approaches tried and abandoned. |
| `workspace_state` | enum | `clean`, `dirty`, `conflicted`. |
| `files_modified` | string[] | Paths relative to workspace root. |
| `session_number` | integer | Monotonically incremented by each successor. |

### 10.2 Agent Memory (Agent-Managed)

Agents maintain cross-session memory independently of the orchestrator.

`[BVV-SS-01]` The orchestrator MUST NOT read, write, or depend on agent memory files. Agent memory is opaque to the infrastructure layer.

The canonical agent memory format is PROGRESS.md, containing:

- **Codebase Patterns** section — reusable knowledge discovered during execution
- **Per-task entries** — status, changes, success criteria verification, learnings
- **Archival** — the agent is responsible for keeping the file within context budget (e.g., archiving old entries when the file exceeds 20 completed tasks)

> *Note (non-normative):* PROGRESS.md is the agent's memory, not the orchestrator's. The orchestrator tracks task state in the ledger. The two may diverge (e.g., agent writes PROGRESS.md entry before signaling completion). The ledger is authoritative.

### 10.3 JSONL Audit Trail (Orchestrator-Managed)

The orchestrator maintains an append-only JSONL event log, extending the `EventLog` pattern from the orchestration specification (OBS-03 run ID propagation, OBS-04 system state view).

Event kinds for build-verify-validate workflows:

| Event Kind | Emitted When |
|------------|-------------|
| `task_dispatched` | Task assigned to worker and session spawned. |
| `task_completed` | Agent exited with code 0. Task marked completed. |
| `task_failed` | Agent exited with code 1 and retries exhausted. Task marked failed (terminal). |
| `task_retried` | Agent exited with code 1 and retries remain. Task reset to open for re-dispatch. |
| `task_blocked` | Agent exited with code 2. Task marked blocked. |
| `task_handoff` | Agent exited with code 3. New session spawned for same task. |
| `worker_spawned` | Tmux session created for a worker. |
| `worker_released` | Worker returned to idle pool. |
| `gap_recorded` | Non-critical task failure recorded in gap tracker. |
| `escalation_created` | Escalation task created for human review. |
| `lifecycle_started` | First task in lifecycle scope dispatched. |
| `lifecycle_completed` | All tasks in lifecycle scope terminal. |
| `gate_created` | PR created by gate handler. |
| `gate_passed` | CI checks passed. Gate task completed. |
| `gate_failed` | CI checks failed. Escalation created. |
| `escalation_resolved` | Previously-terminal task re-opened by human intervention (see BVV-S-02a). |
| `handoff_limit_reached` | Task exceeded maximum handoff count (see BVV-L-04). Treated as failure. |

Resume scans the event log for `gap_recorded` events to recover the monotonic gap counter (per BVV-ERR-05).

---

## 11 Error Semantics

### 11.1 Retry Protocol

When an agent exits with code 1 (task failed), the orchestrator invokes the retry protocol:

1. Increment the task's retry counter
2. If retries remain (configurable, RECOMMENDED default: 2), reset the task to `open` for re-dispatch with an escalating timeout (RECOMMENDED: 1.5x base timeout per retry). The task has NOT reached a terminal status — the orchestrator treats the exit as a non-terminal failure.
3. If retries exhausted, set task status to `failed` (terminal) and create an escalation task

> *Note:* The task only transitions to the terminal `failed` status when retries are exhausted. During the retry cycle, the task oscillates between `open` (ready for dispatch) and `in_progress` (agent executing). This is consistent with BVV-S-02 (Terminal Status Irreversibility): terminal status is never reversed. The `open` → `in_progress` → `open` cycle is a non-terminal retry loop, not a regression from a terminal state.

`[BVV-ERR-01]` The retry counter MUST be monotonic within a lifecycle run. Resume MUST recover the retry count from the event log.

`[BVV-ERR-02]` The escalating timeout MUST apply to the agent session duration, not to individual operations within the session.

`[BVV-ERR-02a]` The orchestrator MUST enforce a configurable base session timeout for all task executions, including first attempts. When the timeout expires, the orchestrator MUST terminate the session and treat the outcome as exit code 1 (task failed), invoking the retry protocol. RECOMMENDED default: 30 minutes. The escalating timeout (BVV-ERR-02) is computed as a multiplier on this base timeout.

### 11.2 Blocked Tasks

When an agent exits with code 2 (task blocked), the orchestrator:

1. Sets the task status to `blocked`
2. If the task is critical (`critical:true` label), aborts the lifecycle immediately (per BVV-ERR-03). If the task is non-critical, records a gap (Section 11.3).
3. Does NOT retry — `blocked` is terminal for dispatch

Blocked tasks may be re-opened by:
- Human intervention (manually setting status to `open`)
- A planning agent re-run that resolves the blocking condition

### 11.3 Gap Tolerance

A task is **critical** if it carries the `critical:true` label in the ledger. A task without this label is non-critical. The planning agent SHOULD mark tasks as critical when their failure would make the feature branch unshippable (e.g., the plan task, migration tasks, core domain tasks). V&V tasks for secondary capabilities MAY be left non-critical.

`[BVV-ERR-03]` Non-critical task failures (`failed` after retries, or `blocked`) MUST increment the gap counter. Critical task failures MUST cause immediate lifecycle abort — the orchestrator MUST stop dispatching new tasks and create an escalation task, regardless of the gap counter.

`[BVV-ERR-04]` The gap tolerance is configurable per lifecycle run (RECOMMENDED default: 3). When the gap count reaches the tolerance, the orchestrator MUST stop dispatching new tasks and create an escalation task.

> *Note (non-normative):* The gap tolerance is a dispatch threshold, not a hard ceiling. Tasks already in-flight when the tolerance is reached continue to completion. If those in-flight tasks also fail, the actual gap count may exceed the configured tolerance by up to (MaxWorkers - 1). Implementations that require a strict ceiling SHOULD drain active sessions before checking the gap count.

`[BVV-ERR-05]` The gap counter MUST be monotonic. Resume MUST recover the gap count by scanning `gap_recorded` events in the audit trail.

### 11.4 Transient vs. Structural Failures

The orchestrator cannot distinguish transient from structural failures. This distinction belongs to agent-level judgment:

- **Transient** (Docker down, flaky test, port conflict) — the agent retries internally per its instruction file (RECOMMENDED: up to 5 internal retries with different remediation per attempt)
- **Structural** (wrong architecture, missing dependency, linter rejects design) — the agent exits with code 1 or 2 after exhausting internal retries

The orchestrator sees only exit codes. Per DSN-01 through DSN-03, the orchestrator MUST NOT classify failures.

---

## 11a Resume and Recovery

### 11a.1 Interrupted Lifecycle Detection

Orchestrator crash, operator kill (SIGINT/SIGTERM), or infrastructure failure may interrupt a lifecycle. The orchestrator detects interrupted lifecycles by stale lifecycle locks (Section 12.4).

`[BVV-ERR-06]` When acquiring the lifecycle lock, if a lock file exists and its timestamp exceeds the staleness threshold, the orchestrator MUST treat the lock as abandoned and acquire it. The previous orchestrator instance is assumed dead.

### 11a.2 State Reconciliation

On resume, the orchestrator reconciles ledger state with observed reality:

1. **Stale assignments:** Tasks with status `assigned` or `in_progress` but no live session are reset to `open`. The orchestrator verifies session liveness by checking whether the tmux session exists (Section 11a.4).
2. **Orphaned sessions:** Tmux sessions that exist but have no corresponding `in_progress` task in the ledger are killed.
3. **Gap recovery:** The orchestrator scans the audit trail for `gap_recorded` events and restores the monotonic gap counter (per BVV-ERR-05).
4. **Retry recovery:** The orchestrator scans the audit trail for `task_retried` and `task_failed` events and restores per-task retry counts (per BVV-ERR-01).
5. **Human re-open detection:** The orchestrator scans the lifecycle scope for tasks that were previously in a terminal status (`completed`, `failed`, `blocked`) but are now `open`. For each such task, the orchestrator resets the retry counter and handoff counter to zero and emits an `escalation_resolved` event in the audit trail (per BVV-S-02a).

`[BVV-ERR-07]` Reconciliation MUST be completed before the dispatch loop starts. No tasks are dispatched during reconciliation.

`[BVV-ERR-08]` If a task has status `in_progress` and its tmux session is still alive, the orchestrator MUST NOT reset the task. The session continues and the orchestrator resumes monitoring it.

### 11a.3 Graceful Shutdown

On SIGINT or SIGTERM, the orchestrator:

1. Stops dispatching new tasks (no new `Assign` calls)
2. Waits for active agent sessions to reach a natural exit point (configurable timeout, RECOMMENDED: 30 seconds)
3. If sessions do not exit within the timeout, records their state in the audit trail
4. Releases the lifecycle lock
5. Exits

`[BVV-ERR-09]` Graceful shutdown MUST NOT modify task statuses in the ledger. Active tasks remain `in_progress`. The next orchestrator instance reconciles them on resume.

`[BVV-ERR-10]` The lifecycle lock MUST be released on all exit paths, including signal-triggered shutdown. The orchestrator MUST register a cleanup handler at startup.

### 11a.4 Liveness Detection

The watchdog (SUP-01 through SUP-04) detects dead sessions by process-level inspection, not by heartbeat writes.

`[BVV-ERR-11]` The watchdog MUST detect session liveness by verifying the tmux session exists (e.g., `tmux has-session -t <name>`). If the session is absent but the worker has an active assignment, the session is dead.

`[BVV-ERR-11a]` When the watchdog restarts a dead session for a task that remains `in_progress`, the orchestrator MUST emit a `task_handoff` event in the audit trail. This restart counts toward the handoff limit (BVV-L-04). If the handoff limit has been reached, the orchestrator MUST NOT restart the session and MUST instead treat the dead session as exit code 1 (task failed), invoking the retry protocol (Section 11.1).

> *Note (non-normative):* The orchestration specification defines a heartbeat mechanism (ITF-03 `heartbeat` command). This specification replaces it with tmux process presence detection (Section 8.3). Tmux-based liveness is simpler and eliminates false-positive "stale heartbeat" failures where the agent is alive but busy.

The circuit breaker (SUP-05, SUP-06) operates unchanged: after N consecutive rapid failures of the same worker (RECOMMENDED: N=3, rapid = session < 60 seconds), the orchestrator suspends the worker and creates an escalation task.

---

## 12 Safety Properties

These properties MUST hold for all conforming implementations.

### 12.1 Terminal Status Irreversibility (BVV-S-02)

`[BVV-S-02]` A task that the orchestrator has set to a terminal status (`completed`, `failed`, `blocked`) MUST NOT return to a non-terminal status (`open`, `assigned`, `in_progress`) through orchestrator action. "Orchestrator action" means any status write initiated by the orchestrator process — including its dispatch loop, retry protocol, reconciliation, and watchdog. CLI commands invoked by a human operator (e.g., `bd update --status open`) are external actions outside this constraint.

During the retry cycle (Section 11.1), a task oscillates between `open` and `in_progress` — these are non-terminal statuses. The task only reaches terminal `failed` when retries are exhausted. This oscillation does not violate irreversibility.

`[BVV-S-02a]` Human intervention MAY re-open a `blocked` or `failed` task by writing directly to the ledger. When the orchestrator detects a previously-terminal task has returned to `open` status during reconciliation (Section 11a.2), it MUST treat it as a new dispatchable task. The orchestrator MUST reset the retry counter and the handoff counter (BVV-L-04) for that task to zero. The orchestrator MUST emit an `escalation_resolved` event in the audit trail.

### 12.2 Single Assignment (BVV-S-03)

`[BVV-S-03]` At most one worker MUST be assigned to a given task at any time. Concurrent assignment to the same task MUST be prevented by atomic ledger operations (LDG-08 through LDG-10).

### 12.3 Dependency Ordering (BVV-S-04)

`[BVV-S-04]` No task MUST be dispatched before all its dependency predecessors have reached terminal status (LDG-04). The orchestrator MUST NOT dispatch a task whose predecessors include any non-terminal task.

### 12.4 Lifecycle Exclusion (BVV-S-01)

`[BVV-S-01]` At most one orchestrator instance MUST execute a given lifecycle (scoped by branch) at any time. The lifecycle lock MUST be released on ALL exit paths — success, failure, abort, and interrupt (see also BVV-ERR-10).

### 12.5 Zero Content Inspection (BVV-S-05)

`[BVV-S-05]` The orchestrator MUST NOT read agent output content, agent memory files, or task body content for dispatch or status decisions. All routing MUST use task metadata tags. All completion detection MUST use exit codes.

### 12.6 Gate Authority (BVV-S-06)

`[BVV-S-06]` PR gate failure MUST block merge until human action. No agent or orchestrator MAY bypass a failed gate.

> *Note (non-normative, Level 1):* The PR gate is a Level 2 concept (Section 9). At Level 1, this property is vacuously satisfied — no gate exists to fail, no PR is created.

### 12.7 Bounded Degradation (BVV-S-07)

`[BVV-S-07]` The lifecycle MUST NOT create a PR if the gap count has reached or exceeded the tolerance:

```
pr_created ⟹ |gaps| < gap_tolerance
```

> *Note (non-normative, Level 1):* Like BVV-S-06, this property is vacuously satisfied at Level 1 — no PR mechanism exists. The gap counter still operates and still triggers lifecycle abort (BVV-ERR-04), but the formal invariant above is trivially true because `pr_created` is always false.

### 12.8 Assignment Durability (BVV-S-08)

`[BVV-S-08]` A session crash MUST NOT cause assignment loss. The ledger record MUST persist through any number of session restarts (inherited from RCV-03, RCV-04).

### 12.9 Workspace Write Serialization (BVV-S-09)

`[BVV-S-09]` At most one build agent MUST write to the feature branch at any time. The planning agent enforces this by serializing build tasks via dependency edges (BVV-DSP-07). When worktree isolation (Section 8.5.2) is used, merge-back to the feature branch MUST be serialized.

> *Note (non-normative, Level 1):* BVV-DSP-07 is a Level 2 requirement (Section 8.5.1). At Level 1, this property depends on the external task graph providing serialization — whoever populates the ledger (human operator) is responsible for adding dependency edges between tasks that modify overlapping files.

> *Note (non-normative):* This adapts orch-spec S10 (Workspace Isolation: "no two workers write to same workspace simultaneously") for the DAG model. In DAG-serialized mode, dependency edges prevent concurrent writes. In worktree mode, the merge-back step is the serialization point. V&V tasks are read-heavy and do not require this property; when V&V tasks fix defects, BVV-DSP-10 and BVV-DSP-11 govern conflict handling.

### 12.10 Watchdog-Retry Non-Interference (BVV-S-10)

`[BVV-S-10]` Watchdog session restarts and orchestrator retry attempts MUST NOT double-count against the retry budget. A watchdog restart of a dead session counts as a handoff (BVV-ERR-11a), not a retry. A retry occurs only when the orchestrator processes exit code 1 (Section 11.1). These are orthogonal operations: watchdog restarts preserve the current attempt; retries reset the attempt.

> *Note (non-normative):* This adapts orch-spec S11 (Watchdog-Retry Non-Interference). Orch-spec S1 (Monotonic Progress: "phase_index never regresses") is subsumed by BVV-S-02 (Terminal Status Irreversibility) — in the DAG model there is no phase index, and the equivalent invariant is that completed task count never decreases through orchestrator action, which BVV-S-02 guarantees.

---

## 13 Liveness Properties

### 13.1 Eventual Termination (BVV-L-01)

`[BVV-L-01]` Given a finite task graph, finite retry budget, finite gap tolerance, finite handoff limit (BVV-L-04), and finite base session timeout (BVV-ERR-02a), every lifecycle MUST terminate. The dispatch loop exits when all tasks in scope are terminal.

> *Note (non-normative):* This property holds for a closed system — no external intervention during the lifecycle run. Human re-opens (BVV-S-02a) inject fresh retry and handoff budgets, creating an unbounded loop if applied indefinitely. The "finite retry budget" premise is violated by external budget injection. Implementations that need guaranteed termination bounds SHOULD disable or rate-limit human re-opens during automated runs.

### 13.2 Lock Release (BVV-L-02)

`[BVV-L-02]` The lifecycle lock MUST have a configurable staleness threshold (RECOMMENDED: 4 hours). A crashed orchestrator's lock MUST be recoverable by a subsequent orchestrator instance after the staleness period.

### 13.3 Worker Recovery (BVV-L-03)

`[BVV-L-03]` The watchdog MUST detect dead worker sessions and restart them (SUP-01 through SUP-04). The circuit breaker MUST prevent cascading restarts (SUP-05, SUP-06): after N consecutive rapid failures (RECOMMENDED: N=3, rapid = session duration < 60s), the worker is removed from rotation and an escalation task is created.

### 13.4 Bounded Handoff (BVV-L-04)

`[BVV-L-04]` The orchestrator MUST enforce a maximum handoff count per task (configurable, RECOMMENDED default: 5). When the handoff count reaches the limit, the orchestrator MUST treat the next exit code `3` as exit code `1` (task failed) and invoke the retry protocol (Section 11.1). The handoff counter MUST be monotonic within a lifecycle run and recoverable from the audit trail by counting `task_handoff` events for the task.

> *Note (non-normative):* The handoff counter is intentionally NOT reset on retry (exit code 1 with retries remaining). After a handoff-limit breach, subsequent retry attempts cannot use handoff — exit code 3 converts to exit code 1 immediately. Rationale: a task that exhausted its handoff budget once will likely exhaust it again. The monotonic counter promotes fail-fast behavior and prevents unbounded context-pressure loops. The counter IS reset when a human re-opens a failed or blocked task (BVV-S-02a), granting a fresh budget under human supervision.

---

## Appendix A: Reference Agent Profiles

*(Non-normative)*

This appendix maps existing production agents to the roles and names defined in this specification. The profiles describe current behavior (pre-BVV). Adapting for orchestrator-managed dispatch requires three changes: (1) replace `bd ready` with `bd show $ORCH_TASK_ID` for task discovery (Section 8.4.1), (2) remove `bd close` calls from the report phase (Section 8.3.1a), and (3) add exit code signaling per Section 8.3.1.

### A.1 Oompa — Builder

**Instruction file:** `oompa/OOMPA.md`

**Phases:** Orient → Plan → Build → Verify → Report

| Phase | Key Actions |
|-------|------------|
| Orient | Read PROGRESS.md, read assigned task from beads, verify branch, claim task. |
| Plan | List files to create/modify, map success criteria to code locations. |
| Build | Implement code following implementation sequence (migrations → entities → services → handlers → wiring → frontend). Run tests per layer. |
| Verify | Run quality gate (`task check-go`), build production binaries, check handler error discrimination. |
| Report | Commit with conventional commit format, close task in beads, append to PROGRESS.md. |

**Decision rules:** CLAUDE.md authority → requirements → simplest wins → missing dependency protocol → unclear criteria → test patterns → commit format → fix root causes.

### A.2 Loompa — Verifier (Batch-Scoped)

**Instruction file:** `loompa/LOOMPA.md`

**Phases:** Pre-flight → Trace → Verify → Fix → Browser Verify → Report

Operates on a batch of use cases from a specification file. Each iteration verifies one UC against the codebase: traces through handler → service → repository → SQL, checks business rule enforcement, event publishing, error paths, and permissions.

### A.3 Loompa-T — Verifier (Task-Scoped)

**Instruction file:** `loompa-t/LOOMPA-T.md`

**Phases:** Orient → Discover → Verify + Fix → Report

Operates on individual beads tasks. Classifies each UC as PASS (code matches spec), FIX (fixable gap), or SKIP (infrastructure missing). Creates implementation issues in beads for SKIP cases.

### A.4 Charlie — Planner

**Instruction file:** `charlie/CHARLIE.md` — not yet implemented. The planning agent lacks a production instruction file. Its normative behavior is defined in Section 7.4 (decomposition process, BVV-TG-02 through BVV-TG-04, BVV-TG-11). An instruction file for this role MUST implement the following phases at minimum:

| Phase | Key Actions |
|-------|------------|
| Orient | Read plan task from ledger (`bd show $ORCH_TASK_ID`), locate work package path from task description, verify branch existence or create it (BVV-TG-06). |
| Decompose | Parse functional/technical/V&V specs. Identify implementable units. |
| Graph | Create build tasks, V&V tasks, and PR gate task with role tags and dependency edges. Assign priorities (Section 7.4). Reconcile with existing tasks if re-running (BVV-TG-02). |
| Validate | Verify graph well-formedness: acyclicity, single gate, full reachability, valid role tags (BVV-TG-07 through BVV-TG-10). |
| Report | Exit with code 0 (success), 1 (decomposition failed), or 2 (work package unreadable/blocked). |

---

## Appendix B: Mapping to orch/ Package

*(Non-normative)*

This appendix maps specification concepts to the `orch/` Go package (`github.com/endgame/facet-scan/orch`).

| Spec Concept | orch/ Component | Adaptation |
|--------------|-----------------|------------|
| DAG dispatch | `Dispatcher` (`dispatch.go`) | Remove structural task auto-advancement (pipeline/phase task completion logic). `ReadyTasks()` already returns all ready tasks unfiltered — phase ordering is enforced by dependency edges, not query filtering. Add lifecycle scoping via branch label filter. |
| Role routing | `agent.go` + `Preset` | Add role-to-instruction-file mapping. Extend `BuildEnv()` with `ORCH_TASK_ID`. |
| Completion protocol | `agent.go` (`DetermineOutcome`) | Replace file-based output validation with exit-code-based outcome. Stdout diagnostic tags captured in audit trail only. |
| PR gate | `gate.go` | New gate type: PR-based (create PR + poll CI status) alongside the existing agent-based gate. |
| Lifecycle lock | `lock.go` | Reuse as-is. Lock path scoped per branch instead of per pipeline. |
| Beads store | `ledger_beads.go` | Already implemented. Add `blocked` status mapping. Add label-based filtering to `ReadyTasks()`. |
| Worker pool | `session.go` | Unchanged. Workers are reused across tasks within the same lifecycle. |
| Audit trail | `eventlog.go` | Add lifecycle-specific event kinds (Section 10.3). |
| Watchdog | `watchdog.go` | Unchanged. Circuit breaker operates per worker as before. |
| Tmux isolation | `tmux.go` | Unchanged. Socket-isolated sessions with `{runID}-{workerName}` naming. |
| Task graph | `expand.go` | Not used. BVV task graphs are created by the planning agent in beads, not by `Expand()`. |

### B.1 Key Simplification

The BVV model is simpler:

- No `Pipeline`, `Phase`, `ConsensusConfig`, or `QualityGate` types needed
- No `Expand()` function (task graph comes from beads)
- No phase advancement logic in the dispatch loop
- No consensus protocol (instances → merge → verify)
- `DetermineOutcome` reduces to a switch on exit code

The `orch/` package's `Engine`, `Dispatcher`, `WorkerPool`, `Watchdog`, `EventLog`, `PipelineLock`, `TmuxClient`, and `Store` interface are reused unchanged or with minor adaptation.

---

*End of specification.*
