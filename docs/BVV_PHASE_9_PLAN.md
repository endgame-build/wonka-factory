# Plan: Phase 9 — Level 2 Planner Integration

## Context

wonka-factory has shipped Phases 1–8: orch/ library, CLI (`wonka run/resume/status`), and agent instruction files (OOMPA, LOOMPA, CHARLIE). Current conformance sits **between Level 1 and Level 2** — CHARLIE.md exists and the role registry already routes `role:planner` to it, but the orchestrator does not yet validate the task graph the planner produces, and no test harness exercises a planner-seeded lifecycle end-to-end.

Phase 9 closes that gap. Per `BUILD_VERIFY_VALIDATE_SPEC.md` §3.2, full Level 2 conformance requires §7.3, §7.4, §7.5, §8.5, §9. §9 (Gate) is already done. §7.3/§7.4 are agent responsibilities satisfied by CHARLIE.md. §8.5.2 worktree isolation is optional (MAY); default DAG serialization is already supported. That leaves **§7.5 Task Graph Well-Formedness (BVV-TG-07..10)** as the only normative gap — plus the V&V plumbing to prove it works.

The spec wording is careful: §7.5 opens with *"the orchestrator **MAY** validate the task graph"*, but the individual BVV-TG-07..10 requirements use **MUST** for the properties themselves. Interpretation: the graph must be well-formed; the orchestrator chooses when/whether to enforce. Phase 9 implements the enforcement as an opt-in (default-on at Level 2) hook.

BVV-DSN-04 guarantees the dispatch loop needs zero changes — the planner is just another agent routed by role tag.

---

## Pre-existing gaps surfaced during research

These are NOT new Phase 9 work items — they're already-shipped issues discovered while planning. Phase 9 depends on them being addressed (or worked around):

- **P1. Dispatch never skips `role:escalation`.** `dispatch.go:459-467` routes every ready task through the role map. Since "escalation" is not in `LifecycleConfig.Roles`, an escalation task created on tick N will be treated as "unknown role" on tick N+1 and spawn *another* escalation task (`escalation-escalation-<orig>`). The implementation plan's Appendix C explicitly says "the dispatch loop skips tasks where role == escalation" — but no code enforces it. **Phase 9 fixes this** before using escalation-task creation as its validation-failure signal. One-line fix in `dispatch()`: `if role == "escalation" { continue }` before the role-map lookup.
- **P2. No `AssertBVV_TG_*` assertions exist** in `invariant.go`. The pattern from `invariant.go:AssertSingleAssignment`/`AssertDependencyOrdering` generalizes; Phase 9 adds `AssertPostPlannerWellFormed` under the `verify` build tag.
- **P3. Project `CLAUDE.md` architecture table** omits `validate.go`. Update when Phase 9 lands.

---

## Scope

### In scope
1. **`orch/validate.go`** — `ValidateLifecycleGraph(store, branch, roles)` implementing BVV-TG-07/09/10 (TG-08 is already enforced at `AddDep` in `ledger_beads.go:548-598`; we add a redundant post-hoc check for defense-in-depth). Returns `*GraphValidationError` with requirement ID and offending task IDs.
2. **Pre-existing-gap fix (P1)** — In `dispatch.go:dispatch()`, skip tasks where `role == "escalation"` before role-map lookup. Add regression test `TestBVV_DSP03a_NoEscalationOfEscalation`.
3. **Engine integration** — After a `role:planner` task transitions to `completed`, run validation before the next dispatch tick. On failure, create an escalation task (safe now that P1 is fixed) + set the lifecycle abort flag + emit `EventGraphInvalid`.
4. **Config flag** — `LifecycleConfig.ValidateGraph bool` (default `true`). CLI escape hatch `--no-validate-graph` for Level 1 operation against pre-populated ledgers.
5. **Event-log kinds** — Add `EventGraphValidated` and `EventGraphInvalid` to `eventlog.go` (bumps the canonical set from 17 to 19; update `AllEventKinds` and any tests that assert the cardinality).
6. **Runtime invariants (P2)** — Add `AssertPostPlannerWellFormed` in `invariant.go`, called from the engine hook under build tag `verify`.
7. **`orch/validate_spec_test.go`** — Unit tests per BVV-TG-* (07, 08, 09, 10, and negatives for 05, 11, 12).
8. **Skip semantics** — Validation is a no-op when (a) `ValidateGraph=false`, or (b) the branch has zero `role:planner` tasks (legitimate Level 1 pre-populated ledger). Codify in `ValidateLifecycleGraph`: return `nil` early in these cases and log the skip via an event.
9. **Multi-planner guard** — Reject branches with >1 `role:planner` task (TG-10 "the plan task" is singular). Covered in the TG-10 reachability check.
10. **Mock planner + work-package fixture** — `orch/testdata/mock-agents/planner.sh` + `orch/testdata/work-packages/example/` (minimal functional/technical/vv specs). Deterministic E2E testing without a real LLM.
11. **`TestE2E_PlannerThenDispatch`** in `engine_e2e_test.go` — Seeds one plan task, runs engine, asserts build→verify→gate ordering and final completion state.
12. **`TestE2E_PlannerIdempotent`** — Runs the mock planner twice against the same ledger; asserts no duplicate tasks (BVV-TG-02 behavior; primarily a planner concern, but we exercise it through the orchestrator's retry path).
13. **`CLAUDE.md` + `BVV_IMPLEMENTATION_PLAN.md` doc updates** — add `validate.go` to the architecture table; tick Phase 9 complete in the Implementation Sequence table.
14. **TLA+ touch-up (optional)** — Extend `BVVLifecycle.tla` with a `PostPlannerWellFormed` invariant. Low priority; defer if time-constrained.

### Out of scope (deferred)
- **Worktree isolation (§8.5.2, BVV-DSP-13)** — MAY, not MUST. Default DAG serialization already works. Defer to a future phase if/when parallel build dispatch is required. `BVV_VV_STRATEGY.md` already tracks this as an open gap (line 389).
- **`--work-package` CLI seeding flag** — Orthogonal usability improvement. Humans can `bd create --label role:planner --label branch:X` manually. Add later if Phase 10 tackles "operator ergonomics."
- **Full real-LLM planner test** — Mock planner suffices for conformance; a real CHARLIE-driven smoke test belongs in integration CI if/when beads+LLM fixtures exist.
- **BVV-TG-04 traceability assertion** — Each build task MUST reference the spec section it implements. This is a planner responsibility with no objective orchestrator check (we can't tell whether a free-text reference is meaningful). Document in CHARLIE.md review only.

---

## Design

### Validation function signature

```go
// orch/validate.go
package orch

type GraphValidationError struct {
    Requirement string   // e.g. "BVV-TG-09"
    Reason      string
    TaskIDs     []string // offending task IDs
}

func (e *GraphValidationError) Error() string { ... }

// ValidateLifecycleGraph checks BVV-TG-07..10 against all tasks carrying
// label "branch:<branch>" in the given store. Returns nil if well-formed.
func ValidateLifecycleGraph(store Store, branch string) error
```

Implementation outline (single pass over the branch-scoped task set):
- **TG-07**: For each task, read its `role` label; reject if missing or not in `lifecycle.Roles`. (Pass the RoleConfig set in via a second parameter or a `ValidateLifecycleGraphWithRoles` variant to keep the signature pure.)
- **TG-08**: Traverse deps from each task, detect cycles via DFS with a color map. Redundant with `AddDep` but catches manual tampering.
- **TG-09**: Count tasks with `role == "gate"`; reject if != 1. Verify the gate transitively depends on every `role:verifier` task (reverse-reachability from gate must cover all V&V tasks).
- **TG-10**: BFS from the plan task (the unique `role:planner` task for the branch); assert every branch-scoped task is visited.

### Engine hook

In `engine.go`, extend the outcome-processing path: when a completed task's role is `planner`, invoke validation before the next dispatch tick. On error, emit an event, create an escalation task (reuse `createEscalation` from `dispatch.go:552-580`), and trip the abort flag via the existing gap-tolerance/abort plumbing. No changes to `dispatch.go` itself — this keeps BVV-DSN-04 intact.

Hook location recommendation: new helper `Engine.onPlannerCompleted(task)` called from the outcome channel handler, not from inside the dispatcher goroutine.

### Role set threading

Validation needs the configured role set (for TG-07). `LifecycleConfig.Roles` (`types.go:180`) is already built by `internal/cmd/config.go:buildRoleRegistry`. Pass `cfg.Roles` into `ValidateLifecycleGraph`; no new plumbing.

### Resume after planner completed but before validation

Edge case: engine crashes after planner task transitions to `completed` but before `onPlannerCompleted` fires. `Engine.Resume()` must detect this and re-run validation. Detection rule: on resume, for each branch, if any `role:planner` task is `completed` and the event log has no `graph_validated` or `graph_invalid` event for that branch, re-run validation. Idempotent — validation has no side effects beyond the event + possible escalation, and escalation creation already guards with `ErrTaskExists` (`dispatch.go:569`).

### Skip semantics (Level 1 compatibility)

`ValidateLifecycleGraph` returns `nil` without emitting events when:
- `cfg.ValidateGraph == false` (caller explicitly opted out), OR
- The branch has zero tasks with `role:planner` (legitimate Level 1 lifecycle, pre-populated ledger).

Anything in between — e.g., one planner task that exited 2 (blocked) — is treated as a legitimate failure path: validation runs and will likely reject orphan/missing-gate conditions, surfacing the degraded state to the operator. This matches BVV-TG-11 semantics (partial graph is not dispatchable at Level 2).

### Mock planner

`orch/testdata/mock-agents/planner.sh`: reads `$ORCH_WORK_PACKAGE` (a dir path passed via `BuildEnv`), emits a fixed task graph via `bd create`/`bd dep add`, exits 0. Used by `TestE2E_PlannerThenDispatch`. Keeps the E2E test hermetic and deterministic.

`orch/testdata/work-packages/example/` — three tiny markdown stubs with `CAP-1 / UC-1.1 / V-1.1` identifiers. Enough for CHARLIE-style decomposition in future manual testing; mock planner may ignore contents.

---

## Files to create / modify

| Action | Path | Reuses |
|---|---|---|
| Create | `orch/validate.go` | `Store.ListTasks`, `Store.ListDeps`, `Task.Role()`, `Task.Branch()` |
| Create | `orch/validate_spec_test.go` | `ledger_mock_test.go` helpers, existing `TestBVV_*` pattern |
| Modify | `orch/dispatch.go` | skip `role == "escalation"` in `dispatch()` (P1 fix) |
| Modify | `orch/dispatch_spec_test.go` | add `TestBVV_DSP03a_NoEscalationOfEscalation` regression |
| Modify | `orch/engine.go` | add `onPlannerCompleted`; reuse `createEscalation`, abort flag |
| Modify | `orch/resume.go` | detect planner-completed-but-not-validated, re-run validation |
| Modify | `orch/resume_spec_test.go` | add resume-after-planner test |
| Modify | `orch/types.go` | add `LifecycleConfig.ValidateGraph bool` |
| Modify | `orch/eventlog.go` | add `EventGraphValidated`, `EventGraphInvalid`; update `AllEventKinds` (17→19) |
| Modify | `orch/eventlog_test.go` | update cardinality assertion |
| Modify | `orch/invariant.go` | add `AssertPostPlannerWellFormed` (build tag `verify`) |
| Modify | `internal/cmd/run.go` | add `--no-validate-graph` flag; default `true` |
| Modify | `internal/cmd/run_test.go` | cover the new flag wiring |
| Modify | `orch/engine_e2e_test.go` | add `TestE2E_PlannerThenDispatch`, `TestE2E_PlannerIdempotent` |
| Create | `orch/testdata/mock-agents/planner.sh` | pattern from `ok.sh`, `handoff.sh` |
| Create | `orch/testdata/work-packages/example/{functional,technical,vv}-spec.md` | — |
| Modify (opt) | `docs/specs/tla/BVVLifecycle.tla` | add `PostPlannerWellFormed` invariant |
| Modify | `docs/BVV_IMPLEMENTATION_PLAN.md` | tick Phase 9 in Implementation Sequence table; cross-ref validate.go |
| Modify | `CLAUDE.md` | add `validate.go` to architecture table |

Critical specs to re-consult while implementing:
- `docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md` §7.5 (lines 461–475) — exact BVV-TG-07..10 prose
- `docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md` §7.4 (lines 418–459) — BVV-TG-02 idempotency semantics
- `docs/BVV_VV_STRATEGY.md` §"Phase 11" (lines 422–434) — test matrix
- `docs/BVV_IMPLEMENTATION_PLAN.md` §Phase 9 (lines 761–776) — concrete guidance

---

## Execution order

Sequenced so each step is independently verifiable. Each ends with `task check` green.

| # | Step | Why first | Effort |
|---|---|---|---|
| 1 | Fix P1 (dispatch skip for `role:escalation`) + regression test | Unblocks step 4's signaling path | S (≤30 min) |
| 2 | Add `EventGraphValidated` / `EventGraphInvalid` to `eventlog.go`; update `AllEventKinds` + any cardinality tests | Needed by steps 3 and 4 | S |
| 3 | Add `LifecycleConfig.ValidateGraph bool`; wire `--no-validate-graph` through `internal/cmd/run.go`; cover flag in `run_test.go` | Wiring-only — no behavior change yet | S |
| 4 | Write `orch/validate.go` (TG-07/08/09/10 + skip semantics + multi-planner guard) | Pure function, no orch dependency | M |
| 5 | Write `orch/validate_spec_test.go` with TG-07/08/09/10 and TG-05/TG-11/TG-12 negative tests | Pins the validator's behavior before integration | M |
| 6 | Add `AssertPostPlannerWellFormed` to `invariant.go` under `verify` build tag | Defense-in-depth; small | S |
| 7 | Wire `Engine.onPlannerCompleted` — calls validator, emits event, creates escalation + aborts on failure | Integrates step 4 with the engine | M |
| 8 | Extend `resume.go` to detect completed-planner-without-validation and re-trigger the hook; add resume test | Closes the crash-between-complete-and-validate gap | S |
| 9 | Author `planner.sh` mock + `work-packages/example/` fixture | Enables E2E step | S |
| 10 | Add `TestE2E_PlannerThenDispatch` and `TestE2E_PlannerIdempotent` (build tag `integration`) | End-to-end conformance proof | L |
| 11 | Doc updates — `CLAUDE.md` architecture table, `BVV_IMPLEMENTATION_PLAN.md` Implementation Sequence | Keeps docs in sync with code | S |
| 12 | (Optional) TLA+ `PostPlannerWellFormed` invariant + small-cfg model check | Formal cross-check | M |

**Rolling total**: ≈3–5 engineering days for steps 1–11; +1 day if step 12 is included. Commit granularity: one `feat(orch):` commit per numbered step (except where steps 1+2 or 4+5 are more naturally paired).

---

## Verification

- **Unit**: `go test -race -tags verify -run TestBVV_TG ./orch/...` — every BVV-TG-* test passes.
- **Property** (if added): `RAPID_CHECKS=10000 go test -race -tags verify -run TestProp_Validate ./orch/...`.
- **Integration**: `go test -race -tags 'verify integration' -run TestE2E_PlannerThenDispatch ./orch/...` — mock planner creates a 4-task graph (plan→build→verify→gate), engine dispatches all in order, final store state matches expectation.
- **Negative smoke**: manually construct a ledger with (a) a cyclic edge inserted by raw DB write, (b) an orphan task, (c) two gate tasks — run `wonka run`, assert each fails with the right `BVV-TG-*` classification in the event log.
- **Regression**: `task check` stays green — no existing BVV-* test regresses.
- **TLA+ cross-check** (optional): `PostPlannerWellFormed` invariant holds in `small.cfg` model-check run.

---

## Open decisions to confirm

Surface these before coding. Recommendations in parens:

1. **Validation timing** — run after every plan-task completion, or only when `ValidateGraph=true`? *(Recommend: gated by flag; default true at Level 2, opt-out via `--no-validate-graph`.)*
2. **Validation failure behavior** — abort lifecycle + create escalation task, or retry planner up to its retry budget? *(Recommend: abort. Planner is idempotent per BVV-TG-02, so an operator can fix the work package and manually reopen. Auto-retrying validation failures risks burning the retry budget on a consistently-broken work package.)*
3. **Event-log bump (17→19 kinds)** — add `EventGraphValidated` + `EventGraphInvalid`, or overload existing `EventEscalationCreated` with a detail string? *(Recommend: add the two new kinds. Separate event kinds make audit trails grep-friendly and match the spec §10.3 pattern.)*
4. **Multi-planner semantics** — reject branches with >1 `role:planner` task, or tolerate (latest-wins)? *(Recommend: reject. BVV-TG-10 references "the plan task" singular; multiple planners is almost certainly a bug.)*
5. **`--work-package` CLI flag** — bundle into Phase 9 or defer to an "operator UX" phase? *(Recommend: defer. Orthogonal to conformance.)*
6. **Worktrees / BVV-DSP-13** — defer or bundle? *(Recommend: defer. Spec §8.5.2 is MAY; default serialization works; already flagged in `BVV_VV_STRATEGY.md:389`.)*

A "yes, proceed with defaults" on all six is the fastest path.
