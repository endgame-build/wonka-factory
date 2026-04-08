# BVV Specification Validation Report

**Spec Version:** 1.0.0-draft (post-defect-fix)
**Validation Date:** 2026-04-04
**Scope:** Internal consistency, cross-spec coherence, temporal correctness

---

## 1. Task State Machine Matrix

States: `open`, `assigned`, `in_progress`, `completed`, `failed`, `blocked`

| State \ Event | `assign` | `session_starts` | `exit_0` | `exit_1_retries_remain` | `exit_1_retries_exhausted` | `exit_2` | `exit_3` | `exit_3_handoff_limit` | `session_timeout` | `watchdog_dead_session` | `human_reopen` | `circuit_breaker_trip` |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **open** | **assigned** BVV-DSP-01, LDG-08 | -- No assignment | -- No session | -- No session | -- No session | -- No session | -- No session | -- No session | -- No session | -- No session | -- Already open | -- No session |
| **assigned** | -- Idempotent (LDG-09) | **in_progress** LDG-14a | -- Not yet executing | -- Not yet executing | -- Not yet executing | -- Not yet executing | -- Not yet executing | -- Not yet executing | -- Not yet executing | -- Not yet executing | -- Non-terminal | -- No session history |
| **in_progress** | -- BVV-S-03 | -- Already executing | **completed** BVV-DSP-04, 8.3.1 | **open** (reset) Sec 11.1 | **failed** + escalation Sec 11.1 | **blocked** + gap Sec 11.2 | **in_progress** (new session) BVV-DSP-14 | Treat as exit 1 → BVV-L-04 | Treat as exit 1 → BVV-ERR-02a | **in_progress** (restart) BVV-ERR-11a | -- Non-terminal | Worker suspended; task → reconciliation SUP-05/06 |
| **completed** | -- Terminal BVV-S-02 | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | **open** BVV-S-02a | -- Terminal |
| **failed** | -- Terminal BVV-S-02 | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | **open** BVV-S-02a | -- Terminal |
| **blocked** | -- Terminal BVV-S-02 | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | -- Terminal | **open** BVV-S-02a | -- Terminal |

### Notes

- **in_progress + exit_3_handoff_limit:** BVV-L-04 converts exit 3 to exit 1. Outcome depends on retry budget: `open` if retries remain, `failed` if exhausted.
- **in_progress + session_timeout:** BVV-ERR-02a terminates session and treats as exit 1. Same retry-budget fork.
- **in_progress + watchdog_dead_session:** BVV-ERR-11a emits `task_handoff` event. If handoff limit not reached, restart session (stays `in_progress`). If reached, treat as exit 1 (retry protocol).
- **in_progress + circuit_breaker_trip:** Worker suspended, task stays `in_progress`. Next reconciliation (11a.2) detects stale `in_progress` with no live session → resets to `open`.
- **human_reopen on terminals:** BVV-S-02a permits. Orchestrator resets retry and handoff counters to zero, emits `escalation_resolved`.

**Verdict: No unspecified cells.** Every (state, event) pair has a defined transition or is provably unreachable.

---

## 2. Cross-Spec Status Reconciliation

| LDG ID | Summary | References Status? | BVV Adaptation |
|---|---|---|---|
| LDG-01 | Ledger durable across restarts | None | INCLUDED. BVV-DSP-16 specifies Beads. |
| LDG-02 | Ledger is single source of truth | None | INCLUDED. |
| LDG-03 | Child tasks via parent_task_id | Parent status derivation | NOT APPLICABLE. BVV uses flat dependency edges. |
| LDG-04 | Unresolved blockers prevent dispatch | Terminal set | ADAPTED. Terminal set: `{completed, failed, blocked}` replaces `{completed, failed, overridden}`. |
| LDG-05 | `deferred` is non-terminal | `deferred` | NOT APPLICABLE. BVV drops `deferred`. Per Section 5.1a. |
| LDG-06 | Reject cyclic dependency edges | None | INCLUDED. Restated in BVV-TG-08. |
| LDG-07 | Deterministic tiebreaker for equal-priority ready tasks | `open` (implicit) | INCLUDED. |
| LDG-07a | Tasks may produce artifacts pre-completion | Non-terminal | INCLUDED. |
| LDG-07b | Agents must not depend on notifications | `assigned` (implicit) | INCLUDED. BVV agents use `ORCH_TASK_ID` env var. |
| LDG-08 | Atomic assignment: set status + assignee | `assigned` | INCLUDED. Beads maps to `open` + `orch:assigned` label. |
| LDG-09 | Assignment idempotent | `assigned` | INCLUDED. |
| LDG-10 | Assignment serialized | `assigned` | INCLUDED. Supports BVV-S-03. |
| LDG-11 | Multi-process-safe serialization | None | INCLUDED. Beads/Dolt provides DB-level serialization. |
| LDG-12 | Atomic writes (write-then-rename) | None | INCLUDED. |
| LDG-13 | Tasks created by operators, agents, or external systems | `open` (implicit) | ADAPTED. BVV restricts: only planning agent and humans create tasks (BVV-TG-01). |
| LDG-14 | New tasks: status=open, assignee=null | `open` | INCLUDED. |
| LDG-14a | assigned→in_progress on session start | `assigned`, `in_progress` | INCLUDED. |
| LDG-15 | Reassignment: atomic, no active session | `assigned` | INCLUDED. BVV does not add new reassignment semantics. |
| LDG-16 | Parent status updated on child terminal | Parent/child | NOT APPLICABLE. No parent-child in BVV. |
| LDG-17 | Parent→completed when all children terminal, none failed | `completed`, `failed` | NOT APPLICABLE. |
| LDG-18 | Parent→failed when any child failed, no non-terminal children | `failed` | NOT APPLICABLE. |
| LDG-19 | Parent stays current while children non-terminal | All non-terminal | NOT APPLICABLE. |
| LDG-20 | Child failure must not auto-fail parent/siblings; escalate | `failed` | NOT APPLICABLE. Spirit preserved: BVV-ERR-03 implements gap tolerance + escalation. |

**Verdict: No contradictions.** LDG-05 divergence and LDG-16-20 non-applicability are explicitly documented in Section 5.1a.

---

## 3. Deadlock and Livelock Analysis

### Scenario 3.1: Hung Agent (No Exit)

**Preconditions:** Task A is `in_progress`. Agent enters infinite loop. Tmux session is alive.
**Event sequence:** Watchdog checks tmux — session exists. No action taken. Task stays `in_progress` indefinitely.
**Mitigation:** BVV-ERR-02a (base session timeout). The orchestrator terminates the session after timeout and treats as exit code 1.
**Residual risk:** None after defect fix. Before the fix, this was a livelock — the most dangerous temporal bug in the spec.

### Scenario 3.2: All Workers Busy, No Tasks Ready

**Preconditions:** MaxWorkers workers all executing. Remaining tasks are `open` but have unsatisfied dependencies.
**Event sequence:** Dispatch tick finds no ready tasks (all deps non-terminal) and no idle workers. Loop idles.
**Outcome:** Eventually active tasks complete, making deps terminal, making more tasks ready. Workers free up.
**Mitigation:** BVV-L-01 (eventual termination) + finite retry/handoff budgets.
**Residual risk:** If ALL active tasks hang simultaneously, all hit session timeout (BVV-ERR-02a) → exit 1 → retry or fail. System makes progress.

### Scenario 3.3: Circular Human Re-opens

**Preconditions:** Task A depends on Task B. Both fail. Human re-opens A. A gets dispatched, fails again (B still failed). Human re-opens A again.
**Event sequence:** Infinite loop of re-open → dispatch → fail → re-open.
**Mitigation:** BVV-L-01 assumes "finite retry budget." Each re-open resets the budget (BVV-S-02a). The loop is bounded only by human behavior.
**Residual risk:** **Known.** BVV-L-01 holds for a closed system. Human intervention injects fresh budget. The spec now documents this assumption (BVV-L-01 non-normative note).

### Scenario 3.4: Circuit Breaker + Reconciliation Race

**Preconditions:** Worker W1 trips circuit breaker (3 rapid failures). W1 is suspended. Task T1 remains `in_progress` with no session.
**Event sequence:** On next reconciliation tick, stale assignment detection (11a.2 step 1) finds T1 `in_progress` with no live session → resets to `open`. T1 re-enters dispatch pool. Gets assigned to W2 (a different worker).
**Mitigation:** SUP-05/06 suspend the worker, not the task. Reconciliation recovers the task.
**Residual risk:** None. Different worker handles the task. If all workers trip circuit breaker, escalation tasks are created (SUP-06).

**Verdict: No deadlocks found.** All stuck states are resolved by: session timeout (BVV-ERR-02a), retry budget exhaustion, gap tolerance, or reconciliation. The only unbounded loop is human re-open (Scenario 3.3), which is inherently external.

---

## 4. Safety Property Enforcement Trace

| Property | Summary | Enforcing Requirements | Verdict |
|---|---|---|---|
| **BVV-S-01** | Lifecycle exclusion (one orchestrator per branch) | BVV-ERR-06, BVV-ERR-10, BVV-DSP-08, BVV-DSP-12, BVV-L-02 | ENFORCED |
| **BVV-S-02** | Terminal status irreversibility | BVV-ERR-01, BVV-ERR-05, BVV-ERR-07, BVV-ERR-08, BVV-ERR-09, BVV-S-02a, BVV-DSP-09 | ENFORCED |
| **BVV-S-02a** | Human re-open protocol | Sec 11a.2 step 5 (detection mechanism), BVV-S-02a itself (counter reset mandate), BVV-L-04 (handoff counter scope) | ENFORCED |
| **BVV-S-03** | Single assignment | BVV-DSP-05, BVV-DSP-15, LDG-08 through LDG-10 | ENFORCED |
| **BVV-S-04** | Dependency ordering | BVV-DSN-01, BVV-TG-08, BVV-DSP-01, BVV-DSP-02, LDG-04 | ENFORCED |
| **BVV-S-05** | Zero content inspection | BVV-DSP-03, BVV-DSP-03a, BVV-DSP-04, BVV-DSN-04, BVV-SS-01 | ENFORCED |
| **BVV-S-06** | Gate authority | BVV-GT-01, BVV-GT-02, BVV-GT-03 | ENFORCED |
| **BVV-S-07** | Bounded degradation | BVV-ERR-03, BVV-ERR-04, BVV-ERR-05, BVV-GT-03 | ENFORCED |
| **BVV-S-08** | Assignment durability | BVV-ERR-07, BVV-ERR-08, BVV-ERR-09, RCV-03, RCV-04 | ENFORCED |
| **BVV-S-09** | Workspace write serialization | BVV-DSP-07, BVV-DSP-10, BVV-DSP-11, BVV-DSP-13 | **PARTIALLY ENFORCED** |
| **BVV-S-10** | Watchdog-retry non-interference | BVV-ERR-11, BVV-ERR-11a, BVV-L-04 | ENFORCED |

### BVV-S-09 Residual Risk

Enforcement splits between the planning agent (serializes overlapping build tasks via dependency edges per BVV-DSP-07) and the orchestrator (dispatches per the dependency graph). If the planning agent omits edges between overlapping build tasks, concurrent writes occur. The orchestrator cannot independently verify file-set disjointness — BVV-DSN-04 (phase-agnostic orchestration) delegates this judgment to the planner. Mitigation: well-formedness validation (BVV-TG-07 through BVV-TG-10) at Level 2; git conflict detection at runtime.

**Verdict: 10 of 11 properties ENFORCED. 1 PARTIALLY ENFORCED** (BVV-S-09 depends on planner correctness, by design).

---

## 5. Replaced Interface Audit

| Command | Orch-Spec Semantics | BVV Replacement | Assessment |
|---|---|---|---|
| `prime` | Return assignment + handoff file | `ORCH_TASK_ID` env var (BVV-DSP-06) + `bd show` | REPLACED |
| `done` | Signal completion + workspace merge | Exit code 0 + direct branch commits + PR gate | REPLACED |
| `done --handoff` | Signal handoff, no status change | Exit code 3 (BVV-DSP-14) | REPLACED |
| `fail` | Signal failure, immediate terminal | Exit code 1 + retry protocol (Sec 11.1) | REPLACED (enhanced with retries) |
| `task create` | Create task in ledger | Planning agent via `bd create` (BVV-TG-01) | REPLACED (restricted to planner) |
| `task reassign` | Reassign task to different worker | None; stale assignments recovered via reconciliation | DROPPED (intentional — orchestrator owns assignment) |
| `status` | Return lifecycle/task state | `bd show` for own task; OBS-04 for lifecycle view | PARTIALLY REPLACED |
| `heartbeat` | Write heartbeat to ledger | Tmux session presence (BVV-ERR-11) | DROPPED (intentional — simpler liveness model) |
| `escalate` | Create escalation task | Orchestrator-initiated at trigger points (BVV-ERR-03, BVV-ERR-04, BVV-DSP-03a, BVV-GT-03) | REPLACED |
| `dashboard` | Read-only system state view | OBS-04 (RECOMMENDED) + audit trail | PARTIALLY REPLACED |
| `version` | Protocol version | Instruction file metadata (static) | DROPPED (acceptable — agents receive version at spawn) |

**Verdict: 7 REPLACED, 2 PARTIALLY REPLACED, 2 DROPPED.** No unjustified gaps. The `status` and `dashboard` commands are RECOMMENDED (OBS-04) rather than MUST — acceptable for a spec that prioritizes simplicity.

---

## 6. Orphan MUST Inventory

| Line | Text (abbreviated) | Disposition |
|---|---|---|
| 191 | "The assignment ledger MUST be implemented using Beads" | **TAGGED** — now BVV-DSP-16 |
| 217 | "An instruction file MUST contain the following sections" | **INFORMATIONAL** — defines instruction file structure, not an orchestrator requirement. Agents are not conformance targets (Sec 3.1). |
| 428 | "Tasks in `in_progress` or `completed` status MUST NOT be modified" | **SCOPED** — within BVV-TG-03's paragraph. Part of TG-03's normative text. |
| 430 | "The planning agent MUST wire dependency edges" | **SCOPED** — within BVV-TG-02's paragraph. Part of TG-02's normative text. |
| 510 | "the orchestrator MUST create an escalation task" (unknown role) | **TAGGED** — now BVV-DSP-03a |
| 533 | "The orchestrator SHOULD capture them... MUST NOT use them for dispatch" | **SCOPED** — within BVV-DSP-04's scope (diagnostic tags). |
| 757 | "the orchestrator MUST NOT classify failures" | **SCOPED** — restates DSN-01 through DSN-03 in context. Not a new requirement. |
| 810 | "These properties MUST hold for all conforming implementations" | **INFORMATIONAL** — preamble to Section 12, not a standalone requirement. |

**Verdict: 2 tagged (BVV-DSP-16, BVV-DSP-03a). 4 scoped within existing requirements. 2 informational.** No untagged normative requirements remain.

---

## 7. Conformance Level Partition

### Summary

**Total unique BVV requirements: 70** (post-formal-verification, up from 68 post-defect-fix, 62 original)

| Level | Count | Domains |
|---|---|---|
| L1 (Core Dispatch) | 52 | DSN(4), AI(3), TG(1), DSP(13), SS(1), ERR(15), S(11), L(4) |
| L2 (Feature Lifecycle) | 18 | TG(11), DSP(4), GT(3) |

New requirements added during validation (all L1): BVV-ERR-02a (base session timeout), BVV-ERR-11a (watchdog handoff semantics), BVV-S-09 (workspace write serialization), BVV-S-10 (watchdog-retry non-interference), BVV-DSP-16 (Beads ledger implementation), BVV-DSP-03a (unknown role escalation).

New requirements added during formal verification (all L1): BVV-ERR-04a (abort cleanup — mark stranded open tasks as blocked), BVV-ERR-10a (lock release preconditions — sessions drained, lifecycle done/aborted). BVV-L-02 was strengthened (lock staleness requires holder crashed) but retains its existing ID.

Note: BVV-TG-01 is L1 (Section 7.1) but references "planning agent" — an L2 concept. At L1, this reduces to "only human operators create tasks."

### L1 Requirements with L2 Cross-Dependencies (7 flagged)

| Requirement | L2 Reference | Impact at L1 |
|---|---|---|
| BVV-AI-02 | References gate role (Section 9) | Vacuously satisfied — no gate at L1; exception clause is inoperative |
| BVV-TG-01 | References planning agent (Section 7.4) | Reduces to "only humans create tasks" at L1 |
| BVV-DSP-08 | References lifecycle scoping | At L1, branch-label filtering is a generic dispatch filter |
| BVV-DSP-12 | References lifecycle locks | At L1, applies to concurrent branch dispatch scopes |
| BVV-S-06 | References PR gate failure | Vacuously true at L1 (no gate) |
| BVV-S-07 | References PR creation | Vacuously true at L1 (no PR mechanism) |
| BVV-S-09 | References BVV-DSP-07 (L2 workspace strategy) | At L1, depends on external task graph providing serialization |

**Verdict: 7 L1→L2 cross-dependencies exist.** All are vacuously satisfied or operationally harmless at L1, but represent specification coupling that could confuse a pure-L1 implementer. Consider adding non-normative notes.

---

## 8. Termination Proof Sketch (BVV-L-01)

**Claim:** Given a finite task graph, finite retry budget *R*, finite gap tolerance *G*, finite handoff limit *H*, and base session timeout *T*, every lifecycle terminates.

**Proof sketch:**

1. Each task has at most *H* handoffs and *R* retries. A handoff-limit breach converts to a retry. Therefore each task has at most *H + R* session attempts before reaching terminal status.

2. Each session attempt has duration at most *T × 1.5^retry_number* (base timeout with escalation). Sessions are finite.

3. A task reaches terminal status via one of:
   - Exit 0 → `completed` (terminal)
   - Exit 1 after *R* retries → `failed` (terminal)
   - Exit 2 → `blocked` (terminal)
   - Exit 3 after *H* handoffs → converted to exit 1 → retry path → eventually `failed` after *R* additional retries

4. Non-critical terminal failures increment the gap counter. After *G* gaps, the lifecycle aborts (BVV-ERR-04) — remaining tasks are not dispatched.

5. Critical failures abort immediately regardless of gap count (BVV-ERR-03).

6. The dispatch loop terminates when all tasks in scope are terminal (Section 8.1.3).

**Bound:** A lifecycle with *N* tasks terminates in at most *N × (H + R + 1) × T × 1.5^R* wall-clock time, assuming sequential execution.

**Assumption:** No external intervention (human re-opens) during the lifecycle run. Human re-opens (BVV-S-02a) inject fresh retry/handoff budgets, creating an unbounded loop if applied indefinitely. BVV-L-01 holds for a **closed system** — the "finite retry budget" premise is violated by external budget injection.

**Verdict: BVV-L-01 HOLDS** under stated premises. The spec now documents the closed-system assumption (BVV-L-01 non-normative note).

---

## 9. Scenario Walkthroughs

### 9.1 Happy Path

1. Human creates plan task: `role:planner`, `branch:feature-x` → **open**
2. Orchestrator dispatches to planner worker → **assigned** → **in_progress**
3. Planner creates: build-1 (→plan), build-2 (→build-1), vv-1 (→build-2), gate (→vv-1). Exits 0.
4. Plan → **completed**. build-1 becomes ready. Dispatched.
5. build-1 → **completed**. build-2 ready. Dispatched.
6. build-2 → **completed**. vv-1 ready. Dispatched.
7. vv-1 → **completed**. gate ready. Dispatched.
8. Gate creates PR. CI passes. Exit 0. gate → **completed**.
9. All tasks terminal. Lifecycle terminates. `lifecycle_completed` event emitted.

**Verified:** Each transition maps to specific BVV requirements.

### 9.2 Planner Partial Failure

1. Planner creates build-1, build-2 (→build-1). Crashes before creating vv-1 and gate. Exit 1.
2. **Level 2:** Orchestrator retries planner. On retry, planner queries existing tasks for `branch:feature-x` (BVV-TG-02). Finds build-1, build-2. Reconciles: adds vv-1 (→build-2), gate (→vv-1). Exits 0.
3. **Level 1:** No well-formedness validation (BVV-TG-11 note). Orchestrator dispatches build-1 immediately. build-1 completes, then build-2 completes. No more ready tasks (no vv-1 or gate). Lifecycle terminates with 2 completed tasks and no PR. Known Level 1 risk (BVV-TG-11).

**Verified:** Level 2 recovers gracefully. Level 1 risk is documented.

### 9.3 Concurrent V&V Conflict

1. vv-1 and vv-2 run in parallel (planning agent did not serialize them — BVV-DSP-10 violated).
2. vv-1 fixes a defect, commits. vv-2 fixes a different defect in the same file, commit fails (git conflict).
3. Per BVV-DSP-11: vv-2 detects conflict, rebases, resolves. If resolution succeeds, exits 0.
4. If resolution fails, vv-2 exits 1. Orchestrator retries. On retry, vv-2 starts from fresh branch HEAD (which includes vv-1's commit). Succeeds.

**Verified:** Conflict resolution protocol works. Residual risk: semantic (not textual) conflicts are undetectable by the spec.

### 9.4 Crash During Reconciliation

1. Orchestrator starts reconciliation. Resets task T1 from `in_progress` to `open` (stale assignment). Crashes before completing step 2 (orphan cleanup).
2. Lifecycle lock is stale. Next orchestrator instance waits for staleness threshold (BVV-L-02), acquires lock (BVV-ERR-06).
3. Runs reconciliation from scratch. T1 is now `open` — no stale assignment to reset. Orphaned sessions re-checked.
4. Dispatch resumes.

**Verified:** Reconciliation is idempotent. Crash during reconciliation is safe.

### 9.5 Parallel Gap Exhaustion

1. MaxWorkers=3. Gap tolerance=3. Three non-critical tasks dispatched simultaneously.
2. All three fail (exit 1, retries exhausted). Gap count increments: 1, 2, 3.
3. After first failure (gap=1): still below tolerance, dispatch continues.
4. After third failure (gap=3): tolerance reached. Orchestrator stops dispatching. Creates escalation.
5. No in-flight tasks (all three already completed). Lifecycle terminates cleanly.

**Alternative:** If 4 tasks are in-flight and all fail, gap count = 4 > tolerance of 3. The overshoot (documented in non-normative note under BVV-ERR-04) is (MaxWorkers - 1) = 2 maximum overshoot.

**Verified:** Gap tolerance works correctly. Overshoot is bounded and documented.

---

## 10. Summary

| Validation Activity | Verdict |
|---|---|
| Task state machine | **COMPLETE** — no unspecified cells |
| Cross-spec status reconciliation | **NO CONTRADICTIONS** — all divergences documented |
| Deadlock/livelock analysis | **NO DEADLOCKS** — all stuck states resolved by timeout/retry/reconciliation |
| Safety property enforcement | **10/11 ENFORCED, 1 PARTIALLY** (BVV-S-09, by design) |
| Replaced interface audit | **7 REPLACED, 2 PARTIAL, 2 DROPPED** — no unjustified gaps |
| Orphan MUST inventory | **CLEAN** — 2 tagged, 4 scoped, 2 informational |
| Conformance level partition | **7 L1→L2 cross-dependencies** — vacuously satisfied but could confuse L1 implementers |
| Termination proof | **HOLDS** under closed-system assumption |
| Scenario walkthroughs | **ALL PASS** — 5 scenarios verified |
| TLC model checking (smoke) | **PASS** — 1,177 distinct states, 9 invariants + 2 action constraints + EventualTermination |
| TLC model checking (small) | **PENDING** — 1.29M distinct states, safety invariants pass, liveness checking in progress |

### Work Package Template Traceability (BVV-TG-04)

The work package template at `forge/skills/work-package/references/WORK_PACKAGE_TEMPLATE.md` provides stable identifiers suitable for BVV-TG-04 traceability:

| Template Section | Identifier Scheme | Build Task Reference | V&V Task Reference |
|---|---|---|---|
| Acceptance Criteria | `AC-NNN` per story | Build task body: "Implements AC-001, AC-002" | V&V task body: "Validates AC-001" |
| Requirements | `REQ-{PREFIX}{NNN}-NN` | Build task body: "Implements REQ-CLI001-01" | V&V task body: "Verifies REQ-CLI001-01" |
| Business Rules | `{PREFIX}-NNN` | Build task body: "Enforces CLI-001" | V&V task body: "Validates rule CLI-001" |
| Test Scenarios | `TS-{PREFIX}-{NNN}-NN` | N/A (test scenarios are V&V inputs) | V&V task body: "Executes TS-CLI-001-01" |

The template supports traceability. The planning agent's instruction file must define the convention for embedding these references in task bodies — this is not specified by BVV (per the non-normative note under BVV-TG-04: "may use any stable identifier scheme").

### Residual Risks (Accept or Mitigate)

1. **BVV-S-09 partial enforcement:** Planner error → concurrent writes. Mitigated by git conflict detection at runtime. Acceptable.
2. ~~**Human re-open loop:** BVV-L-01 requires closed-system assumption.~~ **ADDRESSED** — closed-system assumption now documented in BVV-L-01 non-normative note.
3. **Gap tolerance overshoot:** Documented in non-normative note under BVV-ERR-04. Acceptable.
4. ~~**Stranded open tasks on lifecycle abort:** Undispatched tasks remain in `open` status after abort, violating BVV-L-01.~~ **ADDRESSED** — BVV-ERR-04a added during formal verification. Orchestrator marks remaining open tasks as `blocked` on abort.
5. ~~**Lock release during active dispatch:** Orchestrator could release lock while sessions still running or tasks still dispatchable.~~ **ADDRESSED** — BVV-ERR-10a added during formal verification. Lock release requires sessions drained and lifecycle done/aborted.
6. ~~**Spurious lock staleness cycle:** Lock oscillates held→stale→reacquired without dispatch progress.~~ **ADDRESSED** — BVV-L-02 strengthened during formal verification. Staleness requires holder crashed (not actively dispatching).
4. ~~**L1→L2 specification coupling:** 7 requirements reference L2 concepts.~~ **ADDRESSED** — 7 non-normative Level 1 notes added to the spec.
