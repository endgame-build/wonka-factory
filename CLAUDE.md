# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

wonka-factory is a DAG-driven task dispatch system for autonomous software delivery agents. It coordinates builder agents (Oompa), verifier agents (Loompa), and a planning agent (Charlie) through a Wonka orchestrator that manages worker sessions, a durable assignment ledger, and process supervision.

Two deliverables share one repo:

- **`orch/`** — Reusable, domain-agnostic orchestrator library (forked from `github.com/endgame/facet-scan/orch`, simplified for DAG dispatch)
- **`cmd/wonka/`** — CLI binary that wires `orch/` to BVV lifecycle dispatch

The orchestrator replaces phase-driven pipeline execution with DAG-driven dispatch: lifecycle ordering emerges from dependency edges in the task graph, not from orchestrator logic.

## Specifications

| Document | Purpose |
|----------|---------|
| `docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md` | Primary spec — 70 normative requirements (BVV-*) |
| `docs/specs/BVV_VALIDATION_REPORT.md` | Prose validation: state matrix, deadlock analysis, termination proof |
| `docs/specs/MULTI_AGENT_PIPELINE_ORCHESTRATION_SPEC.md` | Infrastructure layer — BVV reuses Sections 4-11a (LDG-*, WKR-*, SUP-*, RCV-*, CTY-*) |

## Formal Verification (TLA+)

TLA+ model in `docs/specs/tla/` encodes 52 of 70 BVV requirements as mechanically verifiable invariants and temporal properties. TLC model checking precedes Go implementation.

```bash
# Install TLA+ tooling
brew install tlaplus  # or download tla2tools.jar

# Run model checking (smoke → small → lifecycle → full)
java -jar tla2tools.jar -config smoke.cfg BVV.tla       # seconds
java -jar tla2tools.jar -config small.cfg BVV.tla       # minutes
java -jar tla2tools.jar -config lifecycle.cfg BVV.tla   # hours
java -jar tla2tools.jar -config full.cfg -workers 8 BVV.tla  # overnight
```

| Module | Purpose |
|--------|---------|
| `BVVTypes.tla` | Constants, status sets, graph helpers (ReachableFrom, IsAcyclic) |
| `BVVTaskMachine.tla` | 8 task actions: Assign, SessionStart, 4 exit codes, handoff, handoff-limit |
| `BVVDispatch.tla` | 9 system actions: timeout, crash, watchdog, human reopen, reconcile, locks |
| `BVVLifecycle.tla` | Dynamic planner task creation, concurrent lifecycle support |
| `BVV.tla` | Top-level: Init, Next, Spec, 11 safety invariants, 4 liveness properties |

When TLC finds a violation, classify it: **spec bug** (fix prose spec + model), **model bug** (fix TLA+ only), or **expected violation** (document as known result).

## Build and Test

```bash
# Full local quality gate
task check

# Build
task build             # or: CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/wonka ./cmd/wonka

# Run all tests
task test              # or: go test -race -tags verify -count=1 ./orch/... ./internal/...

# Run a single test
go test -race -tags verify -run TestBVV_DSP01 ./orch/...

# Property-based tests
task test-prop         # or: RAPID_CHECKS=10000 go test -race -tags verify -run Prop ./orch/...

# Lint
task lint              # or: golangci-lint run --build-tags=verify --timeout=5m
```

The `-tags verify` build tag enables runtime invariant assertions that panic with requirement IDs (e.g., `[BVV-DSP-01]`, `[BVV-S-03]`). CI always runs with this tag.

## Architecture

### Spec-driven design

Every function traces to a requirement ID from the BVV spec. Test names reference these IDs (e.g., `TestBVV_DSP01_DispatchAllReady`, `TestBVV_S03_SingleAssignment`). TLA+ findings inform implementation — comments reference TLC counterexamples where applicable.

### orch/ package (the orchestrator library)

Forked from `facet-scan/orch` and simplified per BVV Appendix B:

| File | Purpose | Key requirement IDs |
|------|---------|-------------------|
| `types.go` | Task, Worker, Preset, typed enums (TaskStatus, Criticality, WorkerStatus, Model, LedgerKind), label key constants, LockConfig, LifecycleConfig | — |
| `ledger_beads.go` | Beads/Dolt Store implementation (default) | LDG-01..19, BVV-DSP-16 |
| `dispatch.go` | DAG-driven dispatch loop — query ready tasks, assign to idle workers | BVV-DSP-01..02, BVV-DSP-08 |
| `agent.go` | Role-to-instruction-file routing, exit-code-based outcome | BVV-AI-02, BVV-DSP-03..04 |
| `engine.go` | Top-level: `Engine.Run()` (fresh) and `Engine.Resume()` (interrupted) | BVV-ERR-06..08 |
| `session.go` | WorkerPool lifecycle (Allocate/Spawn/Release) | WKR-04..12 |
| `tmux.go` | Socket-isolated tmux wrapper | — |
| `lock.go` | Per-branch lifecycle lock with staleness detection | BVV-S-01, BVV-ERR-06, BVV-ERR-10a, BVV-L-02 |
| `recovery.go` | RetryState (exit code 1), GapTracker (BVV-ERR-03..05), abort cleanup, handoff counter | BVV-ERR-01..05, BVV-ERR-04a |
| `resume.go` | State reconciliation: stale assignments, orphan cleanup, counter recovery | BVV-ERR-07..08 |
| `gate.go` | PR gate: create PR, poll CI, exit code protocol | BVV-GT-01..03 |
| `watchdog.go` | Tmux liveness detection + circuit breaker | BVV-ERR-11..11a, SUP-05..06 |
| `eventlog.go` | Append-only JSONL audit trail (16 event kinds) | BVV-SS, Section 10.3 |
| `invariant.go` | Runtime assertions (build tag `verify`) | BVV-S-01..10 |
| `signal.go` | Graceful shutdown (SIGINT/SIGTERM), no status modification | BVV-ERR-09..10a |

### What was removed from facet-scan/orch

BVV replaces phase-driven execution with DAG dispatch. These types and functions are not needed:

- `Pipeline`, `Phase`, `ConsensusConfig`, `QualityGate` types
- `Expand()` function — task graphs come from the planning agent via Beads, not from Go structs
- Phase advancement logic in the dispatch loop
- Consensus protocol (instances → merge → verify)
- File-based output validation in `DetermineOutcome` — replaced by exit codes

### Key concepts

| Concept | Implementation |
|---------|---------------|
| **DAG dispatch** | `ReadyTasks()` returns tasks where status=open, all deps terminal, assignee empty. No phase logic. |
| **Exit code protocol** | 0=done, 1=fail(retryable), 2=blocked(terminal), 3=handoff(new session) |
| **One task per session** | Each `Assign` → `SpawnSession` → agent runs → exits → session ends. Orchestrator is the outer loop. |
| **Role routing** | Task's `role` label → instruction file path → injected as system prompt via preset's `SystemPromptFlag` |
| **Lifecycle scoping** | `ReadyTasks(branch)` filters by branch label. Per-branch locks, gap counters, abort flags. |
| **Gap tolerance** | Non-critical failures increment gap counter. At threshold, lifecycle aborts. Critical failures abort immediately. |
| **Terminal irreversibility** | Orchestrator never reverses completed/failed/blocked. Only human CLI intervention re-opens tasks. |

### Agent instruction files

| File | Role | Description |
|------|------|-------------|
| `agents/OOMPA.md` | Builder | Writes code, tests, migrations, commits |
| `agents/LOOMPA.md` | Verifier | Traces code against specs, fixes defects, commits |
| `agents/CHARLIE.md` | Planner | Decomposes work packages into task graphs in beads |

Instruction files define agent identity: phases, decision rules, operating rules, completion protocol, memory format. The orchestrator injects them as system prompts and never modifies their content.

### Task status enum

```
{open, assigned, in_progress, completed, failed, blocked}
Terminal: {completed, failed, blocked}
```

Valid transitions (from BVV validation report Section 1):
- `open → assigned → in_progress → completed|failed|blocked`
- `in_progress → open` (exit 1 with retries remaining)
- `in_progress → in_progress` (exit 3 handoff, atomic session respawn)
- `terminal → open` (human re-open only, resets retry+handoff counters)

### Test structure

Four test categories in `orch/`:
- **`*_spec_test.go`** — One test per BVV requirement ID (e.g., `TestBVV_DSP01`, `TestBVV_S03`)
- **`*_prop_test.go`** — Property-based tests with random task graphs using `pgregory.net/rapid`
- **`ledger_contract_test.go`** — Store contract suite run against both Beads and FS implementations
- **`engine_e2e_test.go`** — Integration tests with real tmux + mock agent scripts (build tag `integration`)

## Key Design Decisions

- **Store factory** (`NewStore(kind, dir)`): Defaults to `"beads"`, falls back to `"fs"` when Beads/Dolt unavailable. CLI flag `--ledger` overrides.
- **Beads status mapping**: `assigned` → beads `open` + `orch:assigned` label. `blocked` → beads `blocked` (new BVV status). Orch fields stored as `orch:` prefixed labels.
- **Atomic writes**: All JSON writes via tmp+rename pattern.
- **Cycle detection**: `AddDep` runs DFS reachability check before adding any edge.
- **Per-branch lifecycle lock**: `O_CREATE|O_EXCL` semantics, staleness-based recovery, lock path scoped by branch name.
- **Worker lifecycle**: validate → side-effect (tmux) → persist. `SpawnSession` uses defer-flag pattern for rollback.
- **Idempotent cleanup**: Cleanup operations suppress "not found" errors. Use `KillSessionIfExists`.
- **Tmux socket isolation**: Socket `"wonka-{runID}"`, session names `{runID}-{workerName}`.
- **Exit code protocol replaces command interface**: No `prime`/`done`/`fail`/`heartbeat` commands. Agent reads `$ORCH_TASK_ID`, executes, exits with 0/1/2/3.
- **Watchdog uses tmux presence, not heartbeats**: `tmux has-session -t <name>` replaces heartbeat writes.
- **Gap tolerance**: `GapTracker` recovered from event log by scanning `gap_recorded` events.
- **Circuit breaker**: 3 consecutive rapid failures (session < 60s) suspends worker.
- **Handoff counter monotonic within lifecycle**: Not reset on retry (exit 1). Reset only on human re-open.

## Conventions

- Comments and test names reference BVV requirement IDs as canonical spec references
- `errors.go` defines sentinel errors in 3 groups — match with `errors.Is()`
- Error wrapping: `%w` for sentinel, `%v` for diagnostic context. Prefix with operation name.
- `testify/require` for preconditions, `testify/assert` for assertions
- Property-based tests use `rapid.Check` with `*rapid.T`
- **ZFC principle**: The orchestrator never reads agent output content. Routing uses role metadata. Outcome uses exit codes. Diagnostic tags go to audit trail only.

## Git Conventions

Use conventional commits:

```
feat(orch): add per-branch lifecycle lock
fix(dispatch): prevent dispatch during reconciliation
test(orch): add BVV-S-03 single assignment property test
```
