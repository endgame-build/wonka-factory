# Plan — Replace BeadsStore (Go SDK) with BDCLIStore (bd CLI shell-out)

> **PR-A Implementation Deltas** (read first — the rest of this document is the
> forward plan as written, not the as-shipped state):
>
> 1. **Layer 3 differential bridge test (`orch/ledger_differential_test.go`)
>    was dropped during PR-A**, not deferred to PR-B. It always skipped because
>    `newBeadsForDiff` couldn't open bd 1.0.0 — exactly the SDK ↔ embedded-bd
>    incompatibility this whole migration solves. Parity was instead validated
>    manually via `scripts/level4-redux.sh --ledger-kind bd-cli`. References to
>    Layer 3, the `differential` build tag, `TestStoresAgree`, and the
>    "Differential test: 12/12 scenarios" gate below are obsolete; treat them
>    as historical. Risk #5's mitigation now relies on the contract suite
>    parametrically running against both backends, not on differential parity.
>
> 2. **bd version pinned to `1.0.3` (not `1.0.2`), and the canonical release
>    artifact is `beads_${BD_VERSION}_linux_amd64.tar.gz`** (with `bd` at the
>    archive root), not `bd-linux-amd64.tar.gz` as the example below shows.
>    The actual `.github/workflows/ci.yml` is the source of truth.
>
> 3. **`WONKA_REQUIRE_BD=1`** is now wired into `requireBd(t)` in
>    `orch/store_factory_test.go` — the CI env var converts skips into hard
>    failures so a regression in the "Install bd" workflow step can no longer
>    silently skip the 38-subtest contract suite.

## Context

Wonka's current `BeadsStore` (`orch/ledger_beads.go`) imports `github.com/steveyegge/beads` and calls `beads.Open` — which in beads 1.0.0 expects a Dolt SQL server on TCP. bd 1.0.0 ships in **embedded mode** (cgo Dolt linked into the bd CLI; no SQL server). Since wonka builds `CGO_ENABLED=0`, the two cannot talk on a fresh dev box: every `wonka run --ledger beads` aborts with `Dolt server unreachable at 127.0.0.1:0`. The PR `fix/single-ledger-routing` (#21) verified the routing contract via `scripts/level4-redux.sh` but explicitly cannot run a full Charlie/Oompa/Loompa lifecycle for this reason.

The architectural fix is to honor the same abstraction boundary Charlie does — bd's `--json` CLI surface — and remove the Go SDK from wonka's dependency closure entirely. Charlie already shells out to `bd create`, `bd update`, `bd dep add`. Wonka should too. This:
- Eliminates the cgo / SDK-version variability axis (the symptom of #21's blockage)
- Removes ~50 transitive dependencies (Dolt, vitess, planetscale, etc.) from `go.mod`
- Matches one tool's contract (the CLI's `--json` output, which is documented and stable) instead of two (CLI + SDK Go types)
- Makes wonka↔bd debugging trivial: every interaction is a shell command operators can replicate

**Outcome:** `bin/wonka --ledger beads` runs end-to-end against bd 1.0.0+ on any host with `bd` on PATH. The SDK leaves wonka entirely.

---

## Why not cgo + `beads.OpenBestAvailable`

The alternative — keep the SDK, build wonka with `CGO_ENABLED=1`, and call `beads.OpenBestAvailable` (which uses the same embedded Dolt that bd 1.0 ships with) — is a *correct local optimization against the wrong abstraction*. It re-couples wonka to bd's internal storage types, locks wonka's release matrix to per-platform cgo cross-compile (zig or platform runners), grows the binary from ~10 MB to ~60–80 MB, and means the next bd internal change re-breaks the integration. The CLI-store approach treats `bd --json` as the contract — same boundary Charlie already honors — and removes the SDK from wonka's dependency closure entirely. Pick the right boundary once.

## Approach

Replace `BeadsStore` with `BDCLIStore` over **three incremental PRs**, each independently revertable:

| PR | Goal | Reverts to |
|---|---|---|
| **PR-A** | Add `BDCLIStore` alongside `BeadsStore`. New ledger kind `bd-cli` registered in `storeRegistry`. Contract-test parity. CI installs `bd`. | `BeadsStore` is unchanged and remains the default. |
| **PR-B** | Flip `LedgerBeads`'s registry entry to construct `BDCLIStore`. Delete `ledger_beads.go` + tests. Add `--ledger=beads-sdk` legacy escape hatch behind env var `WONKA_LEDGER_LEGACY_SDK=1` for one release. | PR-A state via env var. |
| **PR-C** | Remove `github.com/steveyegge/beads` from `go.mod`. Mechanical `go mod tidy` after the SDK has no callers. | PR-B state. |

Soak time between PR-A and PR-B: at least one full release cycle dogfooding `--ledger bd-cli`. The Level 4 redux script (`scripts/level4-redux.sh`) is the dogfood smoke.

**Test seam:** `BDCLIStore` exposes an injectable `execCmd` field defaulting to a real `exec.CommandContext` wrapper. Unit tests stub it for fast error-mapping coverage; the contract suite uses real `bd` for fidelity.

**Cycle detection:** single `bd list --json` (returns inline `dependencies[]`) + local DFS. Reuses `orch/ledger_fs.go:reachable` helper. Avoids N+1 `bd dep tree` calls.

**Caching:** none in PR-A. Add only if benchmarks show `ReadyTasks` p95 > 200 ms on `ubuntu-latest`.

**Worker storage:** unchanged. Workers stay as JSON files under `<dir>/workers/` exactly as `BeadsStore` keeps them (bd has no "worker" concept; forcing one is gratuitous).

---

## PR-A: Add BDCLIStore alongside BeadsStore

Branch: `feat/bdcli-store` off `main` (after `fix/single-ledger-routing` lands).

### Phase 0 — verify bd flag surface against assumptions (do this first)

Before writing any code, run these commands against bd 1.0.x and confirm behavior. Each is a load-bearing assumption; surprises here re-shape the design.

| Verification | Command | What we're confirming |
|---|---|---|
| `--id` accepts non-`bd-`-prefixed IDs | `bd create --id "plan-feat-greet" --title X --json` (with and without `--force`) | The seed flow's deterministic `plan-<branch>` IDs survive. If bd refuses without `--force`, we always pass `--force` for orch-managed IDs. If even `--force` rejects, we change `internal/cmd/seed.go` to use bd-assigned IDs + a `wonka:client-id` label for lookup. |
| `bd update --status <V>` accepted values | `bd update <id> --status open\|in_progress\|blocked\|deferred\|closed` (one per try) | Pin the canonical bd-status string for each `TaskStatus`. `StatusBlocked → "blocked"` is presumed; verify. |
| `bd update --claim` semantics under contention | Two parallel `bd update <id> --claim --assignee A` and `--assignee B` | Documents whether bd's atomic claim is "compare-and-swap by current assignee" or "set if empty". This is the multi-orchestrator CAS primitive we get for free; verify before relying on it. |
| Database prefix discovery | `bd config get database` (or read `.beads/config.yaml`) | Knowing the prefix tells us what to pass to `--id` to satisfy bd's matching, and whether `--force` is mandatory for orch-supplied IDs. |
| `bd dep add` cycle error text | Triggered above; text is `"adding dependency would create a cycle"` | Confirms `mapBdError` substring. |
| `bd init --stealth --non-interactive --quiet` exit code on existing `.beads/` | Run twice in same dir | Confirms idempotency for `EnsureBeadsInitialised` retries. |
| `bd ready --json` set vs `bd list --status open --json` minus blocked | One-shot fixture | Pins the "ReadyTasks ≡ bd ready" assumption. If divergent, fall back to local terminality filter. |

If any verification fails, update this plan before proceeding to file changes.

### Files to add

| File | Purpose |
|---|---|
| `orch/ledger_bdcli.go` | `BDCLIStore` struct + 13 Store methods + helpers |
| `orch/ledger_bdcli_test.go` | Contract test wiring (`requireBd(t)`, factory, reopen) |
| `orch/ledger_bdcli_unit_test.go` | Pure-unit tests for `mapBdError`, label encoding, status mapping — all stubbing `execCmd` |
| `orch/ledger_bench_test.go` | `BenchmarkReadyTasks_*`, `BenchmarkAssign_*` (build tag `bench`) |

### Files to modify

| File | Change |
|---|---|
| `orch/types.go` | Add `LedgerBDCLI LedgerKind = "bd-cli"` constant |
| `orch/store_factory.go` | Add `LedgerBDCLI` entry to `storeRegistry` calling `NewBDCLIStore(dir, defaultActor)` |
| `internal/cmd/config.go` | `parseLedgerKind` accepts `"bd-cli"` → `LedgerBDCLI` |
| `internal/cmd/root.go` | `--ledger` flag help text mentions `bd-cli` |
| `.github/workflows/ci.yml` | Add `Install bd` step in `go-quality` job before `Run unit tests`. Pin to a release tarball with SHA256 checksum (mirror the gitleaks pattern at lines 27–33). Cache via `actions/cache` keyed by version. |
| `CLAUDE.md` | Note the new ledger kind under "Store factory" |
| `scripts/level4-redux.sh` | Add `--ledger-kind` flag so verification commands can target either backend without env-var indirection |

### BDCLIStore design

```go
// orch/ledger_bdcli.go
type BDCLIStore struct {
    repoPath  string
    workerDir string
    actor     string
    execCmd   ExecFunc      // injectable for tests
    bdPath    string        // resolved once via exec.LookPath at construction
    mu        sync.Mutex    // serialises mutations within process
}

type ExecFunc func(ctx context.Context, dir, name string, args ...string) (stdout, stderr []byte, err error)

func defaultExecCmd(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
    cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec
    cmd.Dir = dir
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    err := cmd.Run()
    return stdout.Bytes(), stderr.Bytes(), err
}

func NewBDCLIStore(dir, actor string) (*BDCLIStore, error) { ... }
```

### Method-by-method bd CLI mapping

Reuses helpers from existing `BeadsStore` ported verbatim where possible:
- `taskLabelsToBd(*Task) []string` (port of `taskLabelsToBeads` at `orch/ledger_beads.go:130`)
- `bdStatusToOrch(string, []string) TaskStatus` (port of `beadsStatusToOrch` at `:199`)
- `orchStatusToBd(TaskStatus) string` (port of `orchStatusToBeads` at `:175`)

| Store method | bd command(s) | JSON shape | Sentinel mapping |
|---|---|---|---|
| `CreateTask` | `bd create --id <id> [--force] --title <t> --description <body> --priority <p> --labels <k:v,…> --json` (`--force` only if Phase 0 verifies bd rejects non-prefix IDs) | `{id,title,status,priority,…}` | "already exists" → `ErrTaskExists` |
| `GetTask` | `bd show <id> --json` | issue + inline `labels[]` + `dependencies[]` | "not found" → `ErrNotFound` |
| `UpdateTask` | `bd update <id> --title … --description … --priority … --status <bd-status> --assignee … --set-labels <full,…> --json` | mutated issue | "not found" → `ErrNotFound`. `--set-labels` is full replacement (verified on bd 1.0.0). |
| `ListTasks(filters…)` | `bd list --label k:v --json` (one filter pre-applied; remaining AND-matched in Go via `labelsMatch`). Sort locally per LDG-07 (priority↑, ID↑). | array of issues | n/a |
| `ReadyTasks(filters…)` | `bd ready --json` (verified to exist in bd 1.0.0) → array of ready issues. Then in-Go: assignee=="" filter + label match + LDG-07 sort. | array | n/a |
| `Assign(taskID, workerName)` | Under `mu`: ① in-memory pre-checks (read task + worker, return `ErrAlreadyAssigned`/`ErrTaskNotReady`/`ErrWorkerBusy` synchronously); ② `bd update <taskID> --assignee <w> --status open --json`; ③ `atomicWriteJSON(workerPath, worker)`. On step-③ failure, rollback ② via `bd update <taskID> --assignee ""`. | n/a | "not found" → `ErrNotFound`; pre-checks own the rest |
| `CreateWorker`, `GetWorker`, `UpdateWorker`, `ListWorkers` | **No bd calls.** Port verbatim from `orch/ledger_beads.go:469–540`. | n/a | unchanged |
| `AddDep(taskID, dependsOn)` | Under `mu`: ① self-cycle short-circuit; ② `bd list --json` once (returns inline `dependencies[]` per issue); ③ build adjacency map in Go; ④ run `reachable(dependsOn → taskID)` from `orch/ledger_fs.go:428` — if reachable, return `ErrCycle`; ⑤ idempotency check (skip if edge already exists); ⑥ `bd dep add <taskID> <dependsOn>`. | inline | "cycle" → `ErrCycle`; idempotency handled before bd call |
| `GetDeps(taskID)` | `bd show <taskID> --json` and read `.dependencies[].depends_on_id` | inline | n/a |
| `Close()` | no-op; return nil | n/a | n/a |

### Error mapping

Pure function, fully unit-testable:

```go
func mapBdError(exitCode int, stderr string) error {
    switch {
    case strings.Contains(stderr, "not found"):
        return ErrNotFound
    case strings.Contains(stderr, "already exists"),
         strings.Contains(stderr, "UNIQUE constraint"):
        return ErrTaskExists
    case strings.Contains(stderr, "would create a cycle"),
         strings.Contains(stderr, "circular dependency"):
        return ErrCycle
    case exitCode == 2:
        return ErrStoreUnavailable // new sentinel
    default:
        return nil // caller wraps with command context
    }
}
```

New sentinel needed: `ErrStoreUnavailable = errors.New("ledger backend unavailable")` in `orch/errors.go`. Maps to CLI exit code 1 (runtime error) in `internal/cmd/run.go:classifyEngineError`.

### Audit-trail attribution

bd writes its own audit trail using `BEADS_ACTOR` env var, falling back to git `user.name`, then `$USER`. Today `BeadsStore` passes `actor` to every SDK transaction. With BDCLIStore, set `BEADS_ACTOR` on every `exec.Command`:

```go
cmd.Env = append(os.Environ(), "BEADS_ACTOR=orch:"+s.actor)
```

The `actor` string is constructed in `engine.go:initCommon` as `"orch:"+RunID` already (see `orch/ledger_beads.go:56`). Same string flows through. bd's audit trail records `orch:<runID>` as the writer, which is what we want for incident forensics.

### Performance budget

| Method | p50 | p95 | p99 |
|---|---|---|---|
| `ReadyTasks` (≤100 tasks) | 80 ms | 200 ms | 400 ms |
| `ListTasks` (≤100 tasks) | 80 ms | 200 ms | 400 ms |
| `CreateTask` | 50 ms | 120 ms | 250 ms |
| `Assign` (uncontended) | 100 ms | 250 ms | 500 ms |
| `Assign` (8-way contention) | — | 600 ms | 1.5 s |

Each bd invocation runs under `context.WithTimeout(ctx, 5*time.Second)` (10× p99). Timeout returns `ErrStoreUnavailable` and lets the dispatcher skip the tick — never block the loop.

### Test strategy

**Three layers.**

**Layer 1 — pure unit tests** (`orch/ledger_bdcli_unit_test.go`, build tag `verify`):
- `TestMapBdError_Table` — every (exit code, stderr substring) → sentinel
- `TestTaskLabelsToBd_RoundTrip` — pure function on `*Task`
- `TestOrchStatusToBd / BdStatusToOrch` — bidirectional mapping
- `TestBDCLIStore_CreateTask_BuildsExpectedArgs` — stubs `execCmd`, asserts the exact `bd create …` argv

**Layer 2 — contract suite** (`orch/ledger_bdcli_test.go`, build tag `verify`):
```go
func TestBDCLIStore_Contract(t *testing.T) {
    requireBd(t)
    factory := func(t *testing.T) (orch.Store, string) {
        dir := t.TempDir()
        require.NoError(t, exec.Command("bd", "init", "--stealth", "--non-interactive", "--quiet").Run())
        s, err := orch.NewBDCLIStore(dir, "test")
        require.NoError(t, err)
        return s, dir
    }
    reopen := func(t *testing.T, dir string) orch.Store {
        s, err := orch.NewBDCLIStore(dir, "test")
        require.NoError(t, err)
        return s
    }
    RunStoreContractTests(t, factory, reopen)
}
```
Reuses `requireBd(t)` already in `orch/store_factory_test.go`. Drops into all 38 contract subtests.

**Layer 3 — differential bridge test** (`orch/ledger_differential_test.go`, build tag `differential`):
```go
func TestStoresAgree(t *testing.T) {
    requireBd(t)
    scenarios := []scenarioFn{ /* 12 scripted scenarios */ }
    for name, sc := range scenarios {
        t.Run(name, func(t *testing.T) {
            s1, _ := newBeadsStore(t)
            s2, _ := newBDCLIStore(t)
            sc(s1); sc(s2)
            require.Equal(t, dump(s1), dump(s2))
        })
    }
}
```
Run during the migration window; deleted alongside `BeadsStore` in PR-B.

**Concurrency stress.** The contract suite's 400-goroutine `LDG-10` and `BVV-S-03` tests would spawn 400 `bd` processes ≈ 12 GB RAM. Cap with a semaphore at 32 concurrent invocations inside the test driver. The store-level invariant — exactly one Assign succeeds per (task, worker) — still holds under the cap because the test asserts state, not concurrency timing.

### CI changes

`.github/workflows/ci.yml` `go-quality` job, before `Run unit tests`:

```yaml
- name: Install bd
  run: |
    BD_VERSION=v1.0.2  # pinned
    BD_SHA256=...      # SHA-256 of the linux-amd64 tarball, captured at pin time
    curl -fsSL "https://github.com/steveyegge/beads/releases/download/${BD_VERSION}/bd-linux-amd64.tar.gz" -o bd.tar.gz
    echo "${BD_SHA256}  bd.tar.gz" | sha256sum -c
    tar -xzf bd.tar.gz
    sudo mv bd /usr/local/bin/bd
    bd --version
```

Cache via `actions/cache@v4` keyed by `${{ runner.os }}-bd-${BD_VERSION}` so the install step costs ~5s after first run.

`WONKA_REQUIRE_BD=1` set in CI env to convert `requireBd(t)` skips into failures (catches regressions where the install step silently breaks).

### Verification (PR-A)

End-to-end checks before merge:

```bash
# Local quality gate
task check                                    # all green

# New tests run against real bd
WONKA_REQUIRE_BD=1 go test -race -tags verify -run BDCLI ./orch/...

# Pure-unit tests don't need bd
PATH=/usr/bin:/bin go test -race -tags verify -run "TestMapBdError|TaskLabelsToBd|OrchStatusToBd" ./orch/...

# Differential parity
go test -race -tags "verify differential" -run TestStoresAgree ./orch/...

# Benchmarks meet budget
go test -bench=. -tags bench -benchmem ./orch/... | tee bench.txt
benchstat bench.txt   # eyeball BDCLIStore numbers vs the table above

# Level 4 end-to-end against the new store. PR-A adds the --ledger-kind flag
# to scripts/level4-redux.sh so the script can target either backend without
# env-var indirection.
scripts/level4-redux.sh --ledger-kind bd-cli
```

Acceptance gates:
- Contract suite: 38/38 subtests pass against BDCLIStore (CI green with `WONKA_REQUIRE_BD=1`)
- Differential test: 12/12 scenarios match BeadsStore output byte-for-byte
- `BenchmarkReadyTasks_100` p95 ≤ 200 ms on `ubuntu-latest`
- `scripts/level4-redux.sh` reaches `LEVEL 4 PASSED` (not `ENVIRONMENT-BLOCKED`) — proves Charlie/Oompa/Loompa run end-to-end

---

## PR-B: Flip default and delete BeadsStore

Branch: `refactor/drop-beads-sdk` off `main`.

### Changes

- `orch/store_factory.go`: `LedgerBeads` registry entry now constructs `BDCLIStore`. Add `LegacyLedgerBeadsSDK` kind behind env var `WONKA_LEDGER_LEGACY_SDK=1` for one release of operator-controlled rollback.
- Delete `orch/ledger_beads.go`, `orch/ledger_beads_test.go`, `orch/ledger_differential_test.go` (delete differential test in the same diff that deletes `ledger_beads.go` — the differential test imports both stores; deleting one without the other breaks the build).
- Delete `LedgerBDCLI` constant — it's redundant once `LedgerBeads` constructs the same store. CLI flag `--ledger=bd-cli` keeps working for one release as an alias, removed in PR-C.
- Update `CLAUDE.md` and `README.md` "Ledger backends" section to reflect the new mapping (operator-visible behavior unchanged: same `<repo>/.beads/` location, same auto-init).

### Verification (PR-B)

```bash
task check                          # all green
scripts/level4-redux.sh             # full lifecycle
WONKA_LEDGER_LEGACY_SDK=1 task test # legacy escape hatch still functional
```

### Rollback

Operators set `WONKA_LEDGER_LEGACY_SDK=1` in their env and get the old SDK-backed store. Roll forward to a fixed BDCLIStore in the next patch release.

---

## PR-C: Drop SDK from go.mod

Branch: `chore/drop-beads-sdk-dep` off `main`.

### Changes

- Remove `github.com/steveyegge/beads` from `go.mod`.
- Remove the `LegacyLedgerBeadsSDK` env-var path and its registry entry.
- Remove the `--ledger=bd-cli` alias.
- Run `go mod tidy`. Expected: `go.sum` shrinks by ~50 indirect deps (Dolt, vitess, planetscale, etc.).
- Drop the differential test if it wasn't deleted in PR-B.

### Verification (PR-C)

```bash
go mod tidy && git diff go.mod go.sum  # confirm SDK + Dolt + transitive deps gone
task check
scripts/level4-redux.sh
ls -la bin/wonka                        # confirm binary size dropped
```

---

## Critical Files

Already exist; this work reuses them:

- `orch/ledger.go:40–107` — Store interface contract (13 methods)
- `orch/ledger_beads.go:130–215` — label encoding, status mapping (port verbatim)
- `orch/ledger_beads.go:410–465` — Assign rollback pattern (port the shape)
- `orch/ledger_beads.go:469–540` — worker JSON storage (port verbatim)
- `orch/ledger_fs.go:428` — `reachable()` DFS helper (reuse for cycle detection)
- `orch/ledger_fs.go:atomicWriteJSON, readJSON` — used by worker storage
- `orch/ledger_contract_test.go` — `RunStoreContractTests(t, factory, reopen)` parameterised suite (drop-in)
- `orch/store_factory.go:13–20` — `storeRegistry` map; new entry lands here
- `orch/store_factory_test.go:requireBd` — already exists; reused by BDCLIStore tests
- `orch/tmux.go:84` — `exec.Command` + `CombinedOutput` + error-wrap convention to match
- `orch/errors.go` — adds `ErrStoreUnavailable` sentinel
- `internal/cmd/run.go:classifyEngineError` — maps `ErrStoreUnavailable` → `exitRuntimeError`
- `.github/workflows/ci.yml:72–110` — `go-quality` job; bd install step inserted before unit tests
- `scripts/level4-redux.sh` — the dogfood smoke; PR-A adds a `--ledger-kind` flag so we can point it at `bd-cli` for parity testing
- `internal/cmd/seed.go` — the deterministic `plan-<branch>` seed ID is the load-bearing test of Phase 0's `--id` verification. If bd refuses non-prefixed IDs, this file changes; if `--force` works, it stays as-is.
- `agents/CHARLIE.md:127–183` — Charlie's bd CLI usage. **Should not need changes** in any PR (Charlie already shells to bd). Read once before PR-A to confirm wonka's planned bd invocations stay consistent with Charlie's; flag any divergence.

---

## Risks & Open Concerns

1. **Multi-orchestrator safety regression — partially mitigated by `bd update --claim`.** `BeadsStore` relies on Beads SDK transactions for `Assign` atomicity across processes. `BDCLIStore`'s in-process `sync.Mutex` doesn't extend across processes. **Mitigation:** (a) the per-branch lifecycle lock (`orch/lock.go`) already enforces single-orchestrator-per-branch, so this is mostly belt-and-suspenders; (b) bd 1.0.x exposes `bd update <id> --claim` ("atomically claim the issue (sets assignee to you, status to in_progress; idempotent if already claimed by you)") which is the CAS primitive we'd otherwise have to file upstream for. Phase 0 verifies `--claim`'s exact concurrency semantics; if it's "set if empty", `Assign` uses it directly and the cross-process safety story improves vs `BeadsStore` rather than regressing.

2. **bd version compatibility.** PR-A pins bd to a specific minor (e.g. 1.0.x). bd's CLI surface (`--json` output schemas) is the contract. Major version bumps in bd may require coordinated wonka updates. **Mitigation:** `BDCLIStore` runs `bd --version` at startup and refuses to construct if below a documented minimum (e.g. `bd >= 1.0.0`). Surface as `ErrBeadsCLIVersion`.

3. **CI fork-exec cost.** Adding bd install + 38 contract tests via real subprocesses adds ~30s to `go-quality`. Acceptable tradeoff for fidelity.

4. **bd label set has CLI quoting edge cases.** Labels with commas would break `--labels k:v,k2:v2`. **Mitigation:** validate label values in `taskLabelsToBd`; return `ErrInvalidLabelFilter` for values containing commas. Document the constraint; it matches Charlie's existing convention.

5. **`bd ready` semantics differ from `GetReadyWork`.** Verify in PR-A that `bd ready --json` returns the same set as `BeadsStore.ReadyTasks` (open + no active blockers). The differential test in Layer 3 catches divergence.

---

## Out of scope

- bd version-bump automation (we'll bump manually as bd releases land)
- Multi-orchestrator safety beyond the in-process mutex (file upstream; defer)
- Custom labels with commas — operator burden to avoid
- Migration of existing `<repo>/.beads/` databases (no schema change; bd writes are the same regardless of which client wrote them)
- Performance optimization beyond meeting the table in PR-A's budget section (caching, pre-warming, persistent bd subprocess)
