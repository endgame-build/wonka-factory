# Plan — Enable end-to-end lifecycles by aligning wonka and Charlie on a single ledger

## Branching constraint

All changes land on a feature branch off `main`, not on the merged `feat/wonka-run-work-package`.

```bash
git checkout main
git pull
git checkout -b fix/single-ledger-routing
```

Suggested branch name reflects the dominant change. Final commit/PR creation is gated on explicit user approval per CLAUDE.md.

---

## Context

PR #20 shipped the work-package seeding flow, and Level 4 testing surfaced a load-bearing architectural gap that prevents any real lifecycle from completing past the planner.

**The problem:**

```
wonka's dispatcher store:    <run-dir>/.wonka/<branch>/ledger/
                             └── opened by Engine.init() at orch/engine.go:293
                             └── used by ReadyTasks, CreateTask, UpdateTask

Charlie's bd writes:         <target-repo>/.beads/
                             └── written by `bd create` from $ORCH_PROJECT
                             └── invisible to wonka's dispatcher
```

Lifecycle today:

1. Wonka seeds `plan-<branch>` to its own ledger.
2. Wonka dispatches the planner; Charlie spawns.
3. Charlie writes builder/verifier/gate tasks to `<repo>/.beads/`.
4. Charlie exits 0; wonka marks the seed `completed` in *its* ledger.
5. Wonka calls `ReadyTasks(branch)` on its ledger → returns nothing (only the now-completed seed).
6. `ValidateLifecycleGraph` runs → "0 gate tasks, expected 1" → BVV-TG-09 abort.

**Goal:** Make `--ledger beads` open `<repo>/.beads/` directly so wonka and Charlie share a single ledger. Charlie's writes become immediately visible to the dispatcher; full lifecycles run end-to-end. This matches BVV-DSN-04's intent (one ledger, dispatcher reads what planner writes) — the per-run-dir ledger was a Phase-1 simplification that became a Phase-9 mismatch.

Bundle two CHARLIE.md drift fixes that surfaced during the same Level 4 run, since they're trivial and would block validation otherwise.

---

## Recommended approach

### 1. Path resolution — single source of truth

Add a helper `ResolveLedgerDir(repoPath, runDir string, kind LedgerKind, override string) string` to `orch` (e.g. `orch/store_factory.go`, alongside `NewStore`):

```go
func ResolveLedgerDir(repoPath, runDir string, kind LedgerKind, override string) string {
    if override != "" {
        return override  // EngineConfig.LedgerPath escape hatch (test seam)
    }
    if kind == LedgerBeads {
        return filepath.Join(repoPath, ".beads")
    }
    // LedgerFS: per-run dev convenience, unchanged.
    return filepath.Join(runDir, "ledger")
}
```

Replace the two hardcoded `filepath.Join(e.cfg.RunDir, "ledger")` sites in `orch/engine.go` (`init` line 293, `initForResume` line 316) with calls to this helper. Replace `internal/cmd/status.go:80` (currently uses a duplicated `ledgerSubdir` constant — see TODO at line 15) with the same helper, eliminating the documented drift hazard.

Add a non-CLI `LedgerPath string` field to `orch.EngineConfig`. **Purpose:** test seam only — production code paths derive the location from `LedgerKind` + repo/run-dir. Tests that need to point the beads backend at a controlled directory (e.g. `t.TempDir()/.beads`) without requiring `bd` on the test runner's PATH set this field directly. The CLI does *not* expose a `--ledger-path` flag (per the user's decision), and the field's doc comment must say so explicitly.

### 2. Resume detection — switch the sentinel from ledger to event log

Today, `Engine.initForResume()` at `orch/engine.go:317` decides "fresh start vs resume" by `os.Stat(ledgerDir)`. With the ledger now at `<repo>/.beads/`, this would falsely trigger "resume" on any repo that has bd installed.

**Fix:** Switch the existence check to `events.jsonl` under run-dir. The event log is wonka-owned, written on every fresh `Run()` call (see `emitLifecycleStarted` at `orch/engine.go:163`), so its absence is the canonical "no prior wonka run on this branch" signal.

**Robustness:** "Exists" alone isn't sufficient — an empty or zero-byte file (touched by mistake, or left over from a crashed init) would falsely trigger resume. The check must verify the file exists *and* contains at least one parseable event. Use the existing event-log reader (or a cheap one-line variant) rather than re-implementing parsing. Treat parse failure on the first record as `ErrCorruptEventLog` (new sentinel) — distinct from `ErrResumeNoEventLog` because the recovery action differs (corrupt = human intervention; absent = use `wonka run`).

Rename `ErrResumeNoLedger` → `ErrResumeNoEventLog`. Update the CLI hint in `internal/cmd/run.go:107` to match: "no event log for branch %q — run `wonka run` to start a fresh lifecycle".

### 3. Auto-init bd when missing

When `--ledger beads` is set and `<repo>/.beads/` doesn't exist, wonka initialises the beads database in the target repo before opening the store. Per the user's decision, this is friendlier than requiring a separate operator step.

**Prefer the SDK over the CLI.** `github.com/steveyegge/beads` is already a direct dependency (`orch/ledger_beads.go:1` imports it). Check whether the SDK exposes an `Init` or equivalent and call that. Falling back to `exec.Command("bd", "init")` is acceptable only if the SDK doesn't expose programmatic init — the SDK call avoids a process spawn, surfaces typed errors, and doesn't require `bd` on PATH for the wonka-side path.

Implementation goes in `orch/store_factory.go` adjacent to `NewStore`. Sketch:

```go
func ensureBeadsInitialised(beadsDir string) (created bool, err error) {
    if _, statErr := os.Stat(beadsDir); statErr == nil {
        return false, nil
    } else if !os.IsNotExist(statErr) {
        return false, fmt.Errorf("stat beads dir: %w", statErr)
    }
    // 1. Try beads SDK init if available (preferred).
    // 2. Fall back to exec.Command("bd", "init") if not.
    // ...
    return true, nil
}
```

The `created bool` return lets the engine emit a one-time stderr warning the first time auto-init fires:

```
warning: initialised beads at <repo>/.beads/ — also creates AGENTS.md
         and may install Claude hooks per the bd-init contract; review
         the diff in the target repo before committing
```

This matters because the Level 4 run showed `bd init` creates more than just `.beads/`: an `AGENTS.md` file appears at the repo root and Claude hooks may be installed. Operators must know wonka mutated their working tree.

If neither the SDK init nor `bd` on PATH is available, return a clear error: "cannot initialise beads at `<repo>/.beads/` — install `bd` (https://...) or run `bd init` manually first". Surface this at `BuildEngineConfig`-level so it fails fast before the lifecycle lock is acquired.

### 4. Operator-visible behavior change to document

With shared `<repo>/.beads/`, multiple branches' wonka runs against the same repo coexist in one bd database, distinguished only by `branch:<name>` labels. Two consequences worth documenting in the README and CHARLIE.md:

- **`bd list` (operator inspection from the target repo) shows tasks across all branches.** Operators who want a single-branch view should use `wonka status --branch X` or `bd list --label branch:X`.
- **Concurrent wonka runs against different branches in the same repo** all hit the same bd backend. The per-branch lifecycle lock at `<run-dir>/.wonka-<branch>.lock` still prevents two wonka runs from clobbering the *same* branch, but bd-level concurrent writes from different branches' Charlies are now possible. The beads SDK's transaction model handles this — `BeadsStore.CreateTask` already calls `RunInTransaction` (`orch/ledger_beads.go:240`) — but worth a one-line note in README so operators don't expect per-branch isolation at the storage layer.

### 5. CHARLIE.md ride-along fixes

Two trivial doc corrections:

- **Priority value (`agents/CHARLIE.md:163,169`):** Replace "999" with "4" in both places. Charlie observed at runtime that `bd` rejects 999 even though the Go SDK accepts any int — the `bd` CLI applies a 0-4 range check the SDK doesn't. Add a one-line note: "priority is 0-4 (lower = dispatched first); use 4 for the gate so it's the last task scheduled among independents."
- **bd ID format (new section in CHARLIE.md, ~3 lines):** "`bd create` returns IDs prefixed with the repo's beads database name plus a random suffix (e.g. `myrepo-7ze`). Use the returned IDs verbatim in `--deps` arguments; do not invent ID schemes." This eliminates Charlie's "test-prefix-probe" pattern observed in the Level 4 run.

---

## Files to modify

| File | Change |
|------|--------|
| `orch/store_factory.go` | Add `ResolveLedgerDir(repoPath, runDir, kind, override) string`. Add `ensureBeadsInitialised(beadsDir) (created bool, err error)` that prefers the beads SDK and falls back to `exec.Command("bd", "init")` if the SDK lacks programmatic init. Wire both into `NewStore` (or expose for engine to call). |
| `orch/engine.go` | Replace `filepath.Join(e.cfg.RunDir, "ledger")` at lines 293 and 316 with `ResolveLedgerDir(...)`. Switch `initForResume`'s "no prior run" check at line 317 from ledger-stat to event-log existence + parseability. Add `EngineConfig.LedgerPath string` field (test seam only; CLI does not expose a flag for this). |
| `orch/errors.go` | Add `ErrResumeNoEventLog` (renamed from `ErrResumeNoLedger`). Add `ErrCorruptEventLog` for the parseable-on-first-record check. Update any `errors.Is` call sites. |
| `internal/cmd/status.go` | Replace `const ledgerSubdir` + line-80 join with a call to `orch.ResolveLedgerDir(...)`. Resolves the drift TODO at lines 15-19. |
| `internal/cmd/run.go` | Update the `ErrResumeNoLedger` mapping at line 107 to reflect the event-log sentinel. Message text refresh only. |
| `internal/cmd/config.go` | If wonka now needs `bd` on PATH for `--ledger beads`, add a precondition check in `BuildEngineConfig` so a missing `bd` fails fast rather than at engine init. |
| `agents/CHARLIE.md` | Two edits: (a) priority "999" → "4" at lines 163 and 169 with one-line rationale; (b) new ~3-line section on bd-assigned ID format under "Phase 3: GRAPH". |
| `orch/engine_spec_test.go` | Add tests for `ResolveLedgerDir` (beads → repo path, fs → run-dir path). Update existing engine tests that prepopulate the ledger via `prepopulateLedger` (`orch/engine_spec_test.go:95`) to use the resolved path. Add a resume test confirming the event-log sentinel. |
| `orch/engine_internal_test.go`, `orch/engine_e2e_test.go`, `orch/engine_tmux_preservation_test.go`, `orch/resume_errorpath_spec_test.go`, `orch/watchdog_test.go` | Mass update: replace literal `filepath.Join(runDir, "ledger")` with `orch.ResolveLedgerDir(repoPath, runDir, orch.LedgerFS)` (FS still uses run-dir, so behavior unchanged). |
| `internal/cmd/status_test.go` | Same mass update as above. |
| `scripts/smoke-work-package.sh` | Add a Level 2b case that exercises `--ledger beads` against a `bd init`-ed temp repo, asserting the seed lands in `<repo>/.beads/` not `<run-dir>/ledger/`. |

---

## Reused primitives

- `orch.NewStore(kind, dir)` — `orch/store_factory.go`. Wrap rather than replace.
- `orch.LedgerKind` enum + `LedgerBeads`/`LedgerFS` constants — `orch/types.go:69-72`. Already typed; just need the path-resolution layer.
- `Engine.initCommon(ledgerDir)` — `orch/engine.go:348`. Already accepts the path as a parameter; the change is in what gets passed.
- Existing `events.jsonl` writer — `EventLog.emitLifecycleStarted` is the natural sentinel for "wonka has touched this branch."
- `os/exec` for the `bd init` shell-out, matching the pattern used elsewhere in `orch/tmux.go` for `exec.Command("tmux", ...)`.

---

## Verification

### Unit tests

- `TestResolveLedgerDir` — table-driven: beads → `<repo>/.beads`, fs → `<run-dir>/ledger`, both with and without slashes in repo path.
- `TestEnsureBeadsInitialised_AlreadyExists` — with a pre-populated `.beads/`, the function returns `(false, nil)` without invoking init.
- `TestEnsureBeadsInitialised_CreatesIfMissing` — with no `.beads/`, the function returns `(true, nil)` and the dir appears. Gate behind a `requireBd(t)` helper that skips if neither the SDK init nor `bd` on PATH is available.
- `TestEnsureBeadsInitialised_NoBackend` — neither SDK init nor `bd` on PATH → returns a clear error naming the install steps.
- `TestEngine_Resume_NoEventLog` — fresh repo with no `events.jsonl` returns `ErrResumeNoEventLog` even if `<repo>/.beads/` exists.
- `TestEngine_Resume_EmptyEventLog` — zero-byte `events.jsonl` returns `ErrResumeNoEventLog` (not silent-corrupt-resume).
- `TestEngine_Resume_CorruptEventLog` — first line is malformed JSON → returns `ErrCorruptEventLog` distinct from `ErrResumeNoEventLog`.
- `TestEngine_Resume_WithEventLog` — `events.jsonl` present and parseable → resume proceeds, opens the bd ledger from `<repo>/.beads/`.

### Integration

- Update `TestEngine_RunInvokesSeed` and `TestEngine_RunSeedErrorAborts` (added in PR #20) to verify the seed lands at the resolved path under both backends. Both use `EngineConfig.LedgerPath` (the test seam) to point at `t.TempDir()/.beads` so they don't depend on `bd` on PATH.
- New: `TestEngine_FullLifecycle_BeadsBackend` — drive a fake planner via `SetTestSpawnFunc` that creates a builder + verifier + gate task in the same store wonka opens. Assert the lifecycle reaches `lifecycle_completed` (not `aborted`). Uses `EngineConfig.LedgerPath` to bypass the auto-init path; that path gets its own gated test above.

### Test runner dependencies

- Existing tests that use `--ledger fs` and `MockLifecycleConfig` continue to run with no new dependencies.
- New tests touching the beads path either (a) use `EngineConfig.LedgerPath` for direct injection (no `bd` needed) or (b) gate behind `requireBd(t)` and skip when unavailable (CI Linux runners have `bd`; macOS dev machines may not).
- The `Taskfile.yml` `test` target runs everything; the existing `test-integration` target (build tag `integration`) is the natural home for the full-lifecycle tests that exercise `bd init`.

### Manual smoke (Level 4 redux)

Re-run the Level 4 scenario from PR #20 with `--ledger beads`:

```bash
SMOKE=$(mktemp -d) && cd "$SMOKE"
git init -q && touch CLAUDE.md && git add . && git -c user.email=t@t -c user.name=t commit -qm init
go mod init example.com/greet
mkdir agents && cp <wonka-repo>/agents/*.md agents/
mkdir -p work-packages/greet
# (write functional-spec.md and vv-spec.md as in PR #20's Level 4)

# No `bd init` — wonka should auto-init.
~/projects/endgame/wonka-factory/bin/wonka run \
  --branch feat/greet --ledger beads \
  --workers 2 --timeout 15m \
  work-packages/greet/

# Expect: lifecycle_completed (not aborted).
# Expect: bd list shows planner + builder + verifier + gate, all completed.
# Expect: target repo has greet.go + greet_test.go committed on feat/greet.
```

### Quality gates

```bash
CGO_ENABLED=0 go test -race -tags verify -count=1 ./orch/... ./internal/...
CGO_ENABLED=0 golangci-lint run --build-tags=verify --timeout=5m
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/wonka ./cmd/wonka
scripts/smoke-work-package.sh
```

### Pre-merge checklist

- All work on `fix/single-ledger-routing`, not `main`.
- Conventional commits (`fix(orch):` for the path change, `docs(agents):` for CHARLIE.md, `test(scripts):` for smoke updates).
- Full Level 4 smoke against `--ledger beads` runs end-to-end without the BVV-TG-09 abort observed in PR #20.
- PR opened only after explicit user approval.

---

## Out of scope (explicit non-goals)

- **`--ledger-path` CLI flag.** Per the user's decision, derive ledger location from `--ledger` only. The `EngineConfig.LedgerPath` test seam exists but is not exposed on the CLI. If real-world need emerges, add the flag in a future PR.
- **bd database-name normalization issue.** Charlie observed in the Level 4 run that bd rejected a database name with a dot (the temp-dir suffix). Possibly a beads bug; possibly a wonka onboarding gap if temp-dir-style repo names are common. Investigate in a separate issue — this PR's auto-init may sidestep it for typical project repos, and chasing the edge case here would expand scope.
- **Deprecating `--ledger fs`.** It's still useful for fast unit tests and for the Level 1-3 smoke checks where Charlie isn't actually invoked. Document that `--ledger fs` cannot run a full Charlie lifecycle (Charlie is hardcoded to `bd`), but keep the flag.
- **Rewriting Charlie to write to a non-bd store.** Out of scope; wonka conforms to Charlie's `bd` contract, not the other way around.
- **Migration of existing wonka run-dirs.** Pre-existing `<run-dir>/.wonka/<branch>/ledger/` directories from PR #20 lifecycles are stale anyway (those lifecycles aborted on BVV-TG-09). No migration needed; operators with active wonka runs from PR #20 should let them drain (or reset via `git clean`) before re-running with this PR.
- **Rolling back the bd auto-init's side effects.** `bd init` creates `AGENTS.md` and may install Claude hooks alongside `.beads/`. Wonka's auto-init surfaces a warning (per section 3) but does not "uninstall" anything. Operators who don't want those side effects must run `bd init` manually with whatever flags suppress them, or accept them as part of choosing `--ledger beads`.
