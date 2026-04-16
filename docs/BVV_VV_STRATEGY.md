# BVV System Verification & Validation Strategy

**Version:** 1.0.0
**Status:** Approved
**Date:** 2026-04-09
**Companion to:** `docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md` (70 normative requirements), `docs/BVV_IMPLEMENTATION_PLAN.md` (11-phase build plan)

---

## Context

wonka-factory has 70 normative requirements, a TLA+ model encoding 54 of them as mechanically verifiable invariants/temporal properties (plus 1 partial), and a prose validation report with state matrix, deadlock analysis, and termination proof. Go implementation is 0% — no code exists yet.

Before writing implementation code, we need a V&V strategy that ensures every increment is testable by construction and the system as a whole can be certified against both conformance levels (L1: Core Dispatch, L2: Feature Lifecycle).

The strategy creates a **three-layer verification sandwich**: TLA+ model checking proves properties over the abstract model → Go runtime invariants assert the same properties over concrete state → tests exercise specific requirement scenarios. A traceability matrix chains every BVV-* requirement ID through all three layers.

---

## Key Design Decisions

### 1. Co-locate tests with implementation — don't defer to Phase 8

Tests ship alongside their implementation files. The implementation plan's "Phase 8: Tests" becomes "Phase 8: Integration & System-Level V&V" since unit/spec/contract/property tests belong with their source. Each phase has a **gate** — `go test -race -tags verify ./orch/...` must pass before proceeding.

**Why:** Deferring tests creates a gap where regressions accumulate silently. The facet-scan codebase already demonstrates co-location: `dispatch_spec_test.go` alongside `dispatch.go`.

### 2. Runtime invariants in Phase 7, compile-time assertions from Phase 2

`invariant.go` (build-tag `verify`, panics with requirement IDs) depends on Dispatcher/Engine types that don't exist until Phases 6-7. But compile-time interface checks (`var _ Store = (*FSStore)(nil)`) and type-level assertions ship with their types from Phase 2 onward.

### 3. Fork facet-scan test infrastructure with BVV-specific adaptations

| Facet-Scan File | BVV Adaptation |
|---|---|
| `testutil/pipeline_gen.go` | Rewrite → `testutil/graph_gen.go` — random DAG generators using `rapid.Custom` |
| `testutil/failing_store.go` | Adapt — remove `GetChildren`, add `ListTasks`/`ReadyTasks` label filtering |
| `testutil/preset.go` | Adapt — `MockPreset` with `SystemPromptFlag`, role-keyed routing map |
| `testutil/output_validator.go` | **Delete** — BVV has no output validation (BVV-DSP-04) |
| (new) | `testutil/mock_store.go` — in-memory Store for fast unit tests |

### 4. TLA+-to-Go traceability lives in `invariant.go` header comments

Not a separate document. Each `AssertXxx` function's doc comment includes the TLA+ operator name and BVV.tla line reference. Machine-parseable by grep.

```go
// TLA+ Traceability Matrix
//
// Go Assertion                    TLA+ Operator              BVV.tla  Req ID
// AssertTerminalIrreversibility   TerminalIrreversibility    :307     BVV-S-02
// AssertSingleAssignment          SingleAssignment           :321     BVV-S-03
// AssertDependencyOrdering        DependencyOrdering         :327     BVV-S-04
// AssertLifecycleExclusion        LifecycleExclusion         :295     BVV-S-01
// AssertBoundedDegradation        BoundedDegradation         :344     BVV-S-07
// AssertWorkerConservation        TypeOK (worker/session)    :277     WC
// AssertWatchdogNoStatusChange    (by construction)          —        BVV-S-10
```

### 5. Test infrastructure tiers by dependency

Tests are classified by infrastructure requirements so CI pipelines can run the right subset:

| Tier | Dependencies | What runs | Build tags | CI trigger |
|---|---|---|---|---|
| **Tier 0** | None (in-memory) | Types, errors, eventlog, recovery state machines, DetermineOutcome, MockStore-backed dispatch | `verify` | Every commit |
| **Tier 1** | Filesystem (temp dirs) | FSStore contract tests, lock tests, event log file I/O | `verify` | Every commit |
| **Tier 2** | Dolt binary | BeadsStore contract tests | `verify` | Every commit (skip if `dolt` absent) |
| **Tier 3** | tmux | Session, watchdog, engine e2e, fault injection | `verify,integration` | Merge to main |

Test files declare their tier via build tags. `task test` runs Tier 0-2; `task test-integration` adds Tier 3. BeadsStore tests use `testutil.SkipIfNoDolt(t)` to degrade gracefully.

### 6. Smoke vs. full test distinction

Each phase gate has two speeds:

- **Smoke (every commit, <10s):** `go test -race -tags verify -short ./orch/...` — property tests run 100 iterations in `-short` mode, e2e skipped, store contract tests run against MockStore only
- **Full (merge to main):** `task check` + `task test-prop` (RAPID_CHECKS=10000) + integration tests

Property tests use `rapid.Check` with a dynamic count:
```go
func propChecks() int {
    if testing.Short() { return 100 }
    if n, _ := strconv.Atoi(os.Getenv("RAPID_CHECKS")); n > 0 { return n }
    return 1000
}
```

---

## Verification Layers (Bottom-Up)

### Layer 1: Compile-Time

`go vet`, `golangci-lint`, build tags, interface compliance checks. Catches type mismatches, unhandled enum cases (`exhaustive` linter), and ensures both Store implementations satisfy the interface.

### Layer 2: Unit/Spec Tests

One or more test functions per BVV requirement ID. Naming: `TestBVV_{ReqID}_{Description}`. ~50 functions across `*_spec_test.go` files. Run with `go test -race -tags verify`.

### Layer 3: Contract Tests

`RunStoreContractTests(t, factory, reopen)` — parameterized test suite run against both FSStore and BeadsStore. Guarantees both backends produce identical behavior. ~15 subtests.

### Layer 4: Property-Based Tests

`pgregory.net/rapid` with random task DAGs generated by `testutil/graph_gen.go`. 8+ properties covering dispatch invariants, counter monotonicity, and bounded overshoot. Run with `RAPID_CHECKS=10000`.

### Layer 5: Runtime Invariants

Build-tag `verify` assertions in `invariant.go` that panic with requirement IDs at critical code points. 8 functions mirroring TLA+ invariants. Zero overhead when tag absent. CI always runs with `-tags verify`.

### Layer 6: Integration Tests

Build-tag `integration`. Real tmux sessions, mock agent scripts (`testdata/mock-agents/*.sh`), engine e2e scenarios. 6 e2e + 6 fault injection tests. Run on merge to main.

### Layer 7: TLA+ Cross-Reference

54 of 70 requirements encoded as TLC invariants/temporal properties (plus 1 partial). Traceability matrix in `invariant.go` maps Go assertions to TLA+ operators.

---

## Per-Increment Verification Plan

### Phase 2: Foundation Layer

**Implementation:** `types.go`, `errors.go`, `eventlog.go`
**Tests:** `types_test.go`, `errors_test.go`, `eventlog_spec_test.go`
**Requirements covered:** BVV-DSN-04, BVV-DSP-04, BVV-SS-01

| Test | Requirement | Approach |
|---|---|---|
| `TestTaskStatus_Terminal` | BVV-S-02 (type support) | Unit: `StatusBlocked.Terminal()` returns true |
| `TestTask_RoleLabelAccessor` | BVV-DSN-04 | Unit: `Task{Labels: {"role":"builder"}}.Role()` == "builder" |
| `TestTask_BranchLabelAccessor` | BVV-DSP-08 | Unit: branch label extracted correctly |
| `TestDetermineOutcome_AllCodes` | BVV-DSP-04 | Unit: 0→Completed, 1→Failed, 2→Blocked, 3→Handoff, 99→Failed |
| `TestErrors_SentinelWrapping` | Error model | Unit: `errors.Is(wrappedErr, ErrCycle)` |
| `TestEventLog_EmitAndScan` | BVV-SS-01 | Unit: emit 3 events, scan back, verify order |
| `TestEventLog_AllKinds` | Section 10.3 | Unit: all 16 event kinds serializable |

**Negative tests:**
| Test | Violated Precondition | Expected |
|---|---|---|
| `TestTaskStatus_InvalidString` | Unknown status string | `Terminal()` returns false |
| `TestDetermineOutcome_NegativeCode` | Exit code -1 | Maps to OutcomeFailed |

**Gate:** `go test -race -tags verify ./orch/...` + `golangci-lint`

### Phase 3: Store Layer

**Implementation:** `ledger.go`, `ledger_fs.go`, `ledger_beads.go`, `store_factory.go`
**Tests:** `ledger_contract_test.go`, `ledger_fs_test.go`, `ledger_beads_test.go`
**Testutil:** `testutil/mock_store.go`, `testutil/failing_store.go`
**Requirements covered:** BVV-DSP-16, BVV-TG-08, BVV-DSP-08, BVV-S-03, BVV-S-04

Contract test suite (`RunStoreContractTests`):

| Test | Requirement | Approach |
|---|---|---|
| `LDG01_Durability` | LDG-01 | Create, reopen, verify persisted |
| `LDG06_CycleRejection` | BVV-TG-08 | AddDep rejects cycle, returns ErrCycle |
| `LDG08_AtomicAssign` | BVV-S-03 | Assign sets status+assignee atomically |
| `LDG10_ConcurrentAssign` | BVV-S-03 | 10 goroutines race Assign, exactly 1 wins |
| `ReadyTasks_LabelFilter` | BVV-DSP-08 | ReadyTasks("branch:x") excludes "branch:y" |
| `ReadyTasks_DepsNotTerminal` | BVV-S-04 | Task with non-terminal dep not returned |
| `ReadyTasks_BlockedIsTerminal` | Status adaptation | Blocked dep satisfies terminal check |
| `Assign_BlockedTask` | BVV-S-02 | Assign on blocked task returns error |
| `AddDep_SelfCycle` | BVV-TG-08 | AddDep(t, t) rejected |

**Negative tests (in contract suite):**
| Test | Violated Precondition | Expected |
|---|---|---|
| `Assign_TerminalTask` | Assign task with status `completed` or `blocked` | Returns error |
| `Assign_AlreadyAssigned` | Assign task already assigned to another worker | Returns `ErrAlreadyAssigned` |
| `Assign_WorkerBusy` | Assign to worker with active task | Returns `ErrWorkerBusy` |
| `ReadyTasks_InProgressDep` | Dep is `in_progress`, not terminal | Task not returned |
| `GetTask_NotFound` | Unknown task ID | Returns `ErrNotFound` |
| `AddDep_TransitiveCycle` | a→b→c, then AddDep(c, a) | Returns `ErrCycle` |

**Store fallback test:**
| Test | Approach |
|---|---|
| `TestStoreFactory_BeadsFallbackToFS` | `NewStore("beads", dir)` when dolt absent → returns FSStore, no error |

**Gate:** Contract tests pass against both FSStore and BeadsStore. Fallback test passes.

### Phase 4: Infrastructure Layer

**Implementation:** `tmux.go`, `lock.go`, `recovery.go`, `session.go`
**Tests:** `lock_spec_test.go`, `recovery_spec_test.go`, `recovery_prop_test.go`, `session_spec_test.go`
**Requirements covered:** BVV-S-01, BVV-ERR-01, BVV-ERR-04/04a, BVV-ERR-05/06, BVV-ERR-10/10a, BVV-L-04, BVV-S-02a

| Test | Requirement | Approach |
|---|---|---|
| `TestBVV_S01_LifecycleLockExclusion` | BVV-S-01 | Two acquires on same branch, second fails |
| `TestBVV_ERR06_StaleLockRecovery` | BVV-ERR-06 | Create stale lock, new acquire succeeds |
| `TestBVV_ERR10a_LockReleasePrecondition` | BVV-ERR-10a | Release fails when sessions active |
| `TestBVV_ERR01_MonotonicRetryCounter` | BVV-ERR-01 | RecordAttempt increments, never decrements |
| `TestBVV_ERR04_GapToleranceAbort` | BVV-ERR-04 | IncrementAndCheck returns true at threshold |
| `TestBVV_ERR04a_AbortCleanupBlocksOpen` | BVV-ERR-04a | After abort, all open tasks set to blocked |
| `TestBVV_L04_HandoffLimit` | BVV-L-04 | CanHandoff returns false at MaxHandoffs |
| `TestBVV_L04_HandoffNotResetOnRetry` | BVV-L-04 | Retry does not reset handoff counter |
| `TestBVV_S02a_ReopenResetsCounters` | BVV-S-02a | Reset() zeroes both retry and handoff |

Property tests:
- `TestProp_GapMonotonic` (BVV-ERR-05)
- `TestProp_GapAbortExact` (BVV-ERR-04)
- `TestProp_RetryBounded` (BVV-ERR-01)
- `TestProp_HandoffBounded` (BVV-L-04)

**Gate:** All spec + property tests pass.

### Phase 5: Supervision

**Implementation:** `watchdog.go`, `signal.go`
**Tests:** `watchdog_spec_test.go`, `signal_spec_test.go`
**Requirements covered:** BVV-ERR-09/10/11/11a, BVV-S-10, BVV-L-03

| Test | Requirement | Approach |
|---|---|---|
| `TestBVV_ERR11_TmuxLivenessDetection` | BVV-ERR-11 | Mock tmux, dead session detected |
| `TestBVV_ERR11a_WatchdogHandoffEvent` | BVV-ERR-11a | Dead session restart emits task_handoff |
| `TestBVV_ERR11a_WatchdogHandoffLimitFail` | BVV-ERR-11a | At limit, watchdog converts to exit 1 |
| `TestBVV_S10_WatchdogNoRetryCount` | BVV-S-10 | Watchdog restart doesn't increment retry |
| `TestBVV_L03_WorkerRecovery` | BVV-L-03 | Dead session detected, watchdog restarts it, worker returns to pool |
| `TestBVV_ERR09_GracefulShutdownNoStatusChange` | BVV-ERR-09 | SIGINT does not modify task statuses |
| `TestBVV_ERR10_LockReleasedOnShutdown` | BVV-ERR-10 | Cleanup handler releases lock |

### Phase 6: Dispatch Layer (largest verification surface)

**Implementation:** `agent.go`, `dispatch.go`, `gate.go`
**Tests:** `dispatch_spec_test.go`, `dispatch_prop_test.go`, `agent_spec_test.go`, `gate_spec_test.go`
**Testutil:** `testutil/graph_gen.go` (random DAG generator), `testutil/preset.go`, `testutil/fixtures.go` (deterministic graph helpers)
**Requirements covered:** 16 DSP-* + 3 GT-* + AI-01/02/03

**Graph fixture helpers (`testutil/fixtures.go`):**
Pre-built task graphs for readable tests — avoids ad-hoc `CreateTask`/`AddDep` chains:
```go
LinearGraph(store, branch, n)           // plan → build-1 → build-2 → ... → gate
DiamondGraph(store, branch)             // plan → [build-a, build-b] → vv → gate
LifecycleGraph(store, branch)           // plan → build → [vv-1, vv-2] → gate (canonical BVV lifecycle)
ParallelBuildersGraph(store, branch, n) // plan → n independent builders → vv → gate
FailingGraph(store, branch, failIdx)    // like LinearGraph but task[failIdx] uses fail.sh
```

**Random DAG generator (`testutil/graph_gen.go`):**
- Generates acyclic task DAGs with 2-20 tasks via `rapid.Custom`
- Random role assignment from `{builder, verifier, planner, gate}`
- Random criticality labels, branch assignment (1-3 branches)
- Shrinkable — `rapid` minimizes failing cases automatically

Spec tests (`dispatch_spec_test.go`):

| Test | Requirement |
|---|---|
| `TestBVV_DSN01_DAGDrivenDispatch` | BVV-DSN-01 |
| `TestBVV_DSN02_OneTaskPerSession` | BVV-DSN-02 |
| `TestBVV_DSN03_HandoffIsInfrastructureDriven` | BVV-DSN-03 (in `safety_spec_test.go`) |
| `TestBVV_S05_RoutingUsesLabelsOnly` | BVV-S-05 |
| `TestBVV_DSP01_DispatchAllReady` | BVV-DSP-01 |
| `TestBVV_DSP02_NoHoldingReady` | BVV-DSP-02 |
| `TestBVV_DSP03_RoleBasedRouting` | BVV-DSP-03 |
| `TestBVV_DSP03a_UnknownRoleEscalation` | BVV-DSP-03a |
| `TestBVV_DSP04_ExitCodeOutcome` | BVV-DSP-04 |
| `TestBVV_DSP05_OneTaskPerSession` | BVV-DSP-05 |
| `TestBVV_DSP08_LifecycleScoping` | BVV-DSP-08 |
| `TestBVV_DSP09_OrchestratorAuthority` | BVV-DSP-09 |
| `TestBVV_DSP12_ConcurrentLifecycles` | BVV-DSP-12 |
| `TestBVV_DSP14_HandoffNoStatusChange` | BVV-DSP-14 |
| `TestBVV_DSP15_OrchestratorOwnsAssignment` | BVV-DSP-15 |
| `TestBVV_ERR02_EscalatingTimeout` | BVV-ERR-02 |
| `TestBVV_ERR02a_BaseSessionTimeout` | BVV-ERR-02a |
| `TestBVV_ERR03_CriticalTaskAbort` | BVV-ERR-03 |

Property tests (`dispatch_prop_test.go`):
- `TestProp_TerminalCountNeverDecreases` (BVV-S-02)
- `TestProp_NoDepsDispatchedBeforeTerminal` (BVV-S-04)
- `TestProp_SingleWorkerPerTask` (BVV-S-03)
- `TestProp_GapBoundedOvershoot` (BVV-ERR-04, MaxWorkers-1 overshoot)
- `TestProp_ConcurrentOutcomeProcessing` (BVV-S-02, BVV-S-03 under concurrent outcomes)
- `TestProp_MixedOutcomeDAGTerminates` (termination under random exit codes)

Property tests (`recovery_prop_test.go`):
- `TestProp_HandoffRetryBounded` (BVV-ERR-01, BVV-L-04 bounds hold under random op sequences)
- `TestProp_AbortCleanupNoOpenTasks` (BVV-ERR-04a — no open tasks after abort cleanup)

Agent/gate spec tests:

| Test | Requirement |
|---|---|
| `TestBVV_DSP06_OrchTaskIDEnv` | BVV-DSP-06 |
| `TestBVV_AI01_InstructionFileInjection` | BVV-AI-01 |
| `TestBVV_AI02_RoleToInstructionMapping` | BVV-AI-02 |
| `TestBVV_AI03_PresetSelection` | BVV-AI-03 |
| `TestBVV_GT01_NoAutoMerge` | BVV-GT-01 |
| `TestBVV_GT02_GateFailureIsolation` | BVV-GT-02 |
| `TestBVV_GT03_DependentTaskStatusCheck` | BVV-GT-03 |

### Phase 7: Engine, Resume, Invariants

**Implementation:** `engine.go`, `resume.go`, `invariant.go`
**Tests:** `engine_spec_test.go`, `resume_spec_test.go`, `safety_spec_test.go`
**Requirements covered:** BVV-ERR-07/08, BVV-S-02a, all BVV-S-* properties

Resume spec tests:

| Test | Requirement |
|---|---|
| `TestBVV_ERR07_ReconcileBeforeDispatch` | BVV-ERR-07 |
| `TestBVV_ERR08_LiveSessionNotReset` | BVV-ERR-08 |
| `TestBVV_S02a_HumanReopenDetection` | BVV-S-02a |
| `TestBVV_Resume_GapRecoveryFromLog` | BVV-ERR-05 |
| `TestBVV_Resume_RetryRecoveryFromLog` | BVV-ERR-01 |
| `TestBVV_Resume_HandoffRecoveryFromLog` | BVV-L-04 |

Runtime invariants (`invariant.go`, build tag `verify`):

| Assertion | Requirement | TLA+ Operator | Placement |
|---|---|---|---|
| `AssertTerminalIrreversibility` | BVV-S-02 | `TerminalIrreversibility` (:307) | `processOutcome` before `UpdateTask` |
| `AssertSingleAssignment` | BVV-S-03 | `SingleAssignment` (:321) | `Dispatcher.Tick` after `Assign` |
| `AssertDependencyOrdering` | BVV-S-04 | `DependencyOrdering` (:327) | `Dispatcher.Tick` after `ReadyTasks` |
| `AssertLifecycleExclusion` | BVV-S-01 | `LifecycleExclusion` (:295) | `lock.Acquire` |
| `AssertBoundedDegradation` | BVV-S-07 | `BoundedDegradation` (:344) | Gate handler before PR creation |
| `AssertWorkerConservation` | WC | `TypeOK` (:277) | `WorkerPool.Allocate` and `Release` |
| `AssertWatchdogNoStatusChange` | BVV-S-10 | By construction | Watchdog `Check` loop |
| `AssertZeroContentInspection` | BVV-S-05 | By construction | (structural — no code reads agent output) |

Safety spec tests (`safety_spec_test.go`):

| Test | Requirement |
|---|---|
| `TestBVV_S01_LifecycleExclusion` | BVV-S-01 |
| `TestBVV_S02_TerminalIrreversibility` | BVV-S-02 |
| `TestBVV_S03_SingleAssignment` | BVV-S-03 |
| `TestBVV_S04_DependencyOrdering` | BVV-S-04 |
| `TestBVV_S07_BoundedDegradation` | BVV-S-07 |
| `TestBVV_S08_AssignmentDurability` | BVV-S-08 |

### Phase 8: Integration & System-Level V&V

**Tests:** `engine_e2e_test.go`, `engine_fault_test.go` (build tag `integration`)
**Testdata:** `testdata/mock-agents/{ok.sh, fail.sh, blocked.sh, handoff.sh, hang.sh, crash.sh, slow-success.sh}`

Mock agent scripts:

| Script | Exit Code | Purpose |
|---|---|---|
| `ok.sh` | 0 | Task completes |
| `fail.sh` | 1 | Task fails (retryable) |
| `blocked.sh` | 2 | Task blocked (terminal) |
| `handoff.sh` | 3 | Session handoff |
| `hang.sh` | (none) | Tests timeout enforcement |
| `crash.sh` | 1 (delayed) | Partial work then crash |
| `slow-success.sh` | 0 (delayed) | Success after configurable delay |

E2E scenario tests (matching validation report Section 9):

Each e2e test asserts three things: (1) expected final task statuses, (2) mandatory event sequence in audit trail, (3) runtime invariants don't fire.

| Test | Scenario | Report Section | Mandatory Event Sequence | Final Status Assertions |
|---|---|---|---|---|
| `TestE2E_HappyPath` | plan→build→vv→gate, all exit 0 | 9.1 | `lifecycle_started → task_dispatched{plan} → task_completed{plan} → task_dispatched{build} → task_completed{build} → task_dispatched{vv} → task_completed{vv} → task_dispatched{gate} → gate_passed → lifecycle_completed` | All tasks `completed` |
| `TestE2E_RetryThenSucceed` | build fails once, retries, succeeds | — | `...→ task_dispatched{build} → task_retried{build} → task_dispatched{build} → task_completed{build} → ...` | Build `completed` after retry |
| `TestE2E_HandoffSuccess` | exit 3 then exit 0 — happy-path handoff (BVV-DSP-14) | — | `task_dispatched → task_handoff → task_completed`; no `task_failed`/`task_blocked`/`handoff_limit_reached` | Task `completed` |
| `TestE2E_PlannerPartialFailure` | Planner crashes, retries, reconciles | 9.2 | `...→ task_retried{plan} → task_completed{plan} → ...` | Plan `completed`, subsequent tasks dispatched |
| `TestE2E_ConcurrentVVConflict` | Parallel V&V, git conflict, retry | 9.3 | Contains `task_retried` for conflicting V&V task | Both V&V `completed` |
| `TestE2E_CrashDuringReconciliation` | Kill orchestrator during reconcile, resume | 9.4 | Second run's log starts with reconciliation, then dispatch | All tasks eventually terminal |
| `TestE2E_ParallelGapExhaustion` | 3 workers, all fail, gap=3, abort | 9.5 | `...→ gap_recorded × 3 → escalation_created → lifecycle_completed` | Failed tasks `failed`, remaining `blocked` (BVV-ERR-04a) |

**Event sequence validation helper:**
```go
// ValidateEventSequence checks that the event log contains the specified
// event kinds in order (not necessarily contiguous — other events may appear between).
func ValidateEventSequence(t *testing.T, logPath string, expected []EventKind)
```

Fault injection tests:

| Test | Fault | Requirements |
|---|---|---|
| `TestFault_KillTmuxSession` | Kill tmux mid-run | BVV-ERR-11, BVV-S-08 |
| `TestFault_StoreFailureDuringDispatch` | FailingStore mid-dispatch | Graceful degradation |
| `TestFault_CircuitBreakerTrip` | 3 rapid failures (<60s each) | BVV-L-03 |
| `TestFault_SessionTimeout` | Agent hangs past timeout | BVV-ERR-02a |
| `TestFault_ConcurrentLockContention` | Two engines race for lock | BVV-S-01, BVV-ERR-06 |
| `TestFault_FailureThenRetry` | Exit 1 then exit 0 | BVV-ERR-01 (engine level) |
| `TestE2E_HandoffSuccess` | Exit 3 then exit 0 | BVV-DSP-14 (engine level) |

**BVV-DSP-13 (worktree merge-back failure):** NOT YET COVERED. Open gap until worktree merge-back is wired into the dispatcher. The earlier placeholder (`TestFault_WorktreeMergeConflict`) was renamed to `TestFault_FailureThenRetry` because it only exercised the generic exit-1-retry path, not the actual merge-back code.

### Phase 9: CLI

**Implementation:** `cmd/wonka/main.go`, `internal/cmd/run.go`, `internal/cmd/resume.go`, `internal/cmd/status.go`
**Tests:** `internal/cmd/run_test.go`, `internal/cmd/resume_test.go`, `internal/cmd/status_test.go`

| Test | Approach |
|---|---|
| `TestRunCmd_RequiresBranch` | Missing `--branch` flag → error |
| `TestRunCmd_DefaultFlags` | Verify defaults: workers=4, gap-tolerance=3, timeout=30m, etc. |
| `TestResumeCmd_RequiresBranch` | Missing `--branch` flag → error |
| `TestStatusCmd_OutputFormat` | JSON output contains task statuses, roles, assignees |

### Phase 10: Agent Instruction Files

**Implementation:** `agents/OOMPA.md`, `agents/LOOMPA.md`
**Validation:** Automated structural lint + manual review checklist

**Structural lint (`TestAgentInstructionFiles_Structure`):**
- Required sections present: Phases, Decision Rules, Operating Rules, Completion Protocol, Memory Format
- YAML frontmatter parseable (if present)
- No references to removed commands (`bd ready`, `bd close` → should use `bd show $ORCH_TASK_ID`)
- Exit code protocol documented (0/1/2/3)
- No hardcoded task IDs or branch names

**Manual review checklist:**
- [ ] Orient phase reads `$ORCH_TASK_ID` not `bd ready`
- [ ] Report phase does NOT call `bd close` (orchestrator owns closure)
- [ ] Decision rules are precedence-ordered (first match wins)
- [ ] Memory format section defines PROGRESS.md structure
- [ ] Completion protocol matches BVV exit code semantics

### Phase 11: Level 2 — Planner Integration

**Implementation:** `agents/CHARLIE.md`, `orch/validate.go` (graph well-formedness)
**Tests:** `orch/validate_spec_test.go`, L2 integration tests

| Test | Requirement | Approach |
|---|---|---|
| `TestBVV_TG07_ValidRoleTags` | BVV-TG-07 | Task without valid role → validation fails |
| `TestBVV_TG08_Acyclic` | BVV-TG-08 | Cyclic graph → validation fails (redundant with AddDep, defense-in-depth) |
| `TestBVV_TG09_SingleGate` | BVV-TG-09 | Zero or 2+ gate tasks → validation fails |
| `TestBVV_TG10_AllReachable` | BVV-TG-10 | Orphan task → validation fails |
| `TestBVV_TG05_OrchestratorNeverCreatesPlanTask` | BVV-TG-05 | Engine.Run doesn't call CreateTask for role:planner |
| `TestBVV_TG12_NoBranchDeletion` | BVV-TG-12 | Engine never calls branch delete |

---

## Concurrency Stress Testing

Beyond `-race` flag and contract-level `LDG10_ConcurrentAssign`, the dispatch loop has logical concurrency that needs targeted testing.

### Outcome channel stress (`dispatch_prop_test.go`)

Property: `TestProp_ConcurrentOutcomeProcessing`
- Generate random DAG with 5-15 tasks, 3 workers
- Simulate rapid concurrent completions: multiple `runAgent` goroutines send outcomes to channel simultaneously
- Verify: gap counter matches expected count, no task transitions to terminal twice, worker pool conservation holds

### Dispatch-during-outcome race (`dispatch_spec_test.go`)

Test: `TestBVV_DSP02_TickBoundaryDispatch`
- Task A completes (unlocks task B as ready) while outcome processing is mid-flight
- Verify via a spawn counter: task B is dispatched on the *next* tick, not reentrantly during outcome processing of task A

---

## Coverage Measurement

### Targets

| Package | Line Coverage Target | Rationale |
|---|---|---|
| `orch/` (critical paths: dispatch.go, engine.go, resume.go, recovery.go) | ≥ 95% | Core orchestrator logic — every branch matters |
| `orch/` (remaining: types.go, errors.go, tmux.go, eventlog.go) | ≥ 80% | Infrastructure code, some error paths are hard to trigger |
| `internal/cmd/` | ≥ 70% | CLI wiring — less critical than library |

### Measurement

```bash
# Generate coverage profile
go test -race -tags verify -coverprofile=coverage.out ./orch/... ./internal/...

# Per-function coverage
go tool cover -func=coverage.out

# HTML report
go tool cover -html=coverage.out -o coverage.html
```

Add `task coverage` target to `Taskfile.yml`. CI reports coverage delta on PRs but does NOT gate on it — coverage targets are guidelines, not hard blocks. The conformance gate (requirement ID coverage) is the hard block.

---

## Requirement Traceability Tooling

### Forward trace: requirement → all verification artifacts

`scripts/trace-requirement.sh BVV-S-03` outputs:

```
=== BVV-S-03: Single Assignment ===
SPEC:     docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md:860  (Section 12.2)
TLA+:     docs/specs/tla/BVV.tla:322                    SingleAssignment invariant
INVARIANT: orch/invariant.go:XX                          AssertSingleAssignment()
TESTS:
  orch/safety_spec_test.go:XX        TestBVV_S03_SingleAssignment
  orch/ledger_contract_test.go:XX    LDG10_ConcurrentAssign
  orch/dispatch_prop_test.go:XX      TestProp_SingleWorkerPerTask
```

Implementation: grep across spec, TLA+, `invariant.go`, and `*_test.go` for the requirement ID string.

### Reverse trace: test failure → requirement → TLA+

When `TestBVV_S03_SingleAssignment` fails:
1. Requirement ID `BVV-S-03` is in the test name
2. `grep "BVV-S-03" orch/invariant.go` → `AssertSingleAssignment`
3. `grep "BVV-S-03" docs/specs/tla/BVV.tla` → `SingleAssignment` operator at :322
4. Run TLC with `smoke.cfg` to check if the TLA+ invariant still holds — if it does, the Go implementation diverged from the model

### Requirement coverage audit

`scripts/req-coverage.sh` verifies every requirement ID has at least one test:
```bash
#!/usr/bin/env bash
# Extract all BVV-* requirement IDs from spec
SPEC_IDS=$(grep -oE '\[BVV-[A-Z]+-[0-9]+[a-z]?\]' docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md | sort -u | sed 's/[][]//g')
MISSING=0
for id in $SPEC_IDS; do
  NORMALIZED=$(echo "$id" | sed 's/-/_/g; s/BVV_//')
  if ! grep -rq "TestBVV_${NORMALIZED}" orch/ internal/; then
    echo "MISSING: $id"
    MISSING=$((MISSING + 1))
  fi
done
[ $MISSING -eq 0 ] && echo "ALL REQUIREMENTS COVERED" || echo "$MISSING MISSING"
exit $MISSING
```

---

## Requirements NOT in TLA+ (16 of 70)

15 requirements have TLC?=No in the matrix, plus 1 Partial. Verification method for each:

### By Construction (code structure guarantees compliance)
| Requirement | Why | Verification |
|---|---|---|
| BVV-DSP-16 | Store interface + BeadsStore impl | `var _ Store = (*BeadsStore)(nil)` + contract tests |
| BVV-SS-01 | Orchestrator never imports agent memory | Code review + grep in `TestBVV_S05` |
| BVV-DSP-06 | `BuildEnv` hardcodes ORCH_TASK_ID | `TestBVV_DSP06_OrchTaskIDEnv` |
| BVV-DSP-07/10 | Planning agent responsibility | Property test: at most one builder in_progress per branch |
| BVV-DSP-11 | Agent responsibility (instruction file) | `TestE2E_ConcurrentVVConflict` |

### Dedicated Unit Tests
| Requirement | Test |
|---|---|
| BVV-AI-01 | `TestBVV_AI01_InstructionFileInjection` |
| BVV-AI-03 | `TestBVV_AI03_PresetSelection` |
| BVV-ERR-02 | `TestBVV_ERR02_EscalatingTimeout` |

### Unit + Integration Tests
| Requirement | Unit Test | Integration Test |
|---|---|---|
| BVV-ERR-02a | `TestBVV_ERR02a_BaseSessionTimeout` (mock timer) | `TestFault_SessionTimeout` (real tmux hang) |

### L2 Integration Tests Only
| Requirement | Test |
|---|---|
| BVV-TG-04/05/06/11/12 | L2 integration tests with planner |
| BVV-DSP-13 | NOT YET COVERED (see Phase 8 notes) |

---

## Accepted V&V Limitations

Some properties cannot be fully verified by this strategy. These are documented residual risks, not oversights.

| Property | Limitation | Mitigation |
|---|---|---|
| **BVV-S-09** (Workspace Write Serialization) | Partially enforced — depends on planner correctly serializing build tasks via dependency edges. Orchestrator cannot verify file-set disjointness. | Property test checks at most one builder `in_progress` per branch. Git conflict detection at runtime catches violations the planner missed. |
| **Human re-open loop** (BVV-S-02a) | Unbounded — repeated human re-opens inject fresh retry/handoff budgets, violating the "finite budget" premise of BVV-L-01 (Eventual Termination). | Closed-system assumption documented in spec. Implementations that need guaranteed termination bounds should rate-limit human re-opens. Not testable in automated V&V. |
| **Gap tolerance overshoot** (BVV-ERR-04) | Gap count can exceed configured tolerance by up to MaxWorkers-1 because in-flight tasks continue after the threshold is reached. | Property test `TestProp_GapBoundedOvershoot` verifies the overshoot is bounded. Documented in spec non-normative note. |
| **Agent instruction file correctness** | Structural lint catches format issues but cannot verify behavioral correctness (e.g., "does the builder actually run tests per layer?"). | Manual review checklist. Agent behavior is outside spec scope (Section 1.2). |
| **Beads/Dolt availability** | BeadsStore contract tests skip when `dolt` is absent. CI may pass without testing the primary store backend. | `testutil.SkipIfNoDolt(t)` logs a warning. L1 conformance gate requires BeadsStore contract tests to run. |

---

## Test Parallelism Guidance

Go's `go test` runs packages in parallel and subtests within a package serially by default. Some tests require additional coordination.

### Tests that MUST NOT run in parallel

| Test Category | Reason | Enforcement |
|---|---|---|
| Lock tests (`lock_spec_test.go`) | Tests contend on the same lock file paths | Sequential within package (default Go behavior) |
| Tmux session tests | Shared tmux server, session name collisions | Build tag `integration` isolates from fast tests; use unique run IDs per test |
| Store contract tests with `reopen` | Two Store instances on same directory | Sequential within subtest (default) |
| `TestFault_ConcurrentLockContention` | Intentionally races two engines | Manages its own concurrency internally |

### Tests safe for `t.Parallel()`

All Tier 0 tests (in-memory MockStore, no filesystem, no tmux) can call `t.Parallel()` for faster execution. Property tests are inherently parallelizable within `rapid.Check`.

### Cross-package parallelism

`go test ./orch/... ./internal/...` runs `orch` and `internal/cmd` packages in parallel. This is safe because they don't share mutable state. Tmux integration tests use run-ID-scoped sockets (`wonka-{runID}`) to avoid cross-test interference.

---

## Mock Agent Script Interface

Mock scripts accept configuration via environment variables. All scripts are in `orch/testdata/mock-agents/`.

| Script | Env Vars | Behavior |
|---|---|---|
| `ok.sh` | — | Exits immediately with code 0 |
| `fail.sh` | — | Exits immediately with code 1 |
| `blocked.sh` | — | Exits immediately with code 2 |
| `handoff.sh` | — | Exits immediately with code 3 |
| `slow-success.sh` | `DELAY` (default: 5) | Sleeps for delay seconds, then exits 0 |
| `crash.sh` | — | Sleeps 2 seconds then exits 1 |
| `hang.sh` | — | Sleeps 3600 seconds (traps SIGTERM → exit 143). Tests must kill via timeout. |

All scripts write an `<outcome>` diagnostic tag to stdout before exiting (matching BVV Section 8.3.2) so that diagnostic tag capture can be tested alongside exit code handling.

Scripts read `$ORCH_TASK_ID` from the environment (injected by the orchestrator per BVV-DSP-06) but do not interact with beads — they are pure exit-code simulators.

---

## Conformance Certification

### L1 Conformance Gate (52 requirements)

A system is L1-conformant when ALL pass:

1. `task check` — lint + unit tests + build (0 failures)
2. `task test-prop` — property tests with RAPID_CHECKS=10000 (0 failures)
3. `go test -race -tags verify,integration ./orch/...` — e2e (0 failures)
4. TLC smoke.cfg PASS (all invariants + EventualTermination)
5. TLC small.cfg PASS (all invariants + all liveness properties)
6. Requirement coverage check: all 52 L1 req IDs appear in test names

Automated gate script: `scripts/l1-conformance.sh`

### L2 Conformance Gate (additional 18 requirements)

1. L1 passes
2. `ValidateLifecycleGraph` passes for test graphs (BVV-TG-07..10)
3. TLC lifecycle-safety.cfg PASS
4. TLC lifecycle-liveness-*.cfg PASS (all 4 properties)
5. All 18 L2 req IDs appear in test names
6. CHARLIE.md present and structurally valid
7. E2E test with planner agent completes

### Regression Protocol

When a spec change invalidates a TLA+ property:

1. **Classify:** spec bug (fix prose + model), model bug (fix TLA+ only), or spec evolution (update all three layers)
2. **Update pipeline:** spec → TLC (smoke first) → `invariant.go` → `*_spec_test.go` → `task check`
3. **Traceability audit:** every modified requirement ID triggers review of its TLA+ invariant, Go assertion, and test function
4. **CI catches coverage gaps:** conformance gate fails if any req ID lacks a test function

---

## Test File Summary

| Phase | Implementation | Test Files | Testutil |
|---|---|---|---|
| 2 Foundation | `types.go`, `errors.go`, `eventlog.go` | `types_test.go`, `errors_test.go`, `eventlog_spec_test.go` | — |
| 3 Store | `ledger.go`, `ledger_fs.go`, `ledger_beads.go`, `store_factory.go` | `ledger_contract_test.go`, `ledger_fs_test.go`, `ledger_beads_test.go` | `testutil/mock_store.go`, `testutil/failing_store.go` |
| 4 Infrastructure | `tmux.go`, `lock.go`, `recovery.go`, `session.go` | `lock_spec_test.go`, `recovery_spec_test.go`, `recovery_prop_test.go`, `session_spec_test.go` | — |
| 5 Supervision | `watchdog.go`, `signal.go` | `watchdog_spec_test.go`, `signal_spec_test.go` | — |
| 6 Dispatch | `agent.go`, `dispatch.go`, `gate.go` | `dispatch_spec_test.go`, `dispatch_prop_test.go`, `agent_spec_test.go`, `gate_spec_test.go` | `testutil/graph_gen.go`, `testutil/preset.go`, `testutil/fixtures.go` |
| 7 Engine/Resume | `engine.go`, `resume.go`, `invariant.go` | `engine_spec_test.go`, `resume_spec_test.go`, `safety_spec_test.go` | — |
| 8 Integration | — | `engine_e2e_test.go`, `engine_fault_test.go` | `testdata/mock-agents/*.sh` |
| 9 CLI | `cmd/wonka/`, `internal/cmd/` | `internal/cmd/*_test.go` | — |
| 10 Agents | `agents/OOMPA.md`, `LOOMPA.md` | `agent_instruction_test.go` (structural lint) | — |
| 11 Level 2 | `agents/CHARLIE.md`, `orch/validate.go` | `validate_spec_test.go`, L2 integration tests | — |

**Totals:** ~21 test files, ~60 spec tests + 10 property tests + 6 e2e + 6 fault injection = ~82 test functions

---

## Complete Requirement-to-Verification Matrix (70 rows)

Legend — **Method**: S=Spec test, C=Contract test, P=Property test, I=Integration/E2E, F=Fault injection, R=Runtime invariant, N=Negative test, X=By construction, G=Grep/structural

| # | Req ID | Domain | Level | TLC? | Method | Primary Test File | Notes |
|---|---|---|---|---|---|---|---|
| 1 | BVV-DSN-01 | Design | L1 | Yes | S | `dispatch_spec_test.go` | DAG-driven dispatch |
| 2 | BVV-DSN-02 | Design | L1 | Yes | S | `dispatch_spec_test.go` | One task per session |
| 3 | BVV-DSN-03 | Design | L1 | Yes | X,G | _(by construction)_ | Two-layer memory: orchestrator never reads agent memory files (`PROGRESS.md`, handoff paths). Enforced by structural absence of read sites, verifiable via grep. |
| 4 | BVV-DSN-04 | Design | L1 | Yes | S | `types_test.go` | Phase-agnostic via labels |
| 5 | BVV-AI-01 | Agent | L1 | No | S | `agent_spec_test.go` | Instruction file injection via SystemPromptFlag |
| 6 | BVV-AI-02 | Agent | L1 | Yes | S | `agent_spec_test.go` | Role→instruction file mapping |
| 7 | BVV-AI-03 | Agent | L1 | No | S | `agent_spec_test.go` | Preset selection (--agent flag) |
| 8 | BVV-TG-01 | TaskGraph | L1 | Yes | N | `engine_spec_test.go` | Orchestrator never creates work tasks |
| 9 | BVV-TG-02 | TaskGraph | L2 | Yes | I | `engine_e2e_test.go` | Planner idempotency |
| 10 | BVV-TG-03 | TaskGraph | L2 | Yes | I | `engine_e2e_test.go` | No modify in_progress/completed |
| 11 | BVV-TG-04 | TaskGraph | L2 | No | I | `engine_e2e_test.go` | Traceability in task bodies |
| 12 | BVV-TG-05 | TaskGraph | L2 | No | N | `engine_spec_test.go` | Orchestrator never creates plan tasks |
| 13 | BVV-TG-06 | TaskGraph | L2 | No | I | `engine_e2e_test.go` | Planner creates branch |
| 14 | BVV-TG-07 | TaskGraph | L2 | Yes | S | `validate_spec_test.go` | Valid role tag required |
| 15 | BVV-TG-08 | TaskGraph | L2 | Yes | C | `ledger_contract_test.go` | Cycle rejection (enforced at L1 by AddDep/LDG-06) |
| 16 | BVV-TG-09 | TaskGraph | L2 | Yes | S | `validate_spec_test.go` | Exactly one gate per branch |
| 17 | BVV-TG-10 | TaskGraph | L2 | Partial | S | `validate_spec_test.go` | All tasks reachable from plan |
| 18 | BVV-TG-11 | TaskGraph | L2 | No | S | `validate_spec_test.go` | Well-formedness validation |
| 19 | BVV-TG-12 | TaskGraph | L2 | No | N | `engine_spec_test.go` | No branch deletion |
| 20 | BVV-DSP-01 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Dispatch all ready tasks |
| 21 | BVV-DSP-02 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | No holding ready tasks |
| 22 | BVV-DSP-03 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Role-based routing |
| 23 | BVV-DSP-03a | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Unknown role → escalation |
| 24 | BVV-DSP-04 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Exit code only, no content |
| 25 | BVV-DSP-05 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | One task per session |
| 26 | BVV-DSP-06 | Dispatch | L1 | No | S | `agent_spec_test.go` | ORCH_TASK_ID env var |
| 27 | BVV-DSP-07 | Dispatch | L2 | No | P | `dispatch_prop_test.go` | Workspace serialization (planner responsibility) |
| 28 | BVV-DSP-08 | Dispatch | L1 | Yes | S,C | `dispatch_spec_test.go`, `ledger_contract_test.go` | Branch label scoping |
| 29 | BVV-DSP-09 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Orchestrator authoritative for status |
| 30 | BVV-DSP-10 | Dispatch | L2 | No | P | `dispatch_prop_test.go` | V&V serialization (planner responsibility) |
| 31 | BVV-DSP-11 | Dispatch | L2 | No | I | `engine_e2e_test.go` | V&V conflict resolution |
| 32 | BVV-DSP-12 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Concurrent lifecycle isolation |
| 33 | BVV-DSP-13 | Dispatch | L2 | No | F | `engine_fault_test.go` | Worktree merge-back failure |
| 34 | BVV-DSP-14 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Handoff keeps in_progress |
| 35 | BVV-DSP-15 | Dispatch | L1 | Yes | S | `dispatch_spec_test.go` | Orchestrator owns assignment |
| 36 | BVV-DSP-16 | Dispatch | L1 | No | X,C | `ledger_contract_test.go` | Beads store by construction |
| 37 | BVV-GT-01 | Gate | L2 | Yes | S | `gate_spec_test.go` | No auto-merge |
| 38 | BVV-GT-02 | Gate | L2 | Yes | S | `gate_spec_test.go` | Gate failure isolation |
| 39 | BVV-GT-03 | Gate | L2 | Yes | S | `gate_spec_test.go` | Check predecessor statuses |
| 40 | BVV-SS-01 | Session | L1 | No | S,G | `dispatch_spec_test.go` | No agent memory access |
| 41 | BVV-ERR-01 | Error | L1 | Yes | S,P | `recovery_spec_test.go`, `recovery_prop_test.go` | Monotonic retry counter |
| 42 | BVV-ERR-02 | Error | L1 | No | S | `dispatch_spec_test.go` | Escalating timeout (1.5x formula not in TLA+; base timeout is ERR-02a) |
| 43 | BVV-ERR-02a | Error | L1 | Yes | S,F | `dispatch_spec_test.go`, `engine_fault_test.go` | Base session timeout |
| 44 | BVV-ERR-03 | Error | L1 | Yes | S | `recovery_spec_test.go` | Critical task → immediate abort |
| 45 | BVV-ERR-04 | Error | L1 | Yes | S,P | `recovery_spec_test.go`, `recovery_prop_test.go` | Gap tolerance abort |
| 46 | BVV-ERR-04a | Error | L1 | Yes | S | `recovery_spec_test.go` | Abort cleanup blocks open tasks |
| 47 | BVV-ERR-05 | Error | L1 | Yes | S,P | `recovery_spec_test.go`, `recovery_prop_test.go` | Monotonic gap counter |
| 48 | BVV-ERR-06 | Error | L1 | Yes | S | `lock_spec_test.go` | Stale lock recovery |
| 49 | BVV-ERR-07 | Error | L1 | Yes | S | `resume_spec_test.go` | Reconcile before dispatch |
| 50 | BVV-ERR-08 | Error | L1 | Yes | S | `resume_spec_test.go` | Live session not reset |
| 51 | BVV-ERR-09 | Error | L1 | Yes | S | `signal_spec_test.go` | Graceful shutdown no status change (ReleaseLock UNCHANGED) |
| 52 | BVV-ERR-10 | Error | L1 | Yes | S | `signal_spec_test.go` | Lock released on all exit paths |
| 53 | BVV-ERR-10a | Error | L1 | Yes | S | `lock_spec_test.go` | Sessions drained before release |
| 54 | BVV-ERR-11 | Error | L1 | Yes | S | `watchdog_spec_test.go` | Tmux liveness detection |
| 55 | BVV-ERR-11a | Error | L1 | Yes | S | `watchdog_spec_test.go` | Watchdog handoff semantics |
| 56 | BVV-S-01 | Safety | L1 | Yes | R,S | `invariant.go`, `safety_spec_test.go` | Lifecycle exclusion |
| 57 | BVV-S-02 | Safety | L1 | Yes | R,S,P | `invariant.go`, `safety_spec_test.go`, `dispatch_prop_test.go` | Terminal irreversibility |
| 58 | BVV-S-02a | Safety | L1 | Yes | S | `resume_spec_test.go` | Counter reset on re-open |
| 59 | BVV-S-03 | Safety | L1 | Yes | R,C,P | `invariant.go`, `ledger_contract_test.go`, `dispatch_prop_test.go` | Single assignment |
| 60 | BVV-S-04 | Safety | L1 | Yes | R,S,P | `invariant.go`, `safety_spec_test.go`, `dispatch_prop_test.go` | Dependency ordering |
| 61 | BVV-S-05 | Safety | L1 | Yes | S,G | `safety_spec_test.go` | Zero content inspection |
| 62 | BVV-S-06 | Safety | L1 | Yes | S | `gate_spec_test.go` | Gate authority (vacuous at L1) |
| 63 | BVV-S-07 | Safety | L1 | Yes | R,S | `invariant.go`, `safety_spec_test.go` | Bounded degradation (vacuous at L1) |
| 64 | BVV-S-08 | Safety | L1 | Yes | S,F | `safety_spec_test.go`, `engine_fault_test.go` | Assignment durability |
| 65 | BVV-S-09 | Safety | L1 | Yes | P | `dispatch_prop_test.go` | Workspace write serialization (partial) |
| 66 | BVV-S-10 | Safety | L1 | Yes | R,S | `invariant.go`, `watchdog_spec_test.go` | Watchdog-retry non-interference |
| 67 | BVV-L-01 | Liveness | L1 | Yes | I | `engine_e2e_test.go` | Eventual termination |
| 68 | BVV-L-02 | Liveness | L1 | Yes | S | `lock_spec_test.go` | Lock staleness/recovery |
| 69 | BVV-L-03 | Liveness | L1 | Yes | S,F | `watchdog_spec_test.go`, `engine_fault_test.go` | Worker recovery |
| 70 | BVV-L-04 | Liveness | L1 | Yes | S,P | `recovery_spec_test.go`, `recovery_prop_test.go` | Bounded handoff |


*End of V&V strategy.*
