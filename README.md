# wonka-factory

DAG-driven task dispatch for autonomous software delivery agents.

[![CI](https://github.com/endgame-build/wonka-factory/actions/workflows/ci.yml/badge.svg)](https://github.com/endgame-build/wonka-factory/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/go-1.25-00ADD8)
![Status](https://img.shields.io/badge/status-working%20draft-orange)

## Overview

Wonka orchestrates **Oompa** (builder), **Loompa** (verifier), and **Charlie** (planner) through a durable assignment ledger. Charlie decomposes work packages into a task graph in [beads](https://github.com/steveyegge/beads); dispatch walks the DAG, assigning ready tasks to idle workers. Dependency edges drive lifecycle ordering — the orchestrator has no phase logic.

Each session runs exactly one task. Agents signal outcome through exit codes (`0` done, `1` retryable fail, `2` terminal block, `3` handoff). The orchestrator never reads agent output: routing uses role metadata, outcome uses exit codes, diagnostics go to the audit trail.

## Why this exists

| Property | How |
|----------|-----|
| **Spec-driven** | Every function traces to a requirement ID in [`docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md`](docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md) (70 normative requirements). |
| **Formally verified** | TLA+ encodes 52 of 70 requirements as safety invariants and liveness properties; TLC model-checks them before Go code lands. |
| **Crash-recoverable** | Per-branch lifecycle locks detect staleness; `wonka resume` replays the event log to rebuild state. |
| **Gap-tolerant** | Non-critical failures accumulate against a threshold; critical failures abort immediately. |
| **Audited** | Append-only JSONL trail (19 event kinds) at `<runDir>/events.jsonl` (default: `.wonka/<branch>/events.jsonl`) — every assignment, exit, and lifecycle transition. |

## Quickstart

**Prerequisites:** Go 1.25+, `tmux`, [Task](https://taskfile.dev), optional: Docker (for observability), `java` (for TLA+).

```bash
# Build
task build

# Full local quality gate (lint + test + build)
task check

# Start a lifecycle on a branch
bin/wonka run --branch feat/my-change

# Resume an interrupted lifecycle
bin/wonka resume --branch feat/my-change

# Inspect state
bin/wonka status --branch feat/my-change
```

CLI exit codes: `1` runtime error, `2` config error, `3` lock corrupt (needs human), `4` lock busy (safe to retry), `130` SIGINT.

## How it works

```
  work package
       │
       ▼
  ┌─────────┐   decomposes into task graph (beads ledger)
  │ CHARLIE │ ──────────────────────────────┐
  └─────────┘                               ▼
                            ┌────────────────────────────┐
                            │   ReadyTasks(branch)       │
                            │   (status=open, deps done) │
                            └────────────┬───────────────┘
                                         │ assign
                         ┌───────────────┴────────────────┐
                         ▼                                ▼
                   ┌─────────┐                      ┌─────────┐
                   │  OOMPA  │  writes code, tests  │ LOOMPA  │  traces spec,
                   │ builder │  commits             │verifier │  fixes defects
                   └────┬────┘                      └────┬────┘
                        │ exit 0/1/2/3                   │ exit 0/1/2/3
                        └────────────┬───────────────────┘
                                     ▼
                              PR gate → CI → merge
```

Each task runs in an isolated tmux session (socket `wonka-<runID>`, session name `<runID>-<workerName>`). The watchdog tracks liveness via `tmux has-session`, not heartbeat writes. Three consecutive sessions under 60 seconds trip the circuit breaker — the watchdog still restarts the session, and the dispatcher decides whether to halt.

## Commands

| Subcommand     | Purpose                                                         |
|----------------|-----------------------------------------------------------------|
| `wonka run`    | Start a fresh lifecycle on a branch (acquires per-branch lock). |
| `wonka resume` | Re-enter an interrupted lifecycle (reconciles stale state).     |
| `wonka status` | Print tasks for the branch (table; `--json` for scripts).       |

## Formal verification

The TLA+ model in [`docs/specs/tla/`](docs/specs/tla/) encodes 52 BVV requirements. Check them in four escalating configurations:

```bash
brew install tlaplus  # or download tla2tools.jar

java -jar tla2tools.jar -config smoke.cfg BVV.tla       # seconds
java -jar tla2tools.jar -config small.cfg BVV.tla       # minutes
java -jar tla2tools.jar -config lifecycle.cfg BVV.tla   # hours
java -jar tla2tools.jar -config full.cfg -workers 8 BVV.tla  # overnight
```

When TLC finds a violation, classify it: **spec bug** (fix prose spec + model), **model bug** (fix TLA+ only), or **expected violation** (document).

## Observability

Optional stack (OTel collector, Prometheus, Grafana) in `docker-compose.yaml`:

```bash
docker compose up -d
bin/wonka run --branch feat/x --otel-endpoint localhost:14317 --otel-insecure
```

- **Grafana** — http://localhost:3000 (admin/changeme), Telemetry folder
- **Prometheus** — http://localhost:9090 (90-day retention)
- **OTel collector** — OTLP gRPC on `:14317`, HTTP on `:14318`

Telemetry is off by default. `--otel-insecure` works only for loopback endpoints; `BuildTelemetry` rejects cleartext to remote collectors.

## Project layout

```
├── cmd/wonka/              # CLI entry point (thin main)
├── internal/cmd/           # Cobra subcommands
├── orch/                   # Reusable orchestrator library (DAG dispatch)
│   ├── dispatch.go         #   BVV-DSP-01..02
│   ├── engine.go           #   Run() / Resume()
│   ├── ledger_beads.go     #   LDG-01..19 (beads backend)
│   ├── recovery.go         #   retry, gap tracking, abort cleanup
│   └── ...                 #   see CLAUDE.md for full file map
├── agents/                 # OOMPA.md, LOOMPA.md, CHARLIE.md — agent prompts
├── docs/
│   ├── BVV_IMPLEMENTATION_PLAN.md
│   ├── BVV_VV_STRATEGY.md
│   └── specs/              # BVV spec + TLA+ model
└── config/                 # OTel collector, Prometheus, Grafana provisioning
```

## Developing

- **Claude Code users** — [`CLAUDE.md`](CLAUDE.md) is the execution-focused working reference (commands, conventions, design decisions).
- **Specification** — [`docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md`](docs/specs/BUILD_VERIFY_VALIDATE_SPEC.md) is the single source of truth. Every PR traces changes to requirement IDs.
- **Implementation plan** — [`docs/BVV_IMPLEMENTATION_PLAN.md`](docs/BVV_IMPLEMENTATION_PLAN.md) tracks phase rollout.
- **Contributing** — Conventional commits (`feat`, `fix`, `refactor`, `docs`, `chore`, `test`, `ci`, `build`, `perf`). CI lints PR titles against this list. Run `scripts/trace-requirement.sh <BVV-ID>` to find spec/test/code references.

## Status

Working draft. Phases 1–10 of the implementation plan have shipped, including observability. The library, CLI, and spec (1.0.0-draft) remain under active development. Production requires a Beads/Dolt backend; the FS backend exists for local development.

## License

TBD.
