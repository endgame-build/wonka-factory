# Multi-Agent Pipeline Execution Specification

**Version:** 1.0.0-draft
**Status:** Working Draft
**Editors:** ENDGAME
**Date:** 2026-03-27

---

## Abstract

This specification defines the execution model for multi-agent analysis pipelines. It covers the type system for pipeline definitions, well-formedness constraints, operational semantics for phase dispatch and consensus protocols, checkpoint and recovery mechanisms, error handling, and safety and liveness properties. The specification is implementation-agnostic: it defines what a conforming orchestrator must do, not how.

## Status of This Document

This document is a Working Draft.

## Notices

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

Non-normative text is marked with *(Non-normative)* or enclosed in a note block.

---

## 1 Introduction

### 1.1 Purpose

This specification defines the execution model for pipelines that compose autonomous AI agents into phased analysis workflows. It addresses:

- How agents are organized into phases with defined execution topologies
- How multiple independent agent instances reach consensus on analysis findings
- How pipelines checkpoint progress and resume across session boundaries
- What safety and liveness properties a conforming orchestrator must guarantee

### 1.2 Scope

This specification covers:

- Pipeline, phase, and agent definition schemas
- Well-formedness constraints on pipeline definitions
- Operational semantics: dispatch, gate evaluation, session boundaries, resume
- Consensus protocol: instance independence, finding classification, verification tags
- Checkpoint, recovery, and crash detection
- Error semantics: retry, gap accumulation, criticality
- Safety and liveness properties

This specification does not cover:

- Worker identity, lifecycle, or workspace isolation (see Multi-Agent Pipeline Orchestration Specification)
- Internal behavior of any agent implementation
- Domain-specific analysis semantics

### 1.3 Normative References

- RFC 2119: Key words for use in RFCs to Indicate Requirement Levels

### 1.4 Typographical Conventions

Normative requirements carry identifiers in brackets (e.g., `[TYP-01]`).

---

## 2 Terminology and Definitions

| Term | Definition |
|------|-----------|
| **Agent** | An autonomous reasoning process that interprets instructions and produces artifacts. Opaque to the orchestrator. |
| **Anchor Instance** | The instance whose findings define the iteration order in greedy bipartite matching. Determined by a fixed suffix ordering. |
| **Batch** | A partition of consensus rounds within a phase, processed sequentially. Rounds within a batch execute concurrently. |
| **Consensus Round** | A sequence of sub-phases (instance, merge, verify) that produces a validated output from multiple independent agent invocations. |
| **Content Merge** | An agent task that combines findings from multiple consensus instances into a single output using greedy bipartite matching. |
| **Finding** | A semantic unit of analysis output: a table row, diagram node, list item, or equivalent. The atomic element of consensus classification. |
| **Finding Group** | A set of matchable findings from different instances, classified as unanimous, majority, or unique. |
| **Gap** | A recorded non-critical agent failure that the pipeline tolerates. |
| **Phase** | An ordered step within a pipeline, containing agent invocations arranged by topology. |
| **Pipeline** | An ordered sequence of phases that transforms input artifacts into validated outputs. |
| **Quality Gate** | A phase-level checkpoint that evaluates an agent's output and may halt pipeline progression. |
| **Session** | A single execution of the orchestrator. Has a finite context window. A pipeline may span multiple sessions. |
| **Topology** | The execution arrangement of agents within a phase: sequential, parallel, or consensus. |
| **Verification Tag** | An annotation on a consensus finding that tracks its provenance through the verification sub-phase. |

---

## 3 Conformance

### 3.1 Conformance Targets

This specification defines requirements for one conformance target: an **orchestrator** — the process that executes pipelines by dispatching agents, managing state, and enforcing properties.

### 3.2 Conformance Levels

**Level 1 — Core.** An orchestrator conforms at Level 1 if it satisfies all MUST requirements in Sections 4 through 9. This level covers pipeline definition, operational semantics, consensus, checkpoint/recovery, and error handling.

**Level 2 — Full.** An orchestrator conforms at Level 2 if it satisfies Level 1 and all MUST requirements in Sections 10 through 11. This level adds safety and liveness property guarantees.

### 3.3 Orchestrator Modes

*(Non-normative)* This specification defines an abstract execution model. Conforming implementations may realize it through different mechanisms:

| Aspect | Variation |
|--------|-----------|
| Agent launch | Subagent tools, CLI subprocess spawning, container orchestration |
| Parallelism | Built-in concurrency primitives, OS-level process parallelism, distributed execution |
| State persistence | JSON files, databases, distributed state stores |
| Session boundaries | Interactive user re-invocation, continuous unattended execution |
| Error UX | Interactive prompts, automated retry, log-and-continue |

All modes MUST satisfy the same safety and liveness properties. The modes differ only in how they realize the abstract operations described in this specification.

---

## 4 Type System

### 4.1 Primitive Types

| Type | Definition |
|------|-----------|
| `Path` | A nonempty string representing a filesystem path. |
| `Duration` | A positive integer in milliseconds. |
| `Rate` | A real number in the range [0, 100]. |

### 4.2 Enumerations

| Enumeration | Values |
|-------------|--------|
| `Model` | `opus`, `sonnet`, `haiku` |
| `Criticality` | `critical`, `non_critical` |
| `Topology` | `sequential`, `parallel`, `consensus` |
| `Boundary` | `hard`, `soft`, `none` |
| `Mode` | `normal`, `degraded`, `failed` |
| `Status` | `success`, `failed`, `skipped`, `disputed`, `pending`, `overridden` |
| `Verdict` | `confirmed`, `rejected`, `uncertain` |
| `GateResult` | `pass`, `pass_warn`, `fail` |
| `Format` | `md`, `jsonl`, `json`, `yaml` |
| `Tag` | `unverified`, `verified_unique`, `disputed`, `verification_timeout` |
| `ArtifactClass` | `transient`, `permanent` |

### 4.3 Agent

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier within the pipeline. |
| `model` | Model | Model tier for this agent. |
| `tools` | string set | Tool names available to the agent. |
| `skills` | string set | Skill names injected into the agent's context. |
| `inputs` | Path[] | Artifacts this agent reads. |
| `output` | Path | The single artifact this agent writes. |
| `criticality` | Criticality | Whether failure blocks the pipeline. |
| `format` | Format | Expected output format. |
| `id_prefix` | string? | Namespace prefix for identifiers this agent creates. |
| `max_turns` | integer? | Context budget limit for this agent's invocation. |

### 4.4 Consensus Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `instance_count` | integer | 3 | Number of independent instances per agent. |
| `batch_size` | integer | 3 | Maximum concurrent consensus rounds within a phase. |
| `merge_agent` | string | — | Agent identifier for the content merge sub-phase. |
| `verify_agent` | string | — | Agent identifier for the verification sub-phase. |
| `thresholds.high` | Rate | 85.0 | Above this rate, verification is skipped. |
| `thresholds.low` | Rate | 30.0 | Below this rate, all findings are marked disputed. |
| `thresholds.similarity` | real in (0, 1] | 0.80 | Minimum similarity for finding matching. |

### 4.5 Quality Gate

| Field | Type | Description |
|-------|------|-------------|
| `agent` | string | The agent whose output is evaluated. |
| `halt` | boolean | If true, a failing gate halts the pipeline until human override. |

### 4.6 Phase

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier within the pipeline. |
| `topology` | Topology | Execution arrangement of agents. |
| `boundary` | Boundary | Session boundary behavior after this phase. |
| `agents` | Agent[] | Ordered list of agent definitions. |
| `gate` | QualityGate? | Optional quality gate. |
| `consensus` | ConsensusConfig? | Required iff `topology = consensus`. |

### 4.7 Pipeline

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier. |
| `output_dir` | Path | Root directory for all pipeline outputs. |
| `prerequisites` | Path[] | Input artifacts that must exist before execution. |
| `gap_tolerance` | integer | Maximum non-critical failures before forced abort. |
| `phases` | Phase[] | Ordered sequence of phases. |
| `lock` | LockConfig | Pipeline-level concurrency protection. |

### 4.8 Lock Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | Path | — | Lock file location. MUST reside within `output_dir`. RECOMMENDED: a dotfile in the output directory root. |
| `staleness_threshold` | Duration | 14400000 (4 hours) | Age after which a lock is considered stale and reclaimable. |
| `retry_count` | integer | 3 | Attempts to acquire the lock before reporting contention. |
| `retry_delay` | Duration | 100 | Delay between acquisition attempts in milliseconds. |

### 4.9 Runtime State

**Pipeline State:**

| Field | Type | Description |
|-------|------|-------------|
| `run_id` | string | Unique identifier for this execution run. |
| `phase` | integer | Index into `pipeline.phases` of the current phase. |
| `rounds` | map: string → RoundState | Per-agent consensus round state. |
| `gaps` | Gap[] | Accumulated non-critical failures. |
| `warnings` | Warning[] | Non-fatal observations. |
| `audit` | AuditEvent[] | Structured execution events. |
| `updated_at` | timestamp | Last state modification time. |

**Round State:**

| Field | Type | Description |
|-------|------|-------------|
| `instances` | map: string → Status | Per-instance status (keyed by suffix). |
| `merge` | Status | Content merge sub-phase status. |
| `verify` | Status | Verification sub-phase status. |
| `consensus_rate` | Rate? | Computed after merge. |
| `unique_count` | integer? | Count of unique finding groups. |
| `mode` | Mode | `normal`, `degraded`, or `failed`. |

**Gap:**

| Field | Type | Description |
|-------|------|-------------|
| `agent` | string | The failed agent's identifier. |
| `phase` | string | The phase in which failure occurred. |
| `error` | string | Description of the failure. |
| `impact` | string | What downstream analysis is affected. |

**Warning:**

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | The phase that generated the warning. |
| `agent` | string? | The agent, if applicable. |
| `message` | string | Warning description. |

### 4.10 Audit Event

| Field | Type | Description |
|-------|------|-------------|
| `timestamp` | timestamp | When the event occurred. |
| `event_type` | AuditEventType | Category of event. |
| `agent` | string? | Agent involved, if applicable. |
| `phase` | string | Phase in which the event occurred. |
| `summary` | string | One-line description. |
| `detail` | string? | Extended information. |

**Audit Event Types:**

| Value | Meaning |
|-------|---------|
| `decision` | An architectural or classification decision. |
| `phase_start` | A phase begins execution. |
| `phase_complete` | A phase finishes. |
| `agent_start` | An agent is invoked. |
| `agent_complete` | An agent finished (success or failure). |
| `consensus_merge` | A content merge completed with consensus rate. |
| `consensus_verify` | A verification completed with verdicts. |
| `gate_result` | A quality gate was evaluated. |
| `gap_recorded` | A non-critical failure was accepted. |
| `pipeline_complete` | Terminal state reached. |

### 4.11 Verification Tags

| Tag | Meaning |
|-----|---------|
| `unverified` | Assigned at merge to all unique findings. |
| `verified_unique` | Verifier confirmed the finding. |
| (removed) | Verifier rejected the finding — deleted from output. |
| (unchanged) | Verifier returned uncertain — finding remains `unverified`. |
| `disputed` | Consensus rate below low threshold — immutable until human review. |
| `verification_timeout` | Verifier did not complete. |

**Tag transitions:**

| From | To | Trigger |
|------|----|---------|
| `unverified` | `verified_unique` | Verifier confirmed |
| `unverified` | (finding removed) | Verifier rejected |
| `unverified` | `unverified` | Verifier uncertain — no change |
| `unverified` | `verification_timeout` | Verifier did not complete |
| `disputed` | `disputed` | Immutable until human review |

### 4.12 Artifact Lifecycle

`[TYP-01]` Pipeline artifacts are classified as `transient` (deleted on successful completion) or `permanent` (retained indefinitely).

`[TYP-02]` An artifact is transient if its sole purpose is pipeline coordination or intermediate computation — it is not part of the pipeline's deliverable output. Examples: consensus instance files, state tracking files, lock files, active audit trails, retry history.

`[TYP-03]` An artifact is permanent if it is a pipeline deliverable or an archived record of execution. Examples: final agent outputs, consensus reports, archived audit trails, execution summaries.

---

## 5 Well-Formedness Constraints

A pipeline is well-formed iff all of the following hold. The orchestrator MUST verify these before execution.

`[WFC-01]` **Unique agent outputs.** Two agents in a pipeline MUST NOT write to the same output path.

`[WFC-02]` **Phase ordering forms a DAG.** Every agent input MUST reference only pipeline prerequisites or outputs from agents in earlier phases. No agent may depend on an output from the same phase or a later phase.

`[WFC-03]` **Consensus requires configuration.** Every phase with `topology = consensus` MUST have a non-absent `consensus` field.

`[WFC-04]` **Singleton forbids configuration.** Every phase with `topology` other than `consensus` MUST have an absent `consensus` field.

`[WFC-05]` **Threshold ordering.** For every consensus configuration: the low threshold MUST be less than the high threshold, both within [0, 100], and the similarity threshold MUST be in the range (0, 1].

`[WFC-06]` **Instance count bounds.** For every consensus configuration: `instance_count` MUST be at least 2.

`[WFC-07]` **Gate agent membership.** A gate's agent MUST belong to the same phase or an earlier phase in the pipeline.

`[WFC-08]` **Critical agent completeness.** An agent MUST be classified as `critical` if and only if: (a) its output is the sole input dependency for another agent (no alternative source exists), or (b) it is a gate agent.

`[WFC-09]` **Unique identifier prefixes.** Two agents with non-absent `id_prefix` values MUST NOT share the same prefix.

`[WFC-10]` **Gap tolerance bounds.** The `gap_tolerance` MUST be at least 1 and at most the count of non-critical agents in the pipeline.

`[WFC-11]` **Lock path containment.** The pipeline lock file MUST reside within the output directory.

`[WFC-12]` **Consensus lock threshold ordering.** Any finer-grained lock used for consensus state updates MUST have a staleness threshold strictly less than the pipeline lock staleness threshold. RECOMMENDED: 15 minutes for consensus state locks vs. 4 hours for the pipeline lock.

---

## 6 Operational Semantics

### 6.1 Pipeline Execution

`[OPS-01]` Pipeline execution is a sequence of steps, each transforming the pipeline state. The orchestrator MUST initialize state with a generated run ID, phase index 0, empty rounds/gaps/warnings/audit collections, and a current timestamp.

`[OPS-02]` The pipeline reaches a terminal state when either: (a) all phases have been executed (phase index equals the number of phases), or (b) the accumulated gap count meets or exceeds the gap tolerance.

`[OPS-03]` The orchestrator MUST execute the following loop: acquire the pipeline lock; load persisted state or initialize fresh state; for each phase in order, execute the phase and advance the phase index; on termination, run finalization; release the pipeline lock.

### 6.2 Phase Execution

`[OPS-04]` Phase execution MUST dispatch based on topology:

**Sequential topology:** The orchestrator MUST invoke each agent in declaration order, one at a time. Each agent's output is available to the next. After all agents complete, the orchestrator evaluates the quality gate (if present) and advances the phase index.

**Parallel topology:** The orchestrator MUST invoke all agents in the phase concurrently with no inter-agent dependencies. After all agents complete (collecting gaps and warnings from all results), the orchestrator evaluates the quality gate and advances the phase index.

**Consensus topology:** The orchestrator MUST execute the consensus protocol (Section 7) for each agent in the phase, subject to batching (Section 7.7). After all consensus rounds complete, the orchestrator evaluates the quality gate and advances the phase index.

### 6.3 Agent Execution

`[OPS-05]` Before invoking an agent, the orchestrator MUST validate that all declared inputs exist and are non-empty.

`[OPS-06]` After an agent completes, the orchestrator MUST validate the output. The output MUST exist and MUST NOT be empty. The orchestrator SHOULD apply a minimum size threshold (RECOMMENDED: 100 bytes) and format-specific validation appropriate to the declared format. RECOMMENDED format checks:

| Format | Validation |
|--------|-----------|
| `md` | Has a recognizable document header. |
| `jsonl` | First line is parseable, last line indicates completion. |
| `json` | Content parses as valid JSON. |
| `yaml` | Content parses as valid YAML. |

> *Note (non-normative):* Implementations MAY support additional output formats beyond those listed above.

`[OPS-07]` If the output is valid, the agent is considered successful. If the output is invalid and the agent is critical, the orchestrator MUST invoke the retry protocol (Section 9.1). If the output is invalid and the agent is non-critical, the orchestrator MUST record a gap.

### 6.4 Gate Evaluation

`[OPS-08]` After all agents in a phase complete, the orchestrator MUST evaluate the quality gate if one is present:

| Gate Present | Result | Halt | Outcome |
|-------------|--------|------|---------|
| No | — | — | Phase proceeds. |
| Yes | `pass` | — | Phase proceeds. |
| Yes | `pass_warn` | — | Warning recorded, phase proceeds. |
| Yes | `fail` | `true` | Pipeline halted. Requires human override (`overridden` status) to advance. |
| Yes | `fail` | `false` | Warning recorded, phase proceeds. |

### 6.5 Session Boundaries

`[OPS-09]` After phase execution, the orchestrator MUST check the boundary type:

| Boundary | Condition | Outcome |
|----------|-----------|---------|
| `hard` | — | Persist state. Yield control. User must re-invoke to continue. |
| `soft` | Context budget insufficient | Persist state. Yield control. |
| `soft` | Context budget sufficient | Proceed to next phase in same session. |
| `none` | — | Proceed to next phase in same session. |

### 6.6 Resume

`[OPS-10]` On re-invocation, the orchestrator MUST reconstruct state from persisted checkpoints. The orchestrator scans phases in reverse order. A phase is complete when:

- For consensus topology: all agents have a verify status of `success`, `skipped`, or `disputed`.
- For other topologies: all agents have a status of `success`.
- If the phase has a halting gate: the gate agent's status is `success` or `overridden`.

The first phase that is not complete is the resume point. If all phases are complete, the pipeline is already terminal.

`[OPS-11]` The orchestrator MUST reconcile persisted state against filesystem state on resume:

- If an agent's status is not `success` but a valid output file exists at the agent's output path, the orchestrator MUST correct the status to `success` (the agent completed but the status write was lost to a crash).
- If an agent's output path contains a crash marker (Section 8.3), the orchestrator MUST delete the marker. The agent will re-run in the next execution.

### 6.7 Concurrency Control

#### 6.7.1 Pipeline-Level Exclusion

`[OPS-12]` Before any phase execution, the orchestrator MUST acquire an exclusive pipeline lock. At most one orchestrator instance may execute a pipeline at a time.

`[OPS-13]` Lock acquisition follows these rules:

- If no lock file exists, the orchestrator creates one with exclusive-create semantics (to prevent TOCTOU races) and proceeds.
- If a lock file exists and its age meets or exceeds the staleness threshold, the orchestrator deletes the stale lock and creates a new one.
- If a lock file exists and is not stale, the orchestrator MUST halt with a contention message.
- On exclusive-create failure (another process created the file between the check and the create), the orchestrator retries up to `retry_count` times with `retry_delay` between attempts.

`[OPS-14]` The lock MUST be released on ALL exit paths — success, failure, abort, and interrupt. The orchestrator MUST register a cleanup handler at startup.

`[OPS-15]` On phase transitions, the orchestrator SHOULD update the lock's `phase` field for diagnostic purposes.

#### 6.7.2 Consensus State Lock

`[OPS-16]` Consensus state updates use a separate, finer-grained lock with a shorter staleness threshold (RECOMMENDED: 15 minutes). This lock protects read-modify-write cycles on the consensus state store. It is acquired and released within a single operation, not held across phases.

### 6.8 Context Budget

`[OPS-17]` Agent invocations SHOULD be bounded by a configurable turn limit to prevent context window overflow.

`[OPS-18]` If an agent fails with a context overflow signal, the orchestrator SHOULD retry with a reduced turn budget (RECOMMENDED: 60-75% of original).

### 6.9 Finalization

`[OPS-19]` When a pipeline reaches a terminal state, the orchestrator MUST:

1. If a writing quality standard is configured, run a quality pass on all prose output files. The pass edits files in-place, preserving content structure and identifiers. Failure is non-blocking.
2. Generate a consensus report summarizing per-round consensus rates, unique counts, and verification status.
3. Archive the active audit trail to a permanent, timestamped file. Generate an audit summary.
4. Clean up transient artifacts (Section 4.12) if the pipeline completed fully. If terminated due to gap tolerance, retain transient artifacts for potential resume.

`[OPS-20]` Finalization steps are best-effort: failure in any step MUST NOT prevent subsequent steps.

### 6.10 Audit Trail

`[OPS-21]` The orchestrator MUST emit structured audit events throughout execution, conforming to the AuditEvent schema (Section 4.10).

`[OPS-22]` The orchestrator MUST emit events at minimum when: a phase starts, an agent is invoked, an agent completes, a quality gate is evaluated, a gap is recorded, and the pipeline reaches a terminal state.

`[OPS-23]` Agents SHOULD embed decision records within their output files using structured markers. Decision markers are informational and do not affect execution semantics.

---

## 7 Consensus Protocol

### 7.1 Finding Similarity

`[CON-01]` Two findings from different instances are matchable if they share an exact identifier match, or if their normalized similarity meets or exceeds the configured similarity threshold.

`[CON-02]` Similarity MUST be computed as normalized edit distance: one minus the edit distance between normalized forms divided by the maximum length of the two findings. Normalization consists of lowercasing, trimming whitespace, and stripping punctuation.

> *Note (non-normative):* The matchable relation is reflexive and symmetric but NOT transitive. Two findings may each be matchable with a third finding without being matchable with each other.

> *Note (non-normative):* For structured findings (table rows, records with named fields), the implementation SHOULD define a canonical text serialization before computing similarity. Field ordering SHOULD be normalized so that semantically identical records with different field orderings are recognized as matching.

### 7.2 Finding Groups

`[CON-03]` The content merge agent MUST construct finding groups using a greedy matching algorithm. The algorithm proceeds as follows:

1. Designate the anchor instance (determined by a fixed ordering of instance suffixes — alphabetical: A < B < C). In degraded mode, the lower-lettered surviving instance is the anchor.
2. For each finding in the anchor instance, in document order: scan unmatched findings in each other instance for the best match above the similarity threshold. Group matched findings together.
3. Any unmatched findings from non-anchor instances become singleton groups.

`[CON-04]` Matching MUST be deterministic: given the same set of instance outputs, the same finding groups are produced regardless of the order in which instance outputs are provided to the merge agent.

### 7.3 Consensus Classification

Each finding group receives a classification based on how many instances contributed:

| Classification | Condition | For N=3 |
|---------------|-----------|---------|
| **Unanimous** | All N instances contributed. | 3 of 3 |
| **Majority** | More than 1 but fewer than N. | 2 of 3 |
| **Unique** | Exactly 1 instance contributed. | 1 of 3 |

> *Note (non-normative):* For N=2 (degraded mode), there is no majority category. Findings are either unanimous (2) or unique (1). If the two instances disagree on every finding, all findings are unique, the consensus rate is 0%, and the disputed rule (Section 7.6) applies.

> *Note (non-normative):* For N>3, the majority classification covers a wide range of agreement levels (e.g., 2-of-5 through 4-of-5). Implementations using higher instance counts SHOULD consider weighting attribute resolution by agreement strength rather than treating all majority findings equally. This specification does not prescribe a weighting scheme.

### 7.4 Consensus Rate

`[CON-05]` The consensus rate is the percentage of finding groups classified as unanimous or majority out of all finding groups.

`[CON-06]` If no findings exist across any instance (zero finding groups), the rate is 0 and the orchestrator MUST signal this condition and halt for human decision.

### 7.5 Attribute Resolution

`[CON-07]` When instances agree on a finding's existence (group count of 2 or more) but disagree on an attribute value (e.g., severity, description), the merge agent MUST select the majority value. If no majority exists (all values distinct), the anchor instance's value MUST be selected as a deterministic tiebreaker.

### 7.6 Round Semantics

`[CON-08]` A consensus round for a single agent proceeds through three sub-phases:

**Sub-phase 1 — Instances:**

The orchestrator launches N instances concurrently. Each instance executes the agent independently with no access to other instances' outputs. After all instances complete or fail:

| Successful instances | Outcome |
|---------------------|---------|
| All N | Mode = `normal`. Proceed to merge. |
| N−1 | Mode = `degraded`. Proceed to merge with surviving instances. |
| Fewer than N−1, agent is critical | Retry failed instances, then re-evaluate. |
| Fewer than N−1, agent is non-critical | Mode = `failed`. Record gap. |

**Sub-phase 2 — Content Merge:**

The orchestrator invokes the merge agent with the outputs of all successful instances. The merge agent produces a merged output, from which the consensus rate and unique finding count are derived.

**Sub-phase 3 — Verification (conditional):**

| Condition | Outcome |
|-----------|---------|
| Unique findings exist and consensus rate is between low and high thresholds | Invoke the verify agent to evaluate unique findings. |
| Consensus rate meets or exceeds the high threshold | Verification skipped — high consensus. |
| No unique findings exist | Verification skipped — nothing to verify. |
| Consensus rate is below the low threshold | All findings marked `disputed`. Requires human acknowledgment before the pipeline advances. |

### 7.7 Batched Consensus

`[CON-09]` When a phase contains multiple agents under consensus topology, rounds MUST execute in parallel up to the `batch_size`. Batches are processed sequentially: batch 2 starts only after all rounds in batch 1 complete.

`[CON-10]` The total number of concurrent agent invocations within a batch is the batch size multiplied by the instance count.

### 7.8 Verification Tag Lifecycle

`[CON-11]` Unique findings carry verification tags that track their provenance:

- At merge, all unique findings receive the `unverified` tag.
- If the verifier confirms a finding, the tag transitions to `verified_unique`.
- If the verifier rejects a finding, the finding is removed from the output.
- If the verifier returns uncertain, the tag remains `unverified` (no change).
- If the verifier times out or fails, all remaining `unverified` tags transition to `verification_timeout`.
- If the consensus rate is below the low threshold (disputed path), all findings in the output receive the `disputed` tag. Disputed tags are immutable until human review.

### 7.9 Tag Resolution

`[CON-12]` Before quality assessment agents consume merged outputs, the orchestrator resolves remaining tags. Any findings still tagged as `unverified` or `verification_timeout` are kept as-is with a warning noting the count of remaining tagged findings. No user prompt is required; the pipeline proceeds to quality assessment.

`[CON-13]` Tags are informational annotations. Systems consuming pipeline outputs MUST NOT treat tagged findings differently from untagged findings for functional purposes. Tags exist for human reviewers assessing confidence.

---

## 8 Checkpoint and Recovery

### 8.1 State Persistence

`[CHK-01]` The orchestrator MUST persist state to durable storage using atomic writes (write to temporary path, then rename).

`[CHK-02]` Status tracking SHOULD use per-agent status markers that are individually atomic.

`[CHK-03]` Consensus state (round status, consensus rates, verification results) SHOULD be persisted in a single lock-protected store that supports atomic read-modify-write cycles.

### 8.2 Resume Algorithm

`[CHK-04]` On re-invocation, the orchestrator MUST reconstruct state by scanning persisted checkpoints, following the resume rules in Section 6.6.

`[CHK-05]` The orchestrator MUST reconcile persisted state against filesystem state, following the reconciliation rules in Section 6.6.

### 8.3 Crash Detection

`[CHK-06]` Agents MUST write a crash marker to their output path before analysis begins. The marker MUST be distinguishable from valid output (e.g., a sentinel string that fails format validation, or a file smaller than the minimum size threshold). On success, the agent replaces the marker atomically with the actual output. This ensures that if the agent crashes mid-analysis, a recognizable marker remains at the output path.

`[CHK-07]` On resume, the presence of a crash marker at an agent's output path indicates that the agent crashed mid-analysis. The orchestrator MUST delete the marker and re-run the agent.

---

## 9 Error Semantics

### 9.1 Retry Protocol

`[ERR-01]` When a critical agent fails and retries remain (RECOMMENDED maximum: 2), the orchestrator MUST re-invoke the agent with a scaled timeout. The RECOMMENDED timeout multiplier is `1.0 + 0.5 × attempt_number` applied to the base timeout.

`[ERR-02]` When all retries are exhausted for a critical agent, the orchestrator MUST halt with an actionable error message identifying the failed agent.

`[ERR-03]` When all retries are exhausted for a non-critical agent, the orchestrator MUST record a gap and continue.

### 9.2 Gap Accumulation

`[ERR-04]` Gaps accumulate monotonically across sessions. Gaps recorded in an earlier session MUST remain in all subsequent sessions.

`[ERR-05]` When the gap count meets or exceeds the pipeline's gap tolerance, the orchestrator MUST abort the pipeline.

### 9.3 Criticality Assignment

`[ERR-06]` Criticality assignment MUST follow the rule defined in `[WFC-08]`. All agents not meeting the criticality criteria SHOULD be classified as `non_critical`.

---

## 10 Safety Properties

These properties MUST hold for all conforming Level 2 implementations.

### S1: Monotonic Progress

Pipeline checkpoint state MUST never regress across sessions. The phase index at the end of any session MUST be greater than or equal to the phase index at the end of any earlier session.

*Enforcement:* Each session either advances the phase index (on success) or leaves it unchanged (on failure/gap). No operation decrements the phase index.

### S2: Instance Independence

Within a consensus round, no instance MUST read another instance's output. Each instance reads only from prior-phase merged outputs and pipeline prerequisites.

*Enforcement:* Instances write to distinct paths determined by their suffix. The orchestrator MUST NOT provide one instance's output as input to another.

### S3: Merge Determinism

The merge function MUST produce identical output given the same set of instance outputs, regardless of the order in which instance outputs are provided.

*Enforcement:* The anchor instance is determined by alphabetical ordering of suffixes (A < B < C). The greedy matching algorithm iterates the anchor's findings in document order. Similarity-score ties are broken by document order within the candidate instance.

*Caveat:* Merge determinism holds for a fixed similarity threshold. If the threshold changes between runs, groupings may differ.

### S4: No Silent Capability Loss

A pipeline that consumes functional capability identifiers in its prerequisites MUST preserve all of them in its output. If any capability identifier present in the prerequisites is absent from the output, the pipeline MUST halt via a preservation gate (a quality gate with `halt = true`). Only human override can accept capability loss.

### S5: Bounded Degradation

A pipeline MUST produce output only if the gap count stays below the gap tolerance. If the gap tolerance is breached, the pipeline aborts without producing final output.

### S6: Gate Authority

No agent or orchestrator MUST advance past a halting gate without human authorization. A halting gate that evaluates to `fail` blocks all subsequent phases until a human sets the gate agent's status to `overridden`.

### S7: Pipeline Exclusion

At most one orchestrator instance MUST execute a given pipeline at any time.

*Enforcement:* Pipeline-level lock with exclusive-create semantics (Section 6.7.1). The staleness threshold guarantees liveness for crashed orchestrators.

### S8: Cleanup Completeness

On successful pipeline completion (all phases executed), no transient artifacts SHOULD remain. Cleanup failures produce warnings but do not violate the property — this is a weak (best-effort) guarantee.

---

## 11 Liveness Properties

### L1: Eventual Termination

Given a finite number of phases and a finite retry budget, every pipeline MUST eventually terminate. Each phase either completes (advancing the phase counter) or exhausts retries (recording a gap or aborting). Both outcomes reduce remaining work. Since the phase count and retry count are finite, termination follows.

### L2: Consensus Convergence

For any non-empty set of findings, the consensus protocol MUST produce a classification in bounded steps. The upper bound is the number of anchor findings multiplied by the maximum number of findings in any other instance.

### L3: Lock Release

Every acquired lock MUST eventually be released, even on abnormal termination. The maximum time a lock can be held before it becomes reclaimable is bounded by the staleness threshold.

*Enforcement:* The orchestrator registers a cleanup handler at startup (Section 6.7.1). If the orchestrator crashes without releasing, the staleness threshold guarantees the next invocation can reclaim the lock.

---

## Appendix A Pipeline Archetypes (Non-Normative)

Pipelines instantiate five structural archetypes.

### A.1 Consensus Analysis

A consensus scout phase, followed by batched consensus view phases, sequential quality assessment, parallel synthesis, and singleton evaluation.

### A.2 Sequential Consensus

Each consensus round depends on the prior round's merged output. Used when analysis is inherently sequential.

### A.3 Singleton Synthesis

No consensus phases. Consumes outputs from a prior pipeline. Contains at least one halting gate.

### A.4 Sequential Planning

Heavy inter-phase data dependencies. Sequential agent chains with a halting gate at the end.

### A.5 Hybrid

Transitions from consensus phases to singleton phases within a single pipeline.

---

## Appendix B Model Selection Heuristics (Non-Normative)

| Task Characteristic | Recommended Model |
|--------------------|-------------------|
| Cross-domain reasoning (inputs from 3+ views), semantic transformation, foundation/overview production | Most capable (`opus`) |
| Binary pass/fail validation, pattern-matching integrity checks | Fastest (`haiku`) |
| Standard analysis, documentation, history, migration mapping | Balanced (`sonnet`) |

---

## Appendix C Session Count Estimation (Non-Normative)

For capacity planning, estimate session cost per phase:

| Topology | Estimated Sessions |
|----------|-------------------|
| Consensus (hard boundary) | Per batch: instances session + merge session + verify session (conditional, ~30% frequency). Multiply by number of batches. |
| Parallel | 1 |
| Sequential | Number of agents divided by agents per session (typically 1-2), rounded up. |

---

## Appendix D Decision Authority Matrix (Non-Normative)

| Operation | Authority | Mechanism |
|-----------|-----------|-----------|
| Retire or add capabilities | Human | Manual review |
| Override halting gate | Human | Set status to `overridden` |
| Accept disputed findings | Human | Acknowledgment at session boundary |
| Exceed gap tolerance | Human | Override prompt |
| Change technology stack target | Human | Target specification edit |
| Force-abort running pipeline | Human | Interrupt signal |
| All other execution decisions | Orchestrator | Automatic per this spec |
