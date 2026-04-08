# Orch: Local Pipeline Orchestrator over Temporal

**Version:** 0.1.0-draft
**Date:** 2026-03-27

---

## Summary

`orch` is a local CLI that runs multi-agent pipelines on a developer's machine. Temporal runs underneath as the durable execution engine. The user never interacts with Temporal directly — `orch` is the interface.

This document maps the [Multi-Agent Pipeline Orchestration Specification](./MULTI_AGENT_PIPELINE_ORCHESTRATION_SPEC.md) to Temporal primitives and defines the local developer experience.

---

## Architecture

```
┌──────────────────────────────────────────────────┐
│                Developer Machine                  │
│                                                   │
│  ┌──────────┐                                     │
│  │ orch CLI │ ← developer types commands here     │
│  └────┬─────┘                                     │
│       │ Temporal SDK (TypeScript)                  │
│       ▼                                           │
│  ┌──────────────────┐                             │
│  │ Temporal Dev      │ ← single process,          │
│  │ Server            │   in-memory or SQLite       │
│  │                   │   started by `orch start`   │
│  └────────┬─────────┘                             │
│           │                                        │
│           ▼                                        │
│  ┌──────────────────┐                              │
│  │ orch worker       │ ← polls Temporal task queue  │
│  │                   │   spawns agent processes     │
│  │ • creates git     │   validates outputs          │
│  │   worktrees       │   manages workspaces         │
│  │ • injects env     │                              │
│  │ • spawns agents   │                              │
│  │ • validates       │                              │
│  │   output          │                              │
│  └──────────────────┘                              │
│           │                                         │
│           ▼                                         │
│  ┌──────────────────┐  ┌──────────────────┐        │
│  │ Agent Process A   │  │ Agent Process B   │  ...  │
│  │ (git worktree)    │  │ (git worktree)    │       │
│  │                   │  │                   │       │
│  │ sees: orch CLI    │  │ sees: orch CLI    │       │
│  │ sees: env vars    │  │ sees: env vars    │       │
│  │ sees: workspace   │  │ sees: workspace   │       │
│  └──────────────────┘  └──────────────────┘        │
└──────────────────────────────────────────────────┘
```

Agents don't know Temporal exists. They see environment variables, the `orch` CLI, and their isolated workspace.

---

## Spec → Temporal Mapping

### Infrastructure Layer (Sections 4–11a)

| Spec Concept | Spec Ref | Temporal Primitive | Notes |
|---|---|---|---|
| **Assignment Ledger** | `[LDG-01]`–`[LDG-20]` | Workflow state + Visibility API | Workflow state IS the ledger. Queries replace ledger reads. |
| **Single durable store** | `[DSN-07]` | Temporal persistence layer | Workflow history is the single source of truth. No side files. |
| **Worker Identity** | `[WKR-01]`–`[WKR-12]` | Worker with named Task Queue | `orch-worker-{name}` task queue. Identity persists across sessions. |
| **Worker lifecycle (IDLE→ACTIVE)** | `[WKR-04]`–`[WKR-06]` | Worker polling loop | Polling = IDLE. Executing activity = ACTIVE. |
| **Worker pool reuse** | `[WKR-07]`–`[WKR-09]` | Task queue routing | Temporal routes to any available worker on the queue. |
| **Task status tracking** | `[LDG-14]`–`[LDG-19]` | Workflow/activity status | `open`→`assigned`→`in_progress`→`completed`/`failed`. Maps to Temporal activity lifecycle. |
| **Task dependencies (DAG)** | `[LDG-03]`–`[LDG-06]` | Workflow code sequencing | `await activity1(); await activity2();` — dependencies expressed as code, not data. |
| **Dispatch loop** | `[OPS-01]` | Temporal's internal task matching | Workers poll task queues. Temporal dispatches. No custom dispatch loop needed. |
| **Priority ordering** | `[LDG-07]` | Task queue priority (available in Temporal) | Lower priority value = dispatched first. |
| **Concurrency control** | `[LDG-10]`–`[LDG-12]` | Temporal's built-in serialization | One activity per worker at a time (configurable). No file locks. |
| **Heartbeat protocol** | `[OBS-01]`–`[OBS-02]` | Activity heartbeat | `context.heartbeat(data)`. Temporal detects stale heartbeats automatically. |
| **Watchdog** | `[SUP-01]`–`[SUP-04]` | Start-to-close timeout on activities | Activity exceeds timeout → Temporal cancels and retries. Deterministic, no judgment. |
| **Circuit breaker** | `[SUP-05]`–`[SUP-06]` | Retry policy `maximumAttempts` | After N failures, stop retrying. Workflow code creates escalation. |
| **Escalation** | `[SUP-07]`–`[SUP-08]` | Child workflow with type=escalation | `startChildWorkflow(escalationWorkflow, { message })`. Standard task. |
| **Workspace isolation** | `[WSP-01]`–`[WSP-03]` | Git worktrees managed by worker | Worker creates worktree before activity, cleans up after. Temporal doesn't manage filesystems. |
| **Workspace merge** | `[MRG-01]`–`[MRG-08]` | Activity: git rebase + fast-forward | `workspaceMerge` activity acquires git lock, rebases, merges. Returns exit code. |
| **Merge lock** | `[MRG-04]`–`[MRG-06]` | Workflow-level mutex (or Temporal mutex pattern) | Serialize merge operations via a dedicated merge workflow/signal. |
| **Session continuity** | `[CTY-01]`–`[CTY-09]` | Continue-As-New | When context budget low, agent signals `orch done --handoff`. Workflow continues-as-new with carried state. |
| **Handoff schema** | `[CTY-03]` | Continue-As-New input payload | Handoff struct passed as workflow input on continuation. |
| **Crash recovery** | `[RCV-01]`–`[RCV-10]` | Temporal replay | Workflow replays from history on restart. Zero data loss. No recovery code needed. |
| **Orphan detection** | `[RCV-05]`–`[RCV-06]` | Activity cancellation scope | Temporal tracks all activities. No orphans possible — activities either complete, fail, or timeout. |
| **Graceful degradation** | `[RCV-07]`–`[RCV-10]` | Temporal server resilience | If Temporal dev server restarts, workflow history persists (SQLite mode). Workers reconnect automatically. |

### Pipeline Execution Layer (Sections 12–21)

| Spec Concept | Spec Ref | Temporal Primitive | Notes |
|---|---|---|---|
| **Pipeline** | Section 12.1 | Root Workflow | `pipelineWorkflow(definition)` — one workflow per pipeline run. |
| **Phase** | Section 12.2 | Child Workflow | `startChildWorkflow(phaseWorkflow, { topology, agents, gate })` per phase. |
| **Agent invocation** | Section 12.3 | Activity | `executeActivity(invokeAgent, { id, model, inputs, output })`. |
| **Sequential topology** | `[OPS-04]` | Sequential activity execution | `for (const agent of agents) { await invokeAgent(agent); }` |
| **Parallel topology** | `[OPS-04]` | Parallel activity execution | `await Promise.all(agents.map(a => invokeAgent(a)))` |
| **Consensus topology** | `[OPS-04]` | Child workflow: instances → merge → verify | See Consensus section below. |
| **Pipeline expansion** | `[EXP-01]`–`[EXP-14]` | Workflow code | Expansion happens in workflow code. No separate expansion step — the workflow IS the expanded graph. |
| **Phase ordering (DAG)** | `[EXP-04]`–`[EXP-05]` | Sequential child workflow awaits | `await phase1; await phase2; await phase3;` — ordering is code. |
| **Quality gate** | `[OPS-08]` | Activity + conditional signal wait | Gate activity runs. If `halt=true` and failed → `await condition(() => gateOverridden)`. Human sends signal. |
| **Session boundary (hard)** | `[OPS-09]` | Signal wait | After phase completes → `await workflow.condition(() => resumeSignaled)`. User runs `orch resume`. |
| **Session boundary (soft)** | `[OPS-09]` | Context budget check | Workflow queries worker for context budget. If sufficient, continue. If not, pause like hard boundary. |
| **Session boundary (none)** | `[OPS-09]` | No-op | Workflow proceeds to next phase immediately. |
| **Resume** | `[OPS-06]`–`[OPS-07]` | Temporal replay | No resume algorithm needed. Temporal replays workflow history. Completed activities skip. Pipeline continues from where it left off. |
| **Input validation** | `[OPS-03]` | Activity precondition | `invokeAgent` activity checks inputs exist before spawning agent. |
| **Output validation** | `[OPS-04]` | Activity postcondition | `invokeAgent` activity validates output exists, meets size/format thresholds. |
| **Gap accumulation** | `[ERR-06]`–`[ERR-07]` | Workflow state counter | `gaps.push(gap); if (gaps.length >= gapTolerance) throw GapToleranceExceeded;` |
| **Retry protocol** | `[ERR-01]`–`[ERR-03]` | Temporal Retry Policy | `{ maximumAttempts: 3, backoffCoefficient: 1.5 }` on agent activities. |
| **Context budget** | `[OPS-08]`–`[OPS-09]` | Activity heartbeat data | Agent heartbeats include remaining context. Worker reads it. |
| **Audit trail** | `[OPS-12]`–`[OPS-14]` | Workflow history + custom search attributes | Every activity start/complete is in Temporal history. Custom attributes for phase, agent, consensus rate. |
| **Finalization** | `[OPS-10]`–`[OPS-11]` | Final activities in root workflow | Writing quality pass, audit archive, cleanup — sequential activities at workflow end. |

### Consensus Protocol (Section 16)

| Spec Concept | Spec Ref | Temporal Implementation |
|---|---|---|
| **Consensus round** | `[CON-01]`–`[CON-10]` | Child workflow: `consensusRoundWorkflow(agent, config)` |
| **Instance sub-phase** | Section 16.6 | N parallel activities: `Promise.all(suffixes.map(s => invokeInstance(agent, s)))` |
| **Degraded mode (N−1)** | Section 16.6 | Workflow logic: count successes. If N−1, proceed with survivors. If < N−1, retry or gap. |
| **Content merge sub-phase** | Section 16.6 | Activity: `invokeAgent(mergeAgent, { instanceOutputs })` |
| **Consensus rate computation** | `[CON-04]`–`[CON-05]` | Merge activity returns rate + unique count in result. Workflow stores in state. |
| **Verification sub-phase** | Section 16.6 | Conditional activity: `if (uniqueCount > 0 && lowThreshold <= rate < highThreshold) invokeAgent(verifyAgent)` |
| **Verification skip (high consensus)** | Section 16.6 | `if (rate >= highThreshold) skip` |
| **Disputed (low consensus)** | Section 16.6 | `if (rate < lowThreshold) await humanAcknowledgment()` — signal wait. |
| **Batched consensus** | `[CON-06]`–`[CON-07]` | Batch loop: `for (const batch of batches) { await Promise.all(batch.map(consensusRound)); }` |
| **Verification tags** | `[CON-08]` | Merge agent output. Tags are content-level, not workflow-level. |
| **Finding similarity** | Section 16.1 | Merge agent logic. Temporal doesn't participate — this is agent reasoning. |

### Safety Properties (Section 18)

| Property | Spec Ref | Temporal Guarantee |
|---|---|---|
| **S1: Monotonic progress** | Section 18.1 | Temporal workflow history is append-only. Completed activities never re-execute on replay. |
| **S2: Instance independence** | Section 18.2 | Parallel activities get separate worktrees. No shared state. |
| **S3: Merge determinism** | Section 18.3 | Merge agent logic (anchor ordering). Not a Temporal concern. |
| **S4: No silent capability loss** | Section 18.4 | Preservation gate = activity + `halt=true` signal wait. |
| **S5: Bounded degradation** | Section 18.5 | Workflow state tracks gap count. Exceeding tolerance throws. |
| **S6: Gate authority** | Section 18.6 | Halting gate = signal wait. Only `orch override` sends the signal. |
| **S7: Pipeline exclusion** | Section 18.7 | Workflow ID = pipeline ID + repo. Temporal enforces: one workflow per ID. |
| **S8: Cleanup completeness** | Section 18.8 | Finalization activities clean up worktrees and transient files. |
| **S9: Assignment durability** | Section 18.9 | Temporal replay guarantees. Activity state survives any crash. |
| **S10: Workspace isolation** | Section 18.10 | Worker creates worktrees. One worktree per activity. OS-level isolation. |
| **S11: Watchdog-retry non-interference** | Section 18.11 | Temporal separates activity timeouts (process-level) from retry policies (task-level). |

### Liveness Properties (Section 19)

| Property | Spec Ref | Temporal Guarantee |
|---|---|---|
| **L1: Eventual termination** | Section 19.1 | Finite retry policy + finite phases. Workflow code terminates. |
| **L2: Consensus convergence** | Section 19.2 | Bounded by merge agent algorithm. Not a Temporal concern. |
| **L3: Lock release** | Section 19.3 | Temporal activity timeouts guarantee release. No stale locks. |

### Design Principles (Section 4)

| Principle | Spec Ref | Temporal Enforcement |
|---|---|---|
| **Orchestrator MUST NOT reason** | `[DSN-01]` | Temporal workflow code is deterministic. No I/O, no LLM calls, no randomness. Enforced by Temporal SDK. |
| **Orchestrator MUST NOT parse agent output** | `[DSN-03]` | Workflow code receives structured activity results (success/failure + metadata). Never reads file content. |
| **Single durable store** | `[DSN-07]` | Workflow history IS the store. No side files, no separate databases. |
| **No abstraction without duplication** | `[DSN-08]` | Phases are child workflows. Consensus rounds are child workflows. Escalations are child workflows. One abstraction. |

---

## CLI Commands

### Lifecycle

| Command | What It Does |
|---|---|
| `orch start` | Start Temporal dev server + worker process in background. |
| `orch stop` | Stop worker + dev server. |
| `orch status` | Show running pipelines, active agents, worker state. |

### Pipeline Operations

| Command | What It Does |
|---|---|
| `orch run <pipeline> --repo <path>` | Start a pipeline. Returns run ID. |
| `orch resume <run-id>` | Send resume signal to a paused pipeline (hard boundary or halting gate). |
| `orch override <run-id> <gate>` | Override a failed halting gate. Sends signal to unblock. |
| `orch cancel <run-id>` | Cancel a running pipeline. |
| `orch list` | List all pipeline runs (active and completed). |
| `orch logs <run-id>` | Show execution history for a pipeline run. |

### Agent Commands (called from inside agent workspace)

| Command | What It Does | Temporal SDK Call |
|---|---|---|
| `orch prime` | Get current assignment + handoff context. | Query: `getAssignment` |
| `orch done` | Signal task completion. Triggers workspace merge. | Signal: `activityComplete` |
| `orch done --handoff` | Signal session handoff. Task continues, session ends. | Signal: `handoff` |
| `orch fail [--reason <msg>]` | Signal task failure. | Signal: `activityFailed` |
| `orch heartbeat` | Write heartbeat with current state. | Activity heartbeat: `context.heartbeat(state)` |
| `orch escalate <message>` | Create escalation task. | Signal: `escalate` |
| `orch task create <body>` | Create a new task in the ledger. | Signal: `createTask` |
| `orch version` | Print protocol version. | Local — no Temporal call. |

### Observability

| Command | What It Does |
|---|---|
| `orch dashboard` | Live terminal UI: pipelines, phases, agents, consensus rates. |
| `orch inspect <run-id>` | Detailed view of a single pipeline run's task graph. |
| `orch gaps <run-id>` | Show accumulated gaps for a pipeline run. |
| `orch audit <run-id>` | Show audit trail events. |

---

## What Agents See

Agents are unaware of Temporal. They interact with the orchestrator through two mechanisms:

### 1. Environment Variables (injected at spawn)

Per `[ITF-01]`:

```bash
ORCH_WORKER_NAME=worker-alpha
ORCH_PROJECT=facet-scan
ORCH_ROLE=facet-scan/worker-alpha
ORCH_WORKSPACE=/tmp/orch/workspaces/worker-alpha
ORCH_BRANCH=orch/worker-alpha/facet-scan-20260327
ORCH_RUN_ID=facet-scan-20260327-abc123
```

### 2. The `orch` CLI

```bash
# Agent starts, discovers its assignment
eval "$(orch prime --json)"
# Returns: task_id, instructions, handoff context (if resuming)

# Agent does work...

# Agent completes successfully
orch done

# Or: agent's context window is filling up
orch done --handoff

# Or: agent fails
orch fail --reason "output validation failed: missing FUNCTIONAL.md header"

# Periodic heartbeat (optional, for long-running agents)
orch heartbeat --state working
```

---

## What This Replaces

Current Atelier pipelines self-manage state through JSON files in the output directory. With `orch`, Temporal manages all state.

| Current State File | Purpose | Replaced By |
|---|---|---|
| `.pipeline-state.json` | Phase index, degradation flags | Temporal workflow state |
| `.gaps.json` | Failed agents across sessions | Temporal workflow state (`gaps` array) |
| `.gaps.lock` | Race condition prevention | Temporal serialization (no concurrent access) |
| `.in-progress.lock` | Concurrent execution prevention | Temporal workflow ID uniqueness (`[S7]`) |
| `.consensus-state.json` | Instance/merge/verify status per round | Consensus child workflow state |
| `.consensus-state.lock` | Consensus state update lock | Temporal serialization |
| `.retry-history.json` | Agent retry attempts | Temporal retry policy history |
| `.audit-trail.jsonl` | Active run audit events | Temporal workflow history + search attributes |
| Crash markers (in-progress files) | Detect agent crash mid-analysis | Activity timeout detection |

**Eliminated entirely:** All lock files, all JSON state files, all crash markers. Temporal's workflow history replaces them.

**Retained:** Agent output files (the actual deliverables — markdown views, JSONL inventories, YAML findings). These are permanent artifacts written to the output directory. Consensus instance files (`.consensus/`) remain as workspace artifacts during the run.

---

## Workflow Structure

### Root: Pipeline Workflow

```
pipelineWorkflow(definition, prerequisites)
  │
  ├── validate prerequisites exist
  ├── validate well-formedness constraints [WFC-01]–[WFC-12]
  │
  ├── for each phase in definition.phases:
  │     │
  │     ├── startChildWorkflow(phaseWorkflow, phase)
  │     │     │
  │     │     ├── [sequential] → agents in order, each awaits previous
  │     │     ├── [parallel]   → all agents concurrently
  │     │     └── [consensus]  → batched consensus rounds
  │     │
  │     ├── evaluate quality gate (if present)
  │     │     ├── pass → continue
  │     │     ├── pass_warn → record warning, continue
  │     │     ├── fail + halt=false → record warning, continue
  │     │     └── fail + halt=true → await signal('override')
  │     │
  │     └── check session boundary
  │           ├── hard → await signal('resume')
  │           ├── soft + budget low → await signal('resume')
  │           ├── soft + budget ok → continue
  │           └── none → continue
  │
  ├── finalization:
  │     ├── writing quality pass (best-effort activity)
  │     ├── archive audit trail (best-effort activity)
  │     └── cleanup transient artifacts (best-effort activity)
  │
  └── return summary
```

### Consensus Round Workflow

```
consensusRoundWorkflow(agent, config)
  │
  ├── Sub-phase 1: Instances
  │     ├── launch N activities in parallel (one per suffix: A, B, C)
  │     ├── each activity: create worktree → spawn agent → validate output
  │     ├── count successes
  │     │     ├── all N → mode = normal
  │     │     ├── N−1  → mode = degraded
  │     │     ├── < N−1 + critical → retry failed, re-evaluate
  │     │     └── < N−1 + non-critical → mode = failed, record gap, return
  │     │
  ├── Sub-phase 2: Content Merge
  │     ├── invoke merge agent with successful instance outputs
  │     ├── read consensus rate + unique count from result
  │     │
  └── Sub-phase 3: Verification (conditional)
        ├── rate ≥ high threshold → skip (high consensus)
        ├── unique count = 0 → skip (nothing to verify)
        ├── rate < low threshold → mark all disputed, await human signal
        └── otherwise → invoke verify agent on unique findings
```

### Activity: Invoke Agent

```
invokeAgent(agentDef, inputs, outputPath)
  │
  ├── validate all inputs exist and are non-empty [OPS-03]
  ├── create git worktree for agent workspace
  ├── inject environment variables [ITF-01]
  ├── write instruction file [ITF-07]
  ├── spawn agent process (claude --agent <id>)
  ├── wait for process exit or orch signal (done/fail/handoff)
  ├── validate output [OPS-04]:
  │     ├── file exists
  │     ├── size ≥ 100 bytes
  │     └── format-specific check (md header, JSON parse, YAML parse)
  ├── if valid → return success + metadata
  ├── if invalid + critical → throw (triggers retry policy)
  └── if invalid + non-critical → return failure (workflow records gap)
```

---

## Local Mode Specifics

### Temporal Dev Server

`orch start` launches `temporal server start-dev` with:

- In-memory storage (default) or SQLite for persistence across restarts
- Single namespace: `orch-local`
- UI available at `localhost:8233` (Temporal's built-in web UI — free observability)

### Worker Process

`orch start` also launches a worker process that:

- Registers all activity types (invokeAgent, workspaceMerge, gateEvaluate, etc.)
- Polls the `orch-local` task queue
- Manages git worktrees in a temporary directory (`/tmp/orch/workspaces/`)
- Limits concurrency based on machine resources (default: 3 concurrent activities)

### Concurrency

| Setting | Default | Rationale |
|---|---|---|
| Max concurrent activities | 3 | Local machine resource limit |
| Consensus instance parallelism | 3 | N=3 instances run simultaneously |
| Batch parallelism | 1 | Batches serialize locally (machine can't handle 9+ agents) |

Configurable via `orch start --concurrency <n>`.

### Persistence Across Restarts

With `orch start --persist`:

- Temporal dev server uses SQLite instead of in-memory
- `orch stop` + `orch start` resumes all paused/active workflows
- No state loss on machine restart

Without `--persist`:

- In-memory. `orch stop` loses all workflow state.
- Agent output files remain on disk. Pipelines can be re-run.

---

## Migration Path

### From Current Atelier Self-Checkpointing

Current Atelier pipelines manage their own state (`.pipeline-state.json`, etc.) and are invoked by the skill/command orchestration layer (slash commands dispatching agents via the Task tool).

Migration to `orch`:

| Phase | What Changes |
|---|---|
| **Phase 1** | `orch` wraps existing pipeline invocations. `orch run facet-scan` calls `claude --agent 00-facet-scan-scout`, etc. State files still used as fallback. |
| **Phase 2** | Pipeline orchestration logic moves from skill SKILL.md files into Temporal workflows. Agents still run the same way, but sequencing/consensus/gating is in workflow code. State files eliminated. |
| **Phase 3** | Agent-to-orchestrator communication via `orch` CLI. Agents call `orch done` instead of writing state markers. Full spec conformance. |

### To Production (Future)

Same Temporal workflows, different deployment:

| Aspect | Local (`orch start`) | Production |
|---|---|---|
| Temporal server | `temporal server start-dev` | Temporal Cloud or self-hosted cluster |
| Worker | Single local process | Container fleet with tag-based routing |
| Workspaces | `/tmp/orch/workspaces/` | Per-container volumes |
| Persistence | SQLite or in-memory | PostgreSQL + Elasticsearch |
| Concurrency | 3 activities | Scaled to cluster capacity |
| Connection | `localhost:7233` | Cluster endpoint or Temporal Cloud |

`orch connect <endpoint>` switches from local to remote. Same CLI, same workflows, different backend.

---

## References

| Document | Relationship |
|---|---|
| [MULTI_AGENT_PIPELINE_ORCHESTRATION_SPEC.md](./MULTI_AGENT_PIPELINE_ORCHESTRATION_SPEC.md) | The formal spec this design implements |
| [MULTI_AGENT_PIPELINE_EXECUTION_SPEC.md](./MULTI_AGENT_PIPELINE_EXECUTION_SPEC.md) | Pipeline execution subset (Sections 12–21 of orchestration spec) |
| [DARK_FACTORY_VISION.md](../vision/DARK_FACTORY_VISION.md) | The broader factory vision `orch` serves |
| [TECHNICAL_SOLUTION.md](../vision/TECHNICAL_SOLUTION.md) | Technology choices; `orch` replaces Windmill for local pipeline execution |
| [Temporal TypeScript SDK](https://docs.temporal.io/develop/typescript) | Implementation SDK |
| [Temporal Dev Server](https://docs.temporal.io/cli#start-dev) | Local development server |
