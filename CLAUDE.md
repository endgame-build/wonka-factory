# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

DAG-driven task dispatch for builder (Oompa), verifier (Loompa), and planner (Charlie) agents. Lifecycle ordering emerges from dependency edges in the task graph — the orchestrator has no phase logic.

Two deliverables in one repo:
- `orch/` — domain-agnostic orchestrator library
- `cmd/wonka/` — CLI wiring `orch/` to the builder/verifier/planner lifecycle

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

# Tier 3 integration tests (real tmux, fault injection; build tag `integration`)
task test-integration  # or: go test -race -tags "verify integration" -count=1 ./orch/...

# Lint
task lint              # or: golangci-lint run --build-tags=verify --timeout=5m

# Coverage (profile + per-function report)
task coverage          # or: go test -race -tags verify -coverprofile=coverage.out ./orch/... ./internal/... && go tool cover -func=coverage.out
```

The `-tags verify` build tag enables runtime invariant assertions that panic with requirement IDs (e.g., `[BVV-DSP-01]`, `[BVV-S-03]`). CI always runs with this tag.

## CLI Surface

`cmd/wonka/` is a thin `main`; all subcommands live in `internal/cmd/` (Cobra):

| Subcommand     | Purpose                                                         |
|----------------|-----------------------------------------------------------------|
| `wonka run`    | Start a fresh lifecycle on a branch (acquires per-branch lock). |
| `wonka resume` | Re-enter an interrupted lifecycle (reconciles stale state).     |
| `wonka status` | Print tasks for the branch (table; `--json` for scripts).       |

CLI-level exit codes (distinct from the agent exit-code protocol described below):
`1` runtime error, `2` config error, `3` lock corrupt, `4` lock busy, `130` SIGINT.
Wrapper scripts should branch on 3/4 (wait-and-retry vs human intervention).

## Local observability stack

`docker-compose.yaml` spins up OTel collector + Prometheus + Grafana. Metrics from `wonka run` reach Grafana when `--otel-endpoint` is set:

```bash
docker compose up -d                                      # stack on localhost
bin/wonka run --branch feat/x --otel-endpoint localhost:14317 --otel-insecure
```

- Grafana: http://localhost:3000 (admin/changeme) — "Telemetry" folder has `Wonka Orchestrator` and `Claude Code Telemetry` dashboards.
- Prometheus: http://localhost:9090 — 90-day retention.
- OTel collector: OTLP gRPC on `localhost:14317`, HTTP on `localhost:14318` (remapped from 4317/4318 to avoid conflicts with a host jaeger).

**Traces.** Per-task and per-lifecycle spans export only to the collector's `debug` stdout exporter — no Tempo/Jaeger backend, no Grafana trace datasource. View spans via `docker compose logs otel-collector`.

**Default off.** `--otel-endpoint` defaults to empty; no network I/O unless set.

**Transport security.** `--otel-insecure` defaults to `false` (TLS required). The local docker-compose stack ships without TLS, so pass `--otel-insecure` explicitly. `BuildTelemetry` rejects `--otel-insecure` paired with a non-loopback endpoint — otherwise it would leak branch names, task IDs, and error text in cleartext.

## Continuous Integration

GitHub Actions workflows in `.github/workflows/`:

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yml` | PR, push to main | 5 jobs: secret-scan (gitleaks), commit-lint (semantic PR titles), go-quality (lint + test + build + coverage + govulncheck), property-tests (rapid 10k iterations), integration (tmux fault injection) |
| `release.yml` | Tag `v*.*.*` or manual dispatch | GoReleaser cross-platform build (linux/darwin × amd64/arm64), draft release |
| `auto-release.yml` | Successful CI run on main | svu-based semver detection, tag creation, dispatches `release.yml` |

`.goreleaser.yaml` configures the release build. `.github/dependabot.yml` tracks gomod + github-actions updates weekly.

Third-party actions are pinned to 40-char SHAs. External binaries (gitleaks, svu) are installed via checksum-verified tarballs. Conventional commit types allowed: `feat`, `fix`, `refactor`, `docs`, `chore`, `test`, `ci`, `build`, `perf` — `feat`/`fix` drive svu bumps; the rest patch-bump via auto-release logic.

## Architecture

### orch/ package (the orchestrator library)

| File | Purpose |
|------|---------|
| `types.go` | Task, Worker, Preset, typed enums (TaskStatus, Criticality, WorkerStatus, Model, LedgerKind), label key constants, LockConfig, LifecycleConfig |
| `ledger_beads.go` | Beads/Dolt Store implementation (default) |
| `dispatch.go` | DAG-driven dispatch loop — query ready tasks, assign to idle workers |
| `agent.go` | Role-to-instruction-file routing, exit-code-based outcome |
| `engine.go` | Top-level: `Engine.Run()` (fresh) and `Engine.Resume()` (interrupted) |
| `session.go` | WorkerPool lifecycle (Allocate/Spawn/Release) |
| `tmux.go` | Socket-isolated tmux wrapper |
| `lock.go` | Per-branch lifecycle lock with staleness detection |
| `recovery.go` | RetryState (exit code 1), GapTracker, abort cleanup, handoff counter |
| `resume.go` | State reconciliation: stale assignments, orphan cleanup, counter recovery |
| `gate.go` | PR gate: create PR, poll CI, exit code protocol |
| `watchdog.go` | Tmux liveness detection + circuit breaker |
| `eventlog.go` | Append-only JSONL audit trail (19 event kinds) |
| `invariant.go` | Runtime assertions (build tag `verify`) |
| `validate.go` | Post-planner task-graph well-formedness check |
| `signal.go` | Graceful shutdown (SIGINT/SIGTERM), no status modification |
| `telemetry.go` | OTel metrics + spans (nil-safe). Attached via `EventLog.WithTelemetry`. |

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

The orchestrator injects these files as system prompts. Never modify their content.

### Task status enum

```
{open, assigned, in_progress, completed, failed, blocked}
Terminal: {completed, failed, blocked}
```

Valid transitions:
- `open → assigned → in_progress → completed|failed|blocked`
- `in_progress → open` (exit 1 with retries remaining)
- `in_progress → in_progress` (exit 3 handoff, atomic session respawn)
- `terminal → open` (human re-open only, resets retry+handoff counters)

### Test structure

Four test categories in `orch/`:
- **`*_spec_test.go`** — Spec-style tests named after the assertion they cover (e.g., `TestBVV_DSP01`, `TestBVV_S03`)
- **`*_prop_test.go`** — Property-based tests with random task graphs using `pgregory.net/rapid`
- **`ledger_contract_test.go`** — Store contract suite run against both Beads and FS implementations
- **`engine_e2e_test.go`** — Integration tests with real tmux + mock agent scripts (build tag `integration`)

## Key Design Decisions

- **Store factory** (`NewStore(kind, dir) → (Store, LedgerKind, error)`): Defaults to `"beads"`, falls back to `"fs"` when Beads/Dolt unavailable. Returns the actual backend kind so callers can detect fallback. CLI flag `--ledger` overrides.
- **Beads status mapping**: `assigned` → beads `open` + non-empty `Assignee` field (derived on read-back). `blocked` → beads `blocked` (native). `failed` → beads `closed` + `orch:failed` label. Orch fields stored as `orch:` prefixed labels.
- **Atomic writes**: All JSON writes via tmp+rename pattern.
- **Cycle detection**: `AddDep` runs DFS reachability check before adding any edge.
- **Per-branch lifecycle lock**: `O_CREATE|O_EXCL` semantics, staleness-based recovery, lock path scoped by branch name.
- **Runtime state at `.wonka/<branch>/`**: Event log (`events.jsonl`), ledger store (`ledger/`), and lifecycle lock (`.wonka-<branch>.lock`). Run dir defaults to `<repo>/.wonka/<sanitized-branch>/` (override with `--run-dir`). Gap state is not persisted as a file — `GapTracker` is recreated per run, and resume replays `gap_recorded` events to rebuild it. Safe to delete only when no lifecycle is active.
- **Worker lifecycle**: validate → side-effect (tmux) → persist. `SpawnSession` uses defer-flag pattern for rollback.
- **Idempotent cleanup**: Cleanup operations suppress "not found" errors. Use `KillSessionIfExists`.
- **Tmux socket isolation**: Socket `"wonka-{runID}"`, session names `{runID}-{workerName}`.
- **Exit code protocol replaces command interface**: No `prime`/`done`/`fail`/`heartbeat` commands. Agent reads `$ORCH_TASK_ID`, executes, exits with 0/1/2/3.
- **Watchdog uses tmux presence, not heartbeats**: `tmux has-session -t <name>` replaces heartbeat writes.
- **Gap tolerance**: `GapTracker` recovered from event log by scanning `gap_recorded` events.
- **Circuit breaker**: 3 consecutive rapid failures (session < 60s) trip the CB — watchdog still restarts; the dispatcher reads `CBTripped()` to decide whether to halt.
- **Handoff counter monotonic within lifecycle**: Not reset on retry (exit 1). Reset only on human re-open.

## Conventions

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
