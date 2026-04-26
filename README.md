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
| **Crash-recoverable** | Per-branch lifecycle locks detect staleness; `wonka resume` replays the event log to rebuild state. |
| **Gap-tolerant** | Non-critical failures accumulate against a threshold; critical failures abort immediately. |
| **Audited** | Append-only JSONL trail (19 event kinds) at `<runDir>/events.jsonl` (default: `.wonka/<branch>/events.jsonl`) — every assignment, exit, and lifecycle transition. |

## Quickstart

**Prerequisites:** Go 1.25+, `tmux`, [Task](https://taskfile.dev), optional: Docker (for observability).

```bash
# Build
task build

# Full local quality gate (lint + test + build)
task check

# Start a lifecycle on a branch (positional = work-package directory)
bin/wonka run --branch feat/my-change work-packages/my-change/

# Resume an interrupted lifecycle (work-package read from existing planner task)
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
| `wonka run`    | Start a fresh lifecycle on a branch (acquires per-branch lock). Takes a `<work-package>` positional argument. |
| `wonka resume` | Re-enter an interrupted lifecycle (reconciles stale state). Reads the work-package from existing state. |
| `wonka status` | Print tasks for the branch (table; `--json` for scripts).       |

## Work package

A work package is a directory with two Markdown files:

```
work-packages/<feature>/
  functional-spec.md    # WHAT — capabilities (CAP-*), use cases (UC-*), acceptance criteria (AC-*)
  vv-spec.md            # PROOF — verification criteria per capability (V-*), test approach
```

Architectural context (layering, tech stack, conventions) lives in the target repo's `CLAUDE.md`, not in any per-feature spec. Charlie reads both work-package files plus `CLAUDE.md` during ORIENT and decomposes the result into a build/V&V/gate task graph.

`wonka run` hashes the two spec files and stores the digest on the seeded planner task. Re-running with the same content is a no-op; editing either file and re-running reopens the planner so the graph reconciles against the new spec.

## Observability

Optional stack (OTel collector, Prometheus, Grafana) in `docker-compose.yaml`:

```bash
docker compose up -d
bin/wonka run --branch feat/x --otel-endpoint localhost:14317 --otel-insecure work-packages/x/
```

- **Grafana** — http://localhost:3000 (admin/changeme), Telemetry folder
- **Prometheus** — http://localhost:9090 (90-day retention)
- **OTel collector** — OTLP gRPC on `:14317`, HTTP on `:14318`

Telemetry defaults to off. `--otel-insecure` works only for loopback endpoints; `BuildTelemetry` rejects cleartext to remote collectors.

## Project layout

```
├── cmd/wonka/              # CLI entry point (thin main)
├── internal/cmd/           # Cobra subcommands
├── orch/                   # Reusable orchestrator library (DAG dispatch)
│   ├── dispatch.go         #   DAG dispatch loop
│   ├── engine.go           #   Run() / Resume()
│   ├── ledger_beads.go     #   Beads/Dolt backend
│   ├── recovery.go         #   retry, gap tracking, abort cleanup
│   └── ...                 #   see CLAUDE.md for full file map
├── agents/                 # OOMPA.md, LOOMPA.md, CHARLIE.md — agent prompts
└── config/                 # OTel collector, Prometheus, Grafana provisioning
```

## Developing

- **Claude Code users** — [`CLAUDE.md`](CLAUDE.md) is the execution-focused working reference (commands, conventions, design decisions).
- **Contributing** — Conventional commits (`feat`, `fix`, `refactor`, `docs`, `chore`, `test`, `ci`, `build`, `perf`). CI lints PR titles against this list.

## Status

Working draft. Core dispatch, per-branch locking, resume, gap tolerance, circuit breaker, PR gate, and OTel observability ship today; the library and CLI keep evolving. Production requires a Beads/Dolt backend; the FS backend supports local development.

## License

TBD.
