# BVV System Implementation Plan

## Context

wonka-factory has complete specifications (70 normative requirements), a validated TLA+ model (52 requirements encoded, TLC-verified), and a validation report confirming no deadlocks or contradictions. Go implementation is 0% — no code exists yet.

The `facet-scan/orch` package (10.5K LOC, 45 files) is a production-grade phase-driven pipeline orchestrator. BVV forks it and replaces phase-driven execution with DAG-driven dispatch. ~60% of the code transfers unchanged; the dispatch loop, engine wiring, and resume logic need rewrites; pipeline/phase/consensus types are deleted.

The system has two conformance levels:
- **Level 1 (Core Dispatch):** Pre-populated ledger, no planner, no gate. Sufficient for dispatching tasks created by humans.
- **Level 2 (Feature Lifecycle):** Planner agent (Charlie), PR gate, graph well-formedness validation.

**Strategy: Build Level 1 first, then Level 2.**

---

## Phase 1: Project Scaffolding

### 1.1 Go module initialization

```
go mod init github.com/endgame/wonka-factory
```

Dependencies to declare:
- `github.com/steveyegge/beads` — Beads/Dolt store backend
- `github.com/gofrs/flock` — FSStore file locking
- `github.com/google/uuid` — run ID generation
- `gopkg.in/yaml.v3` — YAML frontmatter parsing
- `github.com/stretchr/testify` — test assertions
- `pgregory.net/rapid` — property-based testing
- `github.com/spf13/cobra` — CLI framework

### 1.2 Taskfile.yml

Create build/test/lint targets matching CLAUDE.md commands:
- `task build` → `CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/wonka ./cmd/wonka`
- `task test` → `go test -race -tags verify -count=1 ./orch/... ./internal/...`
- `task test-prop` → `RAPID_CHECKS=10000 go test -race -tags verify -run Prop ./orch/...`
- `task lint` → `golangci-lint run --timeout=5m`
- `task check` → lint + test + build

### 1.3 Directory structure

```
wonka-factory/
├── orch/                    # Orchestrator library (forked from facet-scan/orch)
│   ├── testutil/            # Test helpers (mock store, mock preset, etc.)
│   └── testdata/mock-agents/ # Shell scripts simulating agent exit codes
├── cmd/wonka/               # CLI binary
├── internal/cmd/            # CLI command implementations
├── agents/                  # Agent instruction files
└── docs/specs/              # (existing) Specs + TLA+
```

---

## Phase 2: orch/ Library — Foundation Layer (Layer 0-1)

Build order follows dependency graph: types → errors → eventlog → store interface → store implementations.

### 2.1 `orch/types.go` — ADAPT from facet-scan

**Remove:** `Pipeline`, `Phase`, `ConsensusConfig`, `QualityGate`, `AgentDef`, `BuildAgentIndex()`, `TaskType` enum, `Topology` enum, `Format` enum.

**Keep:** `WorkerStatus`, `Model`, `LedgerKind`, `Worker`, `Preset` (all unchanged).

**Modify:**
- `TaskStatus` — add `StatusBlocked TaskStatus = "blocked"`. Update `Terminal()` to include it.
- `Criticality` — keep as-is (used by gap tolerance logic).

**Add:**
- `Task` struct — label-based metadata, no parent-child:
  ```go
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
  ```
  Accessor methods: `Role() string`, `Branch() string`, `IsCritical() bool` — read from Labels.

- `RoleConfig` — maps a role name to its instruction file + preset:
  ```go
  type RoleConfig struct {
      InstructionFile string  // path to .md file
      Preset          *Preset
  }
  ```

- `LifecycleConfig` — per-run lifecycle configuration (replaces `Pipeline`):
  ```go
  type LifecycleConfig struct {
      Branch       string
      GapTolerance int
      MaxRetries   int
      MaxHandoffs  int
      BaseTimeout  time.Duration
      Lock         LockConfig
      Roles        map[string]RoleConfig // role name → config
  }
  ```

- `AgentOutcome` — extend with `OutcomeBlocked`, `OutcomeHandoff` (4 values total).

**Design decision — labels as `map[string]string`:** The orchestrator uses labels for all metadata routing (role, branch, critical flag). Beads labels are `"key:value"` strings; the Store implementations parse them into the map on read and serialize back on write. This keeps the Task struct clean and aligns with BVV-DSN-04 (phase-agnostic orchestration).

### 2.2 `orch/errors.go` — ADAPT from facet-scan

**Keep:** `ErrNotFound`, `ErrTaskExists`, `ErrCycle`, `ErrAlreadyAssigned`, `ErrTaskNotReady`, `ErrWorkerBusy`, `ErrPoolExhausted`, `ErrLockContention`, `ErrResumeNoLedger`.

**Remove:** `ErrOutputMissing`, `ErrOutputInvalid` (no output validation), `ErrGateHalt` (gates are regular tasks), `SubprocessError` type.

**Rename:** `ErrPipelineAborted` → `ErrLifecycleAborted`.

**Add:** `ErrHandoffLimitReached`.

### 2.3 `orch/eventlog.go` — ADAPT from facet-scan

Replace event kinds with 16 BVV-specific kinds (from spec Section 10.3):

```go
EventTaskDispatched, EventTaskCompleted, EventTaskFailed, EventTaskRetried,
EventTaskBlocked, EventTaskHandoff, EventWorkerSpawned, EventWorkerReleased,
EventGapRecorded, EventEscalationCreated, EventLifecycleStarted,
EventLifecycleCompleted, EventGateCreated, EventGatePassed, EventGateFailed,
EventEscalationResolved, EventHandoffLimitReached
```

Keep `EventLog` struct, `Emit/Close/Path`, `ProgressReporter` interface, `Event` struct — all unchanged structurally.

### 2.4 `orch/ledger.go` — REWRITE Store interface

```go
type Store interface {
    CreateTask(t *Task) error
    GetTask(id string) (*Task, error)
    UpdateTask(t *Task) error
    ListTasks(labels ...string) ([]*Task, error)       // NEW: all tasks matching labels
    ReadyTasks(labels ...string) ([]*Task, error)      // CHANGED: label filter params
    Assign(taskID, workerName string) error
    CreateWorker(w *Worker) error
    GetWorker(name string) (*Worker, error)
    ListWorkers() ([]*Worker, error)
    UpdateWorker(w *Worker) error
    AddDep(taskID, dependsOn string) error
    GetDeps(taskID string) ([]string, error)
    Close() error
}
```

**Removed:** `GetChildren(parentID)`, `DeriveParentStatus()`.

**Label filter semantics:** `ReadyTasks("branch:feature-x")` returns tasks where status=open, all deps terminal, assignee empty, AND the label `branch:feature-x` is present. Multiple labels are AND-combined.

### 2.5 `orch/ledger_fs.go` — ADAPT from facet-scan

- Add `StatusBlocked` handling (terminal).
- Implement label-filtered `ReadyTasks(labels ...string)`.
- Implement `ListTasks(labels ...string)`.
- Remove `GetChildren`.
- Task JSON files now include `Labels` field.

### 2.6 `orch/ledger_beads.go` — ADAPT from facet-scan

- Add `StatusBlocked` mapping: `blocked` → beads native `blocked` status.
- Add `orch:in_progress` label for in_progress status mapping.
- Rewrite `ReadyTasks(labels ...string)` with beads label filter queries.
- Implement `ListTasks(labels ...string)` using `GetIssuesByLabel`.
- Remove `GetChildren`, `orch:parent=` label convention.
- Remove `orch:type=`, `orch:agent=`, `orch:output=` labels (these were Expand-driven; BVV tasks carry their own labels set by the planner).

### 2.7 `orch/store_factory.go` — FORK unchanged

---

## Phase 3: orch/ Library — Infrastructure Layer (Layer 2-3)

### 3.1 `orch/tmux.go` — FORK from facet-scan

Change socket prefix from `"orch-"` to `"wonka-"`. Otherwise identical.

### 3.2 `orch/lock.go` — ADAPT from facet-scan

- Rename conceptually to LifecycleLock (or keep PipelineLock name, update comments).
- Lock path scoped by branch: `{RunDir}/.wonka-{branch}.lock`.
- `LockContent.Phase` → `LockContent.Branch`.
- `Refresh(phase)` → `Refresh(branch)`.
- Add release precondition check (BVV-ERR-10a): verify all sessions drained before voluntary release.

### 3.3 `orch/recovery.go` — ADAPT from facet-scan

**Keep:** `RetryConfig`, `GapTracker`, `ScaledTimeout`, `RetryJitter`.

**Modify `RetryState`:** Key by task ID instead of agent ID (BVV retries reset the same task, not create new task entities). Remove `RetryTaskID()`, `isRetryTask()`.

**Add `HandoffState`:**
```go
type HandoffState struct {
    counts   map[string]int // taskID → handoff count
    maxLimit int
}
func NewHandoffState(maxHandoffs int) *HandoffState
func (h *HandoffState) CanHandoff(taskID string) bool
func (h *HandoffState) RecordHandoff(taskID string)
func (h *HandoffState) Count(taskID string) int
func (h *HandoffState) Reset(taskID string) // for human re-open (BVV-S-02a)
func (h *HandoffState) SetCounts(counts map[string]int) // for resume recovery
```

**Remove:** Crash markers (`WriteCrashMarker`, `IsCrashMarker`, `RemoveCrashMarker`). BVV agents commit to branch, not write output files. The orchestrator has nothing to marker-protect.

### 3.4 `orch/session.go` — ADAPT from facet-scan

**Keep:** `WorkerPool`, `Allocate`, `Release`, `IsAlive`, `Deallocate`.

**Modify `SpawnSession`:** Remove `AgentDef` parameter. New signature:
```go
func (wp *WorkerPool) SpawnSession(
    workerName string,
    task *Task,
    roleCfg RoleConfig,
    taskID string,         // for ORCH_TASK_ID env injection
    branch string,
) error
```
- Read instruction file via `ReadAgentPrompt(roleCfg.InstructionFile)`.
- Inject `ORCH_TASK_ID` via `BuildEnv` (BVV-DSP-06).
- Remove `ValidateInputs` call (no declared inputs).
- Remove crash marker writing.

**Modify `RestartSession`:** Same parameter changes as `SpawnSession`.

### 3.5 `orch/watchdog.go` — ADAPT from facet-scan

- Remove `pipeline *Pipeline` and `agentIndex` from constructor. Replace with `roles map[string]RoleConfig`.
- On dead session restart: look up task's role label → get `RoleConfig` → call `RestartSession`.
- Add BVV-ERR-11a: watchdog restart emits `task_handoff` event, increments handoff counter. If handoff limit reached, treat as exit code 1 (fail) instead of restarting.

### 3.6 `orch/signal.go` — FORK unchanged

Update `Cleanup` to match renamed types if needed.

---

## Phase 4: orch/ Library — Dispatch Layer (Layer 4)

### 4.1 `orch/agent.go` — ADAPT from facet-scan

**Keep:** `BuildCommand`, `ReadAgentPrompt`, `LogPath`.

**Simplify `DetermineOutcome`:** Pure exit-code switch (no output validation):
```go
func DetermineOutcome(exitCode int) AgentOutcome {
    switch exitCode {
    case 0:  return OutcomeCompleted
    case 1:  return OutcomeFailed
    case 2:  return OutcomeBlocked
    case 3:  return OutcomeHandoff
    default: return OutcomeFailed
    }
}
```

**Modify `BuildEnv`:** Add `ORCH_TASK_ID`, remove `ORCH_WORKSPACE`, `ORCH_ROLE` (agents discover role from the task).

**Remove:** `ValidateOutput`, `ValidateInputs`, `BuildPrompt` (all format validators). BVV outcome determination is exit-code-only (BVV-DSP-04).

### 4.2 `orch/dispatch.go` — REWRITE (core change)

This is the largest single change. Replace the phase-driven dispatch loop with DAG-driven dispatch.

**New `Dispatcher` struct:**
```go
type Dispatcher struct {
    store       Store
    pool        *WorkerPool
    lock        *PipelineLock
    log         *EventLog
    watchdog    *Watchdog
    gaps        *GapTracker
    retries     *RetryState
    handoffs    *HandoffState
    retryCfg    RetryConfig
    lifecycle   *LifecycleConfig
    cfg         DispatchConfig
    branchLabel string            // "branch:<name>" for ReadyTasks filter
    aborted     bool              // lifecycle abort flag
    spawnFunc   SpawnFunc
    outcomes    chan taskOutcome   // runAgent sends outcomes here
    agentWg     sync.WaitGroup
    progress    ProgressReporter
}
```

**New `DispatchResult`:**
```go
type DispatchResult struct {
    Dispatched    int
    LifecycleDone bool
    GapAbort      bool
    Error         error
}
```

**DAG Tick algorithm (3 steps per tick):**

```
Tick(ctx):
  1. PROCESS OUTCOMES — drain outcomes channel (from completed runAgent goroutines):
     For each outcome: process exit code, update task status, handle retry/gap/handoff/abort.
     (This runs on the dispatch goroutine, keeping GapTracker/RetryState/HandoffState single-threaded.)

  2. DISPATCH — ReadyTasks(branchLabel):
     For each ready task (up to idle workers):
       a. role = task.Role()
       b. roleCfg = lifecycle.Roles[role]
       c. If role unknown → create escalation task (BVV-DSP-03a), set task to blocked, continue
       d. worker = pool.Allocate()
       e. store.Assign(taskID, workerName) — atomic
       f. pool.SpawnSession(workerName, task, roleCfg, task.ID, lifecycle.Branch)
       g. emit(EventTaskDispatched)
       h. go runAgent(ctx, task, worker, roleCfg) — sends outcome to channel when done

  3. CHECK TERMINATION:
     allTasks = store.ListTasks(branchLabel)
     If ALL terminal AND no active workers → LifecycleDone = true

  4. LOCK REFRESH — lock.Refresh(lifecycle.Branch)
```

**`runAgent` goroutine:**
```
runAgent(ctx, task, worker, roleCfg):
  1. Poll tmux session at AgentPollInterval until session dies or ctx cancelled
  2. If session timeout reached (BVV-ERR-02a): kill session, exitCode = 1
  3. Read exit code from tmux wait or sidecar file
  4. outcome = DetermineOutcome(exitCode)
  5. Send {task, worker, outcome, exitCode} to outcomes channel
```

**Outcome processing (on dispatch goroutine):**
```
processOutcome(o):
  switch o.outcome:
  case Completed:
    task.Status = StatusCompleted; store.UpdateTask(task)
    pool.Release(worker); emit(EventTaskCompleted)

  case Failed:  // exit code 1
    if retries.CanRetry(task.ID):
      retries.RecordAttempt(task.ID)
      task.Status = StatusOpen; task.Assignee = ""; store.UpdateTask(task)
      pool.Release(worker); emit(EventTaskRetried)
    else:
      task.Status = StatusFailed; store.UpdateTask(task)
      pool.Release(worker); emit(EventTaskFailed)
      handleTerminalFailure(task)

  case Blocked:  // exit code 2
    task.Status = StatusBlocked; store.UpdateTask(task)
    pool.Release(worker); emit(EventTaskBlocked)
    handleTerminalFailure(task)

  case Handoff:  // exit code 3
    if handoffs.CanHandoff(task.ID):
      handoffs.RecordHandoff(task.ID)
      pool.RestartSession(worker, task, roleCfg); emit(EventTaskHandoff)
    else:
      emit(EventHandoffLimitReached)
      // Convert to Failed path (same as exit 1)
      <same as Failed case above>
```

**`handleTerminalFailure`:**
```
handleTerminalFailure(task):
  if task.IsCritical():
    aborted = true; abortCleanup(); emit(EventEscalationCreated)
  else:
    gaps.IncrementAndCheck(task.ID)
    emit(EventGapRecorded)
    if gaps.Count() >= lifecycle.GapTolerance:
      aborted = true; abortCleanup(); emit(EventEscalationCreated)
```

**`abortCleanup` (BVV-ERR-04a):**
```
openTasks = store.ListTasks(branchLabel) where status == open
for each: set status = blocked; store.UpdateTask
```

**Design decision — channel-based outcome processing:** facet-scan processed outcomes in `check()` on the main tick goroutine. BVV does the same via a channel: `runAgent` goroutines send outcomes, the tick drains them. This keeps GapTracker/RetryState/HandoffState single-threaded (they are NOT thread-safe) while allowing concurrent agent monitoring.

### 4.3 `orch/gate.go` — REWRITE for PR gate

The gate handler is a deterministic script (BVV-AI-02), not an AI agent. Built-in implementation:

```go
func ExecuteGate(ctx context.Context, store Store, taskID, repoPath, targetBranch string) int {
    // 1. Check predecessor statuses (BVV-GT-03)
    deps = store.GetDeps(taskID)
    for each dep:
        task = store.GetTask(dep)
        if task.Status == StatusFailed || task.Status == StatusBlocked:
            return 1 // don't create PR if predecessors failed
    // 2. Create PR: gh pr create --base targetBranch --head featureBranch
    // 3. Poll CI: gh pr checks featureBranch --watch (with timeout)
    // 4. All pass → return 0; any fail → return 1
}
```

The gate runs inside a tmux session like any agent. Its "instruction file" is a shell wrapper around this logic.

---

## Phase 5: orch/ Library — Engine & Resume (Layer 5)

### 5.1 `orch/engine.go` — REWRITE

**New `EngineConfig`:**
```go
type EngineConfig struct {
    MaxWorkers int
    Lifecycle  *LifecycleConfig
    RunDir     string
    RepoPath   string
    RunID      string
    LedgerKind LedgerKind
    Dispatch   DispatchConfig
    Watchdog   WatchdogConfig
    Progress   ProgressReporter
}
```

**`Engine.Run(ctx)`:**
1. `init()` — create dirs, open Store, EventLog, TmuxClient, PipelineLock, WorkerPool
2. `lock.Acquire(runID, lifecycle.Branch)`
3. **No `Expand()` call** — ledger is pre-populated by planner or human
4. Create GapTracker, RetryState, HandoffState from config
5. Create Dispatcher + Watchdog
6. Emit `EventLifecycleStarted`
7. `runLoop(ctx)` — start watchdog goroutine, run dispatch.Run, block until terminal
8. Emit `EventLifecycleCompleted`
9. Cleanup (release lock, close store, kill tmux)

**`Engine.Resume(ctx)`:**
1. `initForResume()` — reopen existing Store
2. `lock.Acquire(runID, lifecycle.Branch)` (with staleness recovery per BVV-ERR-06)
3. `Reconcile(store, tmux, lifecycle, logPath)` — DAG-based state reconciliation
4. Initialize GapTracker/RetryState/HandoffState with recovered state
5. Create Dispatcher + Watchdog
6. `runLoop(ctx)` — same as Run
7. Cleanup

### 5.2 `orch/resume.go` — REWRITE for DAG reconciliation

**Reconcile algorithm (BVV Section 11a.2):**

```
Reconcile(store, tmux, lifecycle, logPath) → *ResumeResult:
  1. STALE ASSIGNMENTS:
     tasks = store.ListTasks("branch:" + lifecycle.Branch)
     For each task where status ∈ {assigned, in_progress}:
       alive = tmux.HasSession(sessionName)
       If alive AND in_progress → skip (BVV-ERR-08)
       If NOT alive → task.Status = open, task.Assignee = ""; store.UpdateTask

  2. ORPHANED SESSIONS:
     List all tmux sessions for this run
     Kill any not referenced by an active in_progress task

  3. GAP RECOVERY:
     Scan event log for EventGapRecorded → restore gap count + agent list (BVV-ERR-05)

  4. RETRY RECOVERY:
     Scan event log for EventTaskRetried + EventTaskFailed → restore per-task retry counts (BVV-ERR-01)

  5. HANDOFF RECOVERY:
     Scan event log for EventTaskHandoff → restore per-task handoff counts (BVV-L-04)

  6. HUMAN RE-OPEN DETECTION (BVV-S-02a):
     Scan event log for terminal events (task_completed, task_failed, task_blocked) per task ID
     Compare with current ledger status
     For each task that was terminal but is now open:
       Reset retry and handoff counters to zero
       Emit EventEscalationResolved

  7. WORKER RESET:
     Reset all workers to idle; kill stale session state
```

### 5.3 `orch/invariant.go` — REWRITE for BVV safety properties

Runtime assertions (build tag `verify`) that panic with requirement IDs:

| Assertion | Requirement | Checks |
|-----------|-------------|--------|
| `AssertTerminalIrreversibility` | BVV-S-02 | Orchestrator never reverses a terminal status |
| `AssertSingleAssignment` | BVV-S-03 | At most one worker per task |
| `AssertDependencyOrdering` | BVV-S-04 | No task dispatched before all deps terminal |
| `AssertLifecycleExclusion` | BVV-S-01 | At most one orchestrator per branch |
| `AssertZeroContentInspection` | BVV-S-05 | Routing uses only task metadata |
| `AssertBoundedDegradation` | BVV-S-07 | No PR if gaps >= tolerance |
| `AssertWorkerConservation` | WC | idle + active <= maxWorkers |
| `AssertWatchdogNoStatusChange` | BVV-S-10 | Watchdog never changes task status |

---

## Phase 6: Tests

### 6.1 Spec tests — one per BVV requirement ID

**`orch/dispatch_spec_test.go`:**
- `TestBVV_DSP01_DispatchAllReady` — all ready tasks dispatched up to MaxWorkers
- `TestBVV_DSP02_NoHoldingReady` — ready tasks not held waiting for others
- `TestBVV_DSP03_RoleBasedRouting` — routing by role tag, not content
- `TestBVV_DSP03a_UnknownRoleEscalation` — escalation on missing/unknown role
- `TestBVV_DSP04_ExitCodeOutcome` — outcome from exit code only
- `TestBVV_DSP05_OneTaskPerSession` — fresh session per task
- `TestBVV_DSP06_OrchTaskIDEnv` — ORCH_TASK_ID in session environment
- `TestBVV_DSP08_LifecycleScoping` — ReadyTasks filters by branch label
- `TestBVV_DSP09_OrchestratorAuthority` — orchestrator is authoritative for status
- `TestBVV_DSP14_HandoffNoStatusChange` — exit 3 keeps task in_progress

**`orch/recovery_spec_test.go`:**
- `TestBVV_ERR01_MonotonicRetryCounter`
- `TestBVV_ERR02a_BaseSessionTimeout`
- `TestBVV_ERR03_CriticalTaskAbort`
- `TestBVV_ERR04_GapToleranceAbort`
- `TestBVV_ERR04a_AbortCleanupBlocksOpen`
- `TestBVV_ERR05_MonotonicGapCounter`
- `TestBVV_L04_HandoffLimit`
- `TestBVV_L04_HandoffNotResetOnRetry`

**`orch/resume_spec_test.go`:**
- `TestBVV_ERR07_ReconcileBeforeDispatch`
- `TestBVV_ERR08_LiveSessionNotReset`
- `TestBVV_S02a_HumanReopenDetection`
- `TestBVV_S02a_ReopenResetsCounters`

**`orch/safety_spec_test.go`:**
- `TestBVV_S01_LifecycleExclusion`
- `TestBVV_S02_TerminalIrreversibility`
- `TestBVV_S03_SingleAssignment`
- `TestBVV_S04_DependencyOrdering`

**`orch/ledger_contract_test.go`:**
- Run full Store contract suite against both BeadsStore and FSStore
- Tests: ReadyTasksWithLabels, ListTasksWithLabels, AssignBlockedTask, CycleDetection, BlockedIsTerminal

### 6.2 Property-based tests

**`orch/dispatch_prop_test.go`:**
- Property: terminal task count never decreases (BVV-S-02)
- Property: no task dispatched before all deps terminal (BVV-S-04)
- Property: at most one worker per task at any time (BVV-S-03)
- Property: gaps never exceed tolerance + MaxWorkers - 1 (bounded overshoot)

**`orch/recovery_prop_test.go`:**
- Property: handoff + retry bounded per task
- Property: abort cleanup leaves no open tasks (BVV-ERR-04a)

### 6.3 Integration tests

**`orch/engine_e2e_test.go`** (build tag `integration`):
- Mock agent scripts in `testdata/mock-agents/` that exit with configurable codes
- Scenarios: happy path, retry, blocked, gap abort, handoff limit, resume after crash, concurrent lifecycles

---

## Phase 7: CLI Binary — `cmd/wonka/`

### 7.1 Command structure

```
wonka run     --branch <name> [flags]   # Start fresh lifecycle
wonka resume  --branch <name> [flags]   # Resume interrupted lifecycle
wonka status  --branch <name>           # Show lifecycle status (read-only)
```

### 7.2 Flags

| Flag | Default | Requirement |
|------|---------|-------------|
| `--branch` | (required) | BVV-DSP-08 lifecycle scoping |
| `--ledger` | `beads` | BVV-DSP-16 store selection |
| `--agent` | `claude` | BVV-AI-03 preset selection |
| `--workers` | `4` | MaxWorkers |
| `--gap-tolerance` | `3` | BVV-ERR-04 |
| `--timeout` | `30m` | BVV-ERR-02a base session timeout |
| `--handoff-limit` | `5` | BVV-L-04 |
| `--retry-count` | `2` | BVV-ERR-01 |
| `--agent-dir` | `agents/` | Path to instruction files |

### 7.3 Wiring

`runLifecycle` builds `EngineConfig` from flags, constructs `LifecycleConfig` with role→preset bindings, calls `Engine.Run(ctx)`.

`resumeLifecycle` same, but calls `Engine.Resume(ctx)`.

`showStatus` queries beads directly: `bd list --label "branch:<name>" --json`, displays task table with status/role/assignee.

### 7.4 Preset registry

```go
var Presets = map[string]*orch.Preset{
    "claude": {
        Name:             "claude",
        Command:          "claude",
        Args:             []string{"-p", "--verbose"},
        SystemPromptFlag: "--system-prompt-file",
        ModelFlag:        "--model",
        Env:              map[string]string{},
    },
    // Future: "codex", "goose", etc.
}
```

---

## Phase 8: Agent Instruction Files

### 8.1 `agents/OOMPA.md` — Builder

**Phases:** Orient → Plan → Build → Verify → Report

**BVV adaptations from existing Oompa pattern:**
1. Task discovery: `bd show $ORCH_TASK_ID --json` (replaces `bd ready`)
2. No `bd close` in report phase (orchestrator owns closure)
3. Exit code signaling: 0/1/2/3

**Key sections:**
- Orient: read PROGRESS.md, read task from beads, verify branch, load specs
- Plan: map success criteria to code locations, identify files to create/modify
- Build: implementation sequence (migrations → entities → services → handlers → wiring), test per layer
- Verify: run quality gate (`task check`), verify each success criterion
- Report: commit with conventional format, update PROGRESS.md, exit with code

**Decision rules (precedence):**
1. CLAUDE.md authority → 2. Requirements (task success criteria) → 3. Simplest wins → 4. Missing dependency → exit 2 → 5. Test patterns → 6. Commit format → 7. Fix root causes

### 8.2 `agents/LOOMPA.md` — Verifier

**Phases:** Orient → Discover → Verify + Fix → Report

**Key behavior:**
- Reads build task(s) it depends on to understand what was implemented
- For each verification criterion: traces code path, checks business rule enforcement, classifies as PASS/FIX/SKIP
- FIX: makes minimal fix, runs tests, commits separately
- SKIP: documents what's missing, creates beads issue if needed

**Exit codes:**
- 0: all items PASS or FIX (gaps addressed)
- 1: fix attempt broke something
- 2: critical SKIP (missing infrastructure)
- 3: context pressure

### 8.3 `agents/CHARLIE.md` — Planner (Level 2)

**Phases:** Orient → Decompose → Graph → Validate → Report

**Key behavior:**
- Reads work package (functional-spec + technical-spec + vv-spec)
- Creates build tasks with `role:builder`, V&V tasks with `role:verifier`, gate task with `role:gate`
- Wires dependency edges: build tasks serialized by layer, V&V depends on builds, gate depends on all V&V
- **Idempotency (BVV-TG-02):** queries existing tasks for branch label before creating; reconciles rather than duplicates
- **Never modifies in_progress or completed tasks (BVV-TG-03)**
- Validates well-formedness: acyclic, single gate, all reachable, valid roles

**Exit codes:** 0/1/2 only (no exit 3 — planner completes in one session or fails)

### 8.4 Work package format (non-normative)

```
work-packages/<feature-name>/
  functional-spec.md   # Capabilities, use cases, acceptance criteria (CAP-*, UC-*, AC-*)
  technical-spec.md    # Architecture, tech stack, implementation layers, constraints
  vv-spec.md           # Per-capability verification items (V-*), test requirements
```

Stable identifiers (CAP-1, UC-1.1, V-1.1) provide BVV-TG-04 traceability anchors.

---

## Phase 9: Level 2 — Planner Integration

After Level 1 is solid:

### 9.1 Task graph well-formedness validation

Add to `orch/` (optional, RECOMMENDED per spec):
- `ValidateLifecycleGraph(store, branchLabel)` — checks BVV-TG-07..10 after planner completes
- Every task has valid role tag
- Graph is acyclic (already enforced by AddDep)
- Exactly one gate task
- All tasks reachable from plan task

### 9.2 Planner-to-dispatch integration

The dispatch loop already handles this: after the planner exits 0 (plan task completed), its dependent build tasks become ready via the DAG. No special planner-aware logic needed in the orchestrator — that's the whole point of BVV-DSN-04.

---

## Implementation Sequence Summary

| Step | Files | Depends On | Effort |
|------|-------|------------|--------|
| 1. Scaffolding | go.mod, Taskfile.yml, dirs | — | S |
| 2. Foundation types | types.go, errors.go, eventlog.go | — | M |
| 3. Store interface + impls | ledger.go, ledger_fs.go, ledger_beads.go, store_factory.go | Step 2 | L |
| 4. Infrastructure | tmux.go, lock.go, recovery.go, session.go | Step 3 | M |
| 5. Supervision | watchdog.go, signal.go | Step 4 | S |
| 6. Agent + Dispatch | agent.go, dispatch.go, gate.go | Steps 4-5 | XL |
| 7. Engine + Resume | engine.go, resume.go, invariant.go | Step 6 | L |
| 8. Tests | *_spec_test.go, *_prop_test.go, contract, e2e | Step 7 | XL |
| 9. CLI | cmd/wonka/, internal/cmd/ | Step 7 | M |
| 10. Agent instructions | agents/OOMPA.md, LOOMPA.md | Step 9 | M |
| 11. Level 2 planner | agents/CHARLIE.md, validation | Step 10 | L |

---

## Verification Plan

### Unit/spec tests
```bash
task test   # go test -race -tags verify -count=1 ./orch/... ./internal/...
```

### Property tests
```bash
task test-prop   # RAPID_CHECKS=10000 go test -race -tags verify -run Prop ./orch/...
```

### Manual integration test
```bash
# Pre-populate ledger with 3 tasks: build → verify → gate
bd create --title "build-example" --label "role:builder" --label "branch:test-1"
bd create --title "vv-example" --label "role:verifier" --label "branch:test-1" --depends-on build-example
bd create --title "gate-test-1" --label "role:gate" --label "branch:test-1" --depends-on vv-example

# Run orchestrator
bin/wonka run --branch test-1 --workers 1 --agent claude

# Observe: build dispatched → (agent runs) → verify dispatched → gate dispatched
# Check: bd list --label "branch:test-1" --json → all tasks completed
```

### TLA+ cross-check
After implementation, verify that Go dispatch logic matches TLA+ model:
- Each `AssertXxx` in invariant.go corresponds to a TLA+ invariant in BVV.tla
- Test names reference the same requirement IDs as TLA+ properties

---

## Key Design Decisions

1. **Labels-only Task metadata** — No typed fields for role/branch/critical. The orchestrator reads Labels, consistent with BVV-DSN-04.

2. **Retry resets same task** — BVV resets task to `open` (stable ID), unlike facet-scan which creates new task entities. Simpler, matches TLA+ model.

3. **Channel-based outcome processing** — runAgent goroutines send outcomes to a channel; the dispatch tick processes them serially. Keeps GapTracker/RetryState/HandoffState single-threaded.

4. **Gate as deterministic script** — The PR gate shells out to `gh pr create`/`gh pr checks`. No LLM needed for mechanical logic.

5. **Level 1 first** — Build and test the full dispatch engine with pre-populated ledger before introducing the planner.

6. **Handoff/retry counters recovered from event log** — Not stored in task labels. The event log is the source of truth for monotonic counters, consistent with gap recovery.

---

## Critical Source Files

| Purpose | Path |
|---------|------|
| Primary spec (70 requirements) | `/Users/z0xcu/projects/endgame/wonka-factory/docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md` |
| Validation report (state matrix) | `/Users/z0xcu/projects/endgame/wonka-factory/docs/specs/BVV_VALIDATION_REPORT.md` |
| TLA+ model (task machine) | `/Users/z0xcu/projects/endgame/wonka-factory/docs/specs/tla/BVVTaskMachine.tla` |
| TLA+ model (dispatch) | `/Users/z0xcu/projects/endgame/wonka-factory/docs/specs/tla/BVVDispatch.tla` |
| facet-scan dispatch (rewrite source) | `/Users/z0xcu/projects/endgame/facet-scan/orch/dispatch.go` |
| facet-scan engine (rewrite source) | `/Users/z0xcu/projects/endgame/facet-scan/orch/engine.go` |
| facet-scan types (adapt source) | `/Users/z0xcu/projects/endgame/facet-scan/orch/types.go` |
| facet-scan resume (rewrite source) | `/Users/z0xcu/projects/endgame/facet-scan/orch/resume.go` |
| facet-scan beads store (adapt source) | `/Users/z0xcu/projects/endgame/facet-scan/orch/ledger_beads.go` |

---

## Appendix A: Exit Code Capture Mechanism

tmux does not expose child process exit codes. facet-scan solves this with a **sidecar file** pattern in `BuildShellCommand` (tmux.go:121-167). BVV reuses this mechanism unchanged.

### How it works

`BuildShellCommand` wraps the agent command in a shell pipeline that:
1. Exports environment variables (`export ORCH_TASK_ID=...; export ORCH_BRANCH=...; ...`)
2. Runs the agent command with stdout/stderr redirected to a log file
3. Writes `$?` (the exit code) to a sidecar file at `{logPath}.exitcode`

```bash
# What BuildShellCommand produces (simplified):
export ORCH_TASK_ID='task-abc'; export ORCH_BRANCH='feature-x'; \
  claude -p --verbose --system-prompt-file /tmp/prompt.md \
  > /path/logs/task-abc.stdout 2>&1; \
  echo $? > /path/logs/task-abc.stdout.exitcode
```

With jq text filter (for structured output agents):
```bash
... | tee /path/logs/task-abc.stdout | jq -r --unbuffered '.text' \
  > /path/logs/task-abc.txt 2>/dev/null; \
  echo ${PIPESTATUS[0]} > /path/logs/task-abc.stdout.exitcode
```

`PIPESTATUS[0]` captures the agent's exit code, not jq's. This requires bash (not sh), which tmux's `CreateSession` uses via `bash -c`.

### ReadExitCode

```go
func ReadExitCode(logPath string) (int, error)
```

Reads `{logPath}.exitcode`, parses the integer. Returns -1 if:
- File doesn't exist (agent killed before bash wrote it)
- File is empty (partial write / abrupt kill)

### BVV adaptation

Fork `BuildShellCommand` and `ReadExitCode` unchanged into `orch/tmux.go`. The sidecar pattern is agent-agnostic — it captures exit codes from any process.

In `runAgent`, after detecting session death:
```go
logPath := LogPath(runDir, task.ID)
exitCode, err := ReadExitCode(logPath)
if err != nil || exitCode < 0 {
    exitCode = 1 // unknown → treat as failure (conservative)
}
outcome := DetermineOutcome(exitCode)
```

**Difference from facet-scan:** facet-scan has a fallback: if exit code is unknown (-1) but output file is valid, treat as success. BVV removes this fallback because BVV does not validate output files — exit code is the sole signal (BVV-DSP-04). Unknown exit code → failure.

---

## Appendix B: Session Timeout Enforcement

BVV-ERR-02a requires a configurable base session timeout (default: 30min) with 1.5x escalation per retry (via `ScaledTimeout`).

### Implementation in `runAgent`

```go
func (d *Dispatcher) runAgent(ctx context.Context, task *Task, worker *Worker, roleCfg RoleConfig) {
    // Compute timeout: base × (1.0 + 0.5 × attempt_number)
    attempt := d.retries.AttemptCount(task.ID)
    timeout := ScaledTimeout(d.lifecycle.BaseTimeout, attempt)

    timer := time.NewTimer(timeout)
    defer timer.Stop()

    pollInterval := d.cfg.AgentPollInterval
    ticker := time.NewTicker(pollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return // graceful shutdown (BVV-ERR-09)

        case <-timer.C:
            // BVV-ERR-02a: session timeout expired
            sessionName := SessionName(d.runID, worker.Name)
            _ = d.tmux.KillSession(sessionName) // force-kill
            // Fall through to exit code processing below
            // The sidecar file won't exist → ReadExitCode returns -1 → treated as exit 1

        case <-ticker.C:
            alive, err := d.pool.IsAlive(worker.Name)
            if err != nil { continue }
            if alive { continue }
        }
        break // session is dead (naturally or by timeout kill)
    }

    // Read exit code, determine outcome, send to channel
    exitCode := readExitCodeOrDefault(logPath, 1)
    outcome := DetermineOutcome(exitCode)
    d.outcomes <- taskOutcome{task, worker, outcome, exitCode, roleCfg}
}
```

The timeout kill is indistinguishable from agent exit 1 at the outcome processing level. The sidecar file either doesn't exist (killed before write) or contains the agent's actual exit code (agent exited naturally before timer fired). Both paths converge correctly.

### ScaledTimeout (forked from facet-scan/orch/recovery.go)

```go
func ScaledTimeout(base time.Duration, attempt int) time.Duration {
    return time.Duration(float64(base) * (1.0 + 0.5*float64(attempt)))
}
```

Attempt 0: 30min. Attempt 1: 45min. Attempt 2: 60min.

---

## Appendix C: Escalation Task Specification

Multiple requirements reference creating escalation tasks. Here is the concrete design.

### Escalation task structure

An escalation task is a regular beads task with specific labels. It is NOT dispatched by the orchestrator — it exists for human or triage-agent visibility.

```go
func createEscalationTask(store Store, branch, reason, sourceTaskID, detail string) error {
    t := &Task{
        ID:     fmt.Sprintf("escalation-%s-%d", sourceTaskID, time.Now().Unix()),
        Title:  fmt.Sprintf("ESCALATION: %s", reason),
        Body:   detail,
        Status: StatusOpen,
        Labels: map[string]string{
            "branch":      branch,
            "role":        "escalation",
            "source_task": sourceTaskID,
            "escalation":  "true",
        },
    }
    return store.CreateTask(t)
}
```

### When escalation tasks are created

| Trigger | Requirement | Reason text | Detail content |
|---------|-------------|-------------|----------------|
| Retries exhausted (exit 1) | Sec 11.1 | `"retries exhausted"` | Last exit code, attempt count, task title |
| Critical task failure | BVV-ERR-03 | `"critical task failed"` | Task status, exit code, whether blocked or failed |
| Gap tolerance reached | BVV-ERR-04 | `"gap tolerance reached"` | Gap count, list of failed task IDs |
| Unknown/missing role tag | BVV-DSP-03a | `"unknown role"` | Task ID, role label value (or "missing") |
| Gate failure (CI failed) | BVV-GT-03 | `"gate failed: CI checks"` | PR URL, which checks failed |
| Gate failure (predecessors) | BVV-GT-03 | `"gate failed: predecessors"` | List of failed/blocked predecessor task IDs |
| Circuit breaker trip | SUP-05/06 | `"circuit breaker tripped"` | Worker name, failure timestamps |

### Dispatch behavior

The orchestrator MUST NOT dispatch escalation tasks to agent workers. The dispatch loop skips tasks where `role == "escalation"`. Escalation tasks are consumed by:
- Humans reviewing beads (`bd list --label escalation:true`)
- A triage agent (future, outside BVV scope)

---

## Appendix D: Diagnostic Tag Capture

BVV Section 8.3.2 says agents MAY emit `<outcome>` stdout tags. The orchestrator SHOULD capture them in the audit trail but MUST NOT use them for dispatch decisions (BVV-DSP-04).

### Capture mechanism

Agent stdout is already redirected to a log file by `BuildShellCommand`:
```
> /path/logs/{taskID}.stdout 2>&1
```

After `runAgent` detects session death and reads the exit code, it also scans the last few lines of the stdout log for diagnostic tags:

```go
func extractDiagnosticTag(logPath string) string {
    // Read last 4KB of log file (tail, not full scan)
    f, err := os.Open(logPath)
    if err != nil { return "" }
    defer f.Close()

    info, _ := f.Stat()
    offset := max(0, info.Size()-4096)
    f.Seek(offset, io.SeekStart)
    data, _ := io.ReadAll(f)

    // Match <outcome>...</outcome> tag
    re := regexp.MustCompile(`<outcome>(.*?)</outcome>`)
    matches := re.FindSubmatch(data)
    if len(matches) >= 2 {
        return string(matches[1])
    }
    return ""
}
```

The extracted tag is stored in the Event's `Detail` field:
```go
ev := Event{
    Kind:    EventTaskCompleted, // or Failed, Blocked, etc.
    TaskID:  task.ID,
    Worker:  worker.Name,
    Summary: fmt.Sprintf("exit=%d outcome=%s", exitCode, outcome),
    Detail:  extractDiagnosticTag(logPath), // e.g. "DONE reason=\"All SCs verified\""
}
```

This is informational only. The `outcome` is derived from `exitCode`, never from the tag.

---

## Appendix E: Agent Prompt Content

In facet-scan, `BuildPrompt` constructs: "Write your output to: {path}. Input files: {list}". BVV agents don't produce output files — they commit to the branch. The prompt needs a different approach.

### Decision: Minimal initial prompt

The agent receives:
1. **System prompt** (via `SystemPromptFlag`): the instruction file content (OOMPA.md / LOOMPA.md / CHARLIE.md), stripped of YAML frontmatter. This is the "what to do" payload.
2. **Initial prompt** (via `PromptFlag` or positional argument): a minimal task context string.

```go
func BuildTaskPrompt(task *Task) string {
    return fmt.Sprintf(
        "Your assigned task ID is %s. Read your task with: bd show %s --json",
        task.ID, task.ID,
    )
}
```

This is intentionally minimal. The instruction file tells the agent HOW to work (phases, decision rules, exit codes). The beads task tells the agent WHAT to work on (title, body, success criteria, spec references). The prompt just bridges them.

### Full command construction

```go
// Instruction file → system prompt flag
args := []string{preset.SystemPromptFlag, instructionContent}

// Model override from frontmatter
if model != "" && preset.ModelFlag != "" {
    args = append(args, preset.ModelFlag, model)
}

// Initial prompt → prompt flag
taskPrompt := BuildTaskPrompt(task)
if preset.PromptFlag != "" {
    args = append(args, preset.PromptFlag, taskPrompt)
}
```

For Claude: `claude -p --verbose --system-prompt-file <tmpfile> "Your assigned task ID is build-migrations. Read your task with: bd show build-migrations --json"`

---

## Appendix F: Concurrent Lifecycle Mechanics

BVV-DSP-12 allows multiple feature lifecycles concurrently.

### Implementation: separate processes (simplest)

Each lifecycle runs as a separate `wonka run` process:
```bash
wonka run --branch feature-a --workers 2 &
wonka run --branch feature-b --workers 2 &
```

Each process:
- Acquires its own lifecycle lock: `.wonka-feature-a.lock`, `.wonka-feature-b.lock`
- Creates its own tmux socket: `wonka-{runID-a}`, `wonka-{runID-b}`
- Queries `ReadyTasks("branch:feature-a")` — sees only its tasks
- Has its own WorkerPool (workers w-01..w-02 per process)
- Has its own EventLog, GapTracker, RetryState, HandoffState

Worker pools are NOT shared across processes. Each process manages its own workers. This is the safest and simplest model — no cross-lifecycle coordination needed.

### Why not shared worker pool?

Sharing workers across lifecycles requires:
- Worker reuse semantics (WKR-07..09): cleanup workspace state between lifecycles
- Cross-lifecycle locking on worker assignment
- More complex store queries (worker tracks which lifecycle it's serving)

For v1, separate processes with separate pools. A shared-pool optimization can be added later if needed.

### Lock isolation

Per-branch lock paths guarantee mutual exclusion:
```go
lockPath := filepath.Join(runDir, fmt.Sprintf(".wonka-%s.lock", sanitizeBranch(branch)))
```

Two `wonka run --branch feature-a` commands will contend on the same lock. Two `wonka run` commands with different branches use different locks and never interfere.

---

## Appendix G: PR Gate Packaging

The PR gate (BVV-GT-01..03) needs a concrete implementation strategy.

### Decision: Shell script at `agents/gate.sh`, invoked in tmux like any agent

The gate handler is a bash script (not an AI agent). It runs in a tmux session like any other task, receives `$ORCH_TASK_ID`, and exits with 0/1.

**`agents/gate.sh`:**
```bash
#!/usr/bin/env bash
set -euo pipefail

TASK_JSON=$(bd show "$ORCH_TASK_ID" --json)
BRANCH=$(echo "$TASK_JSON" | jq -r '.labels.branch')
TARGET_BRANCH="${TARGET_BRANCH:-main}"

# BVV-GT-03: Check predecessor statuses
DEPS=$(bd deps "$ORCH_TASK_ID" --json)
FAILED=$(echo "$DEPS" | jq -r '.[] | select(.status == "failed" or .status == "blocked") | .id')
if [ -n "$FAILED" ]; then
    echo "<outcome>FAIL reason=\"predecessors failed: $FAILED\"</outcome>"
    exit 1
fi

# Create PR (BVV-GT-01: don't auto-merge)
PR_URL=$(gh pr create --base "$TARGET_BRANCH" --head "$BRANCH" \
    --title "feat($BRANCH): automated delivery" \
    --body "Created by wonka-factory BVV gate handler." 2>&1) || {
    # PR may already exist
    PR_URL=$(gh pr view "$BRANCH" --json url -q .url 2>/dev/null || echo "")
    if [ -z "$PR_URL" ]; then
        echo "<outcome>FAIL reason=\"PR creation failed\"</outcome>"
        exit 1
    fi
}

# Poll CI checks (with timeout)
TIMEOUT=1800  # 30 minutes
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    STATUS=$(gh pr checks "$BRANCH" --json 'state' -q '.[].state' 2>/dev/null || echo "PENDING")
    if echo "$STATUS" | grep -q "FAILURE"; then
        echo "<outcome>FAIL reason=\"CI checks failed\"</outcome>"
        exit 1
    fi
    if ! echo "$STATUS" | grep -q "PENDING"; then
        # All checks passed (no FAILURE, no PENDING)
        echo "<outcome>DONE reason=\"CI passed, PR created: $PR_URL\"</outcome>"
        exit 0
    fi
    sleep 30
    ELAPSED=$((ELAPSED + 30))
done

echo "<outcome>FAIL reason=\"CI check timeout after ${TIMEOUT}s\"</outcome>"
exit 1
```

### Role config wiring

The gate uses a special preset that runs bash scripts instead of AI agents:

```go
"gate": RoleConfig{
    InstructionFile: "", // no instruction file — script IS the handler
    Preset: &Preset{
        Name:    "gate",
        Command: "bash",
        Args:    []string{filepath.Join(agentDir, "gate.sh")},
        Env:     map[string]string{},
        // No SystemPromptFlag, PromptFlag, ModelFlag — not an AI agent
    },
}
```

When `SpawnSession` sees an empty `SystemPromptFlag`, it skips instruction file injection and just runs the command with the env vars (`ORCH_TASK_ID`, `ORCH_BRANCH`).

---

## Appendix H: Worker Workspace Cleanup Between Tasks

Workers are reused across tasks within the same lifecycle (WKR-07..09). When worker w-01 finishes task A and gets assigned task B, what needs cleaning?

### BVV context

In facet-scan, `ResetWorkspace(previousOutput)` deletes the previous agent's output file so the next agent starts clean. In BVV, agents don't produce output files — they commit to the branch. The "workspace" is the git repository.

### What carries over that shouldn't

1. **Handoff file** — if task A exited with code 3 (handoff), it wrote a handoff file. Task B should not see task A's handoff file.
2. **Log files** — each task gets its own log path (`LogPath(runDir, taskID)`), so these don't conflict.
3. **Git state** — agents may leave uncommitted changes (e.g., on failure). The branch may have uncommitted work.

### Cleanup in Release/pre-Spawn

```go
func (wp *WorkerPool) Release(workerName string) error {
    sessionName := SessionName(wp.runID, workerName)
    _ = wp.tmux.KillSessionIfExists(sessionName) // idempotent

    worker, err := wp.store.GetWorker(workerName)
    if err != nil { return err }

    // Cleanup handoff file from previous task
    if worker.CurrentTaskID != "" {
        handoffPath := filepath.Join(wp.repoPath, ".wonka-handoff.json")
        _ = os.Remove(handoffPath) // best-effort, may not exist
    }

    worker.Status = WorkerIdle
    worker.CurrentTaskID = ""
    worker.SessionPID = 0
    worker.SessionStartedAt = time.Time{}
    return wp.store.UpdateWorker(worker)
}
```

Git state is NOT cleaned by the orchestrator. If an agent fails mid-commit, the next agent on that branch encounters the dirty state. This is by design — the instruction file's Orient phase checks branch state and handles dirty work trees (e.g., `git stash` or `git reset`). The orchestrator is phase-agnostic (BVV-DSN-04).

---

## Appendix I: Event Log Scan Functions for Resume

The reconciliation algorithm (resume.go) says "scan event log for X events" multiple times. Here are the concrete function signatures.

### Event log JSON schema

Each line in the JSONL event log:
```json
{
  "timestamp": "2026-04-08T15:30:00Z",
  "kind": "task_retried",
  "task_id": "build-migrations",
  "worker": "w-01",
  "summary": "retry 1/2",
  "detail": "exit=1",
  "outcome": 1
}
```

### Recovery functions

```go
// recoverGaps scans the event log for EventGapRecorded events.
// Returns the gap count and list of task IDs that contributed gaps.
// Used by Resume to restore the monotonic gap counter (BVV-ERR-05).
func recoverGaps(logPath string) (count int, taskIDs []string, err error)

// recoverRetries scans the event log for EventTaskRetried events.
// Returns a map of taskID → attempt count.
// Used by Resume to restore per-task retry counts (BVV-ERR-01).
func recoverRetries(logPath string) (map[string]int, error)

// recoverHandoffs scans the event log for EventTaskHandoff events.
// Returns a map of taskID → handoff count.
// Used by Resume to restore per-task handoff counts (BVV-L-04).
func recoverHandoffs(logPath string) (map[string]int, error)

// recoverTerminalHistory scans for terminal events (task_completed, task_failed, task_blocked).
// Returns a set of taskID → last terminal status observed.
// Used by Resume for human re-open detection (BVV-S-02a):
// if a task was terminal in the log but is now open in the ledger, it was re-opened.
func recoverTerminalHistory(logPath string) (map[string]TaskStatus, error)
```

All four functions follow the same pattern (from facet-scan's `recoverGaps`):
```go
func recoverXxx(logPath string) (..., error) {
    f, err := os.Open(logPath)
    if err != nil {
        if os.IsNotExist(err) { return ..., nil } // no log → fresh state
        return ..., err
    }
    defer f.Close()

    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
        var e Event
        if err := json.Unmarshal(scanner.Bytes(), &e); err != nil { continue }
        if e.Kind == targetKind {
            // accumulate into result
        }
    }
    return ..., scanner.Err()
}
```

### ResumeResult struct

```go
type ResumeResult struct {
    Reconciled      int               // tasks whose status was corrected
    StaleReset      int               // assigned/in_progress tasks reset to open
    OrphanedKilled  int               // orphaned tmux sessions killed
    GapsRecovered   int               // gap count from event log
    GapTaskIDs      []string          // task IDs that contributed gaps
    RetryCounts     map[string]int    // taskID → retry attempt count
    HandoffCounts   map[string]int    // taskID → handoff count
    Reopened        int               // tasks detected as human-reopened
}
```

---

## Appendix J: Beads CLI Commands for Agent Instruction Files

Each agent uses beads CLI commands. Here are the exact invocations.

### All agents — task discovery (BVV-DSP-06)

```bash
# Read assigned task
bd show "$ORCH_TASK_ID" --json
# Returns: { "id": "...", "title": "...", "body": "...", "status": "...", "labels": {...}, "deps": [...] }
```

### Charlie (planner) — task graph creation

```bash
# Check existing tasks for this branch (BVV-TG-02 idempotency)
bd list --label "branch:feature-x" --json

# Create a build task with dependencies
bd create \
  --title "build-migrations" \
  --body "Target files: db/migrations/001_*.sql\nSuccess criteria:\n- Migration creates clients table\n- Rollback drops table\nSpec refs: CAP-1, technical-spec.md §Migrations" \
  --label "role:builder" \
  --label "branch:feature-x" \
  --label "critical:true" \
  --depends-on "plan-feature-x" \
  --priority 1

# Create a V&V task depending on a build task
bd create \
  --title "vv-client-crud" \
  --body "Verify: UC-1.1 through UC-1.4\nCriteria: V-1.1 handler trace, V-1.2 BR enforcement, V-1.3 error paths\nSpec refs: vv-spec.md §CAP-1" \
  --label "role:verifier" \
  --label "branch:feature-x" \
  --depends-on "build-service-handlers" \
  --priority 5

# Create PR gate task depending on all V&V tasks
bd create \
  --title "pr-gate-feature-x" \
  --label "role:gate" \
  --label "branch:feature-x" \
  --label "critical:true" \
  --depends-on "vv-client-crud" \
  --depends-on "vv-validation-rules" \
  --depends-on "vv-event-publishing" \
  --priority 999

# Check dependencies of a task
bd deps "build-migrations" --json

# Update a task body (only if status is open, per BVV-TG-02)
bd update "build-migrations" --body "Updated body text..."

# Reset a failed/blocked task to open (only if blocking condition resolved)
bd update "build-migrations" --status open
```

### Oompa (builder) — no beads writes

Oompa only reads from beads. It does NOT call `bd close` (BVV-DSP-09).

```bash
# Read task
bd show "$ORCH_TASK_ID" --json

# Read predecessor tasks (to understand context)
bd deps "$ORCH_TASK_ID" --json
bd show "predecessor-task-id" --json
```

### Loompa (verifier) — no beads writes

Same as Oompa. Reads task and predecessors, does not write to beads.

```bash
bd show "$ORCH_TASK_ID" --json
bd show "build-task-id" --json  # read the build task it verifies
```

---

## Appendix K: Test Helper Package (`orch/testutil/`)

### MockStore — in-memory Store implementation

```go
type MockStore struct {
    tasks   map[string]*Task
    workers map[string]*Worker
    deps    map[string][]string // taskID → list of dependency task IDs
    mu      sync.Mutex
}

func NewMockStore() *MockStore
// Implements all Store interface methods using in-memory maps.
// ReadyTasks: scans tasks, checks status=open, all deps terminal, assignee empty, label match.
// Assign: atomic status+assignee update with validation.
// AddDep: DFS cycle detection before adding edge.
```

### FailingStore — Store that returns errors

```go
type FailingStore struct {
    *MockStore
    FailOn map[string]error // method name → error to return
}
```

### MockPreset — test preset that runs a shell command

```go
func MockPreset() *Preset {
    return &Preset{
        Name:    "mock",
        Command: "bash",
        Args:    []string{"-c"},
        Env:     map[string]string{},
    }
}
```

### MiniLifecycle — creates a small test DAG

```go
// MiniLifecycle creates a 4-task DAG: plan → build → verify → gate
// All tasks have branch label set. Returns task IDs.
func MiniLifecycle(store Store, branch string) (plan, build, verify, gate string, err error)

// DiamondDAG creates a diamond: plan → [build-a, build-b] → verify → gate
// Tests parallel dispatch.
func DiamondDAG(store Store, branch string) (ids map[string]string, err error)

// ChainDAG creates a linear chain of N tasks with given roles.
// Tests sequential dispatch and retry behavior.
func ChainDAG(store Store, branch string, n int, role string) ([]string, error)
```

### Mock agent scripts (`testdata/mock-agents/`)

```bash
# testdata/mock-agents/exit0.sh — always succeeds
#!/bin/bash
echo "<outcome>DONE reason=\"mock success\"</outcome>"
exit 0

# testdata/mock-agents/exit1.sh — always fails
#!/bin/bash
echo "<outcome>FAIL reason=\"mock failure\"</outcome>"
exit 1

# testdata/mock-agents/exit2.sh — always blocked
#!/bin/bash
echo "<outcome>BLOCKED reason=\"mock blocked\"</outcome>"
exit 2

# testdata/mock-agents/exit3.sh — always requests handoff
#!/bin/bash
echo "<outcome>HANDOFF</outcome>"
exit 3

# testdata/mock-agents/exit-from-env.sh — exit code from env var
#!/bin/bash
echo "<outcome>DONE reason=\"mock agent exit=${MOCK_EXIT_CODE}\"</outcome>"
exit "${MOCK_EXIT_CODE:-0}"

# testdata/mock-agents/slow.sh — runs for configurable duration
#!/bin/bash
sleep "${MOCK_DURATION:-60}"
exit 0
```

---

## Appendix L: Requirement Traceability Matrix

Every BVV requirement → the Go file and function that implements it.

### Design Principles (BVV-DSN-01..04)

| Req | File | Function/Mechanism | Notes |
|-----|------|--------------------|-------|
| BVV-DSN-01 | dispatch.go | `Tick()` step 2 (DISPATCH) | ReadyTasks returns all tasks with satisfied deps; no phase logic |
| BVV-DSN-02 | dispatch.go | `runAgent()` | One goroutine per task; session ends on exit |
| BVV-DSN-03 | session.go / agent.go | `SpawnSession`, `BuildEnv` | Orchestrator manages handoff files; never reads PROGRESS.md |
| BVV-DSN-04 | dispatch.go | `Tick()` step 2, role lookup | Dispatch branches on `role` label, never on role semantics |

### Agent Identity (BVV-AI-01..03)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-AI-01 | session.go | `SpawnSession` → `agentPromptArgs` | Injects instruction file via SystemPromptFlag; never modifies content |
| BVV-AI-02 | types.go | `RoleConfig` struct | Each role maps to exactly one instruction file; gate exception handled by script preset |
| BVV-AI-03 | types.go / cmd | `LifecycleConfig.Roles`, `--agent` flag | Multiple presets per role via config; CLI flag selects |

### Task Graph (BVV-TG-01..12)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-TG-01 | dispatch.go | `Tick()` dispatch step | Orchestrator never calls CreateTask except for escalations |
| BVV-TG-02 | agents/CHARLIE.md | Planner Orient+Graph phases | Queries existing tasks before creating; reconciles |
| BVV-TG-03 | agents/CHARLIE.md | Graph phase | Never modifies in_progress/completed |
| BVV-TG-04 | agents/CHARLIE.md | Graph phase | Each task body references spec sections |
| BVV-TG-05 | — (external) | Human/CLI action | Plan task created by `bd create`, not orchestrator |
| BVV-TG-06 | agents/CHARLIE.md | Orient phase | Planner creates branch if needed |
| BVV-TG-07 | agents/CHARLIE.md | Validate phase | Every task has valid role tag |
| BVV-TG-08 | ledger.go | `AddDep` | Cycle rejection via DFS |
| BVV-TG-09 | agents/CHARLIE.md | Validate phase | Exactly one gate task per lifecycle |
| BVV-TG-10 | agents/CHARLIE.md | Validate phase | All tasks reachable from plan task |
| BVV-TG-11 | agents/CHARLIE.md + dispatch.go | Planner retry + reconciliation | Partial graph survives; re-run reconciles |
| BVV-TG-12 | — | N/A | Branch NOT auto-deleted on terminal failure |

### Dispatch (BVV-DSP-01..16)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-DSP-01 | dispatch.go | `Tick()` dispatch loop | Assigns all ready tasks up to MaxWorkers |
| BVV-DSP-02 | dispatch.go | `Tick()` dispatch loop | No holding; only constraint is worker availability |
| BVV-DSP-03 | dispatch.go | role lookup | Routing by `task.Role()` label only |
| BVV-DSP-03a | dispatch.go | role lookup failure | Creates escalation task, sets task to blocked |
| BVV-DSP-04 | agent.go | `DetermineOutcome` | Pure exit code switch; no output parsing |
| BVV-DSP-05 | session.go | `SpawnSession` | Fresh session per task; Release between tasks |
| BVV-DSP-06 | agent.go | `BuildEnv` | Sets ORCH_TASK_ID in env |
| BVV-DSP-07 | agents/CHARLIE.md | Graph phase | Planner adds dep edges between overlapping tasks |
| BVV-DSP-08 | dispatch.go | `ReadyTasks(branchLabel)` | Label-filtered query |
| BVV-DSP-09 | dispatch.go | `processOutcome` | Orchestrator writes terminal status; agent writes are idempotent |
| BVV-DSP-10 | agents/CHARLIE.md | Graph phase | Planner serializes V&V tasks on overlapping files |
| BVV-DSP-11 | agents/LOOMPA.md | Verify+Fix phase | Agent handles git conflicts internally |
| BVV-DSP-12 | engine.go + lock.go | Per-branch lock | Separate processes with separate locks |
| BVV-DSP-13 | — | Not implemented in v1 | Worktree isolation is advanced mode |
| BVV-DSP-14 | dispatch.go | `processOutcome` Handoff case | Task stays in_progress; session respawned |
| BVV-DSP-15 | dispatch.go | `Tick()` dispatch step + `Assign` | Orchestrator selects and assigns tasks |
| BVV-DSP-16 | ledger_beads.go | `BeadsStore` | Beads is default store |

### Gate (BVV-GT-01..03)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-GT-01 | agents/gate.sh | No `gh pr merge` call | PR created but NOT auto-merged |
| BVV-GT-02 | engine.go | Separate engine per lifecycle | Gate failure in one lifecycle doesn't block others |
| BVV-GT-03 | agents/gate.sh | Predecessor check | Checks dep statuses before creating PR |

### Session (BVV-SS-01)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-SS-01 | dispatch.go / agent.go | — (negative requirement) | Orchestrator never reads PROGRESS.md or agent output |

### Error Semantics (BVV-ERR-01..11a)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-ERR-01 | recovery.go | `RetryState.RecordAttempt` | Monotonic; recovered from event log |
| BVV-ERR-02 | recovery.go | `ScaledTimeout` | 1.5x base per attempt |
| BVV-ERR-02a | dispatch.go | `runAgent` timer | Kill session on timeout; treat as exit 1 |
| BVV-ERR-03 | dispatch.go | `handleTerminalFailure` | Critical → immediate abort |
| BVV-ERR-04 | dispatch.go | `handleTerminalFailure` → `GapTracker` | Gap count ≥ tolerance → abort |
| BVV-ERR-04a | dispatch.go | `abortCleanup` | All open tasks → blocked |
| BVV-ERR-05 | recovery.go | `GapTracker.IncrementAndCheck` | Monotonic; recovered from event log |
| BVV-ERR-06 | lock.go | `Acquire` staleness check | Stale lock → reclaim |
| BVV-ERR-07 | resume.go | `Reconcile` step 1 | No dispatch during reconciliation |
| BVV-ERR-08 | resume.go | `Reconcile` step 1 | Live in_progress sessions not reset |
| BVV-ERR-09 | signal.go | `SetupSignalHandler` | Shutdown does not modify task statuses |
| BVV-ERR-10 | signal.go / engine.go | `Cleanup` | Lock released on all exit paths |
| BVV-ERR-10a | lock.go / engine.go | Release precondition | Lock released only when all sessions drained |
| BVV-ERR-11 | watchdog.go | `CheckOnce` | `tmux has-session` for liveness |
| BVV-ERR-11a | watchdog.go | `CheckOnce` restart path | Restart emits task_handoff; checks handoff limit |

### Safety Properties (BVV-S-01..10)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-S-01 | lock.go | `Acquire` (O_CREATE\|O_EXCL) | At most one orchestrator per branch |
| BVV-S-02 | dispatch.go / invariant.go | `processOutcome` + `AssertTerminalIrreversibility` | Never reverses terminal through orchestrator action |
| BVV-S-02a | resume.go | `Reconcile` step 6 | Detects terminal→open; resets counters |
| BVV-S-03 | ledger.go / invariant.go | `Assign` atomicity + `AssertSingleAssignment` | At most one worker per task |
| BVV-S-04 | ledger.go / invariant.go | `ReadyTasks` dep check + `AssertDependencyOrdering` | No dispatch before deps terminal |
| BVV-S-05 | dispatch.go / invariant.go | Role-only routing + `AssertZeroContentInspection` | Never reads task body for dispatch |
| BVV-S-06 | agents/gate.sh | Gate exits 1 on failure | Failed gate blocks merge |
| BVV-S-07 | dispatch.go / invariant.go | Gap check in gate + `AssertBoundedDegradation` | No PR if gaps ≥ tolerance |
| BVV-S-08 | ledger.go | Store durability | Assignment persists through crashes |
| BVV-S-09 | agents/CHARLIE.md | Dep edges serialize builds | At most one build writes at a time |
| BVV-S-10 | watchdog.go / invariant.go | Watchdog restart ≠ retry + `AssertWatchdogNoStatusChange` | Orthogonal operations |

### Liveness Properties (BVV-L-01..04)

| Req | File | Function | Notes |
|-----|------|----------|-------|
| BVV-L-01 | dispatch.go | Termination check in `Tick` | Finite graph × finite budgets → all terminal |
| BVV-L-02 | lock.go | Staleness threshold | Dead orchestrator's lock recoverable |
| BVV-L-03 | watchdog.go | `CheckOnce` + CircuitBreaker | Dead sessions restarted; cascading restarts prevented |
| BVV-L-04 | recovery.go / dispatch.go | `HandoffState` + handoff→failure conversion | Bounded handoffs per task |
