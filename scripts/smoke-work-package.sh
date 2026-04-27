#!/bin/bash
# smoke-work-package.sh — End-to-end smoke test for `wonka run <work-package>`.
#
# Usage: scripts/smoke-work-package.sh
#
# Exercises the encapsulated planner-task seeding flow without launching real
# agents. Three levels:
#
#   Level 1: CLI surface — help text, exit codes, validation rejections
#   Level 2: Seed verification — confirms plan-<branch> task is created in the
#            ledger with correct labels, body, and hash
#   Level 3: Replan logic — confirms hash-match no-ops vs hash-mismatch reopens
#
# Each level uses an isolated temp dir. Failures print the failing assertion
# and exit non-zero. Clean exits leave no residue.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WONKA="$ROOT/bin/wonka"

# Aggregated counters for the final summary. Track passes too so the summary
# distinguishes "ran nothing" (suspicious) from "ran 12, all passed".
PASS=0
FAIL=0
LAST_FAIL=""

# pass / fail helpers — log a green ✓ on success and a red ✗ + failure
# context on miss, but never `exit` mid-test. We want to run every assertion
# in a level so the operator sees the whole picture, not just the first break.
pass()  { printf "  \033[32m✓\033[0m %s\n" "$1"; PASS=$((PASS+1)); }
fail()  { printf "  \033[31m✗\033[0m %s\n     %s\n" "$1" "$2"; FAIL=$((FAIL+1)); LAST_FAIL="$1"; }
section() { printf "\n\033[1m=== %s ===\033[0m\n" "$1"; }

# require_tool errors out before we touch the filesystem so missing deps
# surface as a clear "install X" message rather than mid-test confusion.
require_tool() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "ERROR: required tool '$1' not on PATH"; exit 2
    fi
}

# build_wonka rebuilds the binary every run. Cheap (~2s) and removes the
# "stale binary" failure mode where smoke tests pass against an old build.
build_wonka() {
    section "Build"
    (cd "$ROOT" && CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/wonka ./cmd/wonka)
    pass "binary built at $WONKA"
}

# seed_target creates a fresh fake target repo with the layout wonka expects:
# git-initialised, CLAUDE.md present, agents/ populated, work-packages/demo/
# carrying a minimal valid two-file work order. Returns the path on stdout.
seed_target() {
    local dir
    dir="$(mktemp -d)"
    (
        cd "$dir"
        git init -q
        touch CLAUDE.md
        git add CLAUDE.md
        git -c user.email=t@t -c user.name=t commit -qm init >/dev/null
        mkdir agents
        cp "$ROOT"/agents/*.md agents/
        mkdir -p work-packages/demo
        printf '# CAP-1: Demo\n## UC-1.1\nAC-1.1.1: ok.\n' > work-packages/demo/functional-spec.md
        printf '# V-1: Demo\n- V-1.1: AC-1.1.1 covered.\n' > work-packages/demo/vv-spec.md
    )
    echo "$dir"
}

# run_briefly starts wonka in the background and SIGINTs it after the lock is
# acquired and the seed has run. The seed fires synchronously between
# lifecycle_started and the first dispatch tick, so 2s is generous; the
# kill -INT lets the engine shut down cleanly without leaving a stale lock.
#
# Accept exit codes {0, 130}: clean lifecycle exit or SIGINT-cancel. Anything
# else (panic, config error, runtime crash, lock-busy) means the run didn't
# play out as expected and downstream assertions would read junk ledger state.
run_briefly() {
    local target="$1"; shift
    "$WONKA" run --repo "$target" --ledger fs --run-dir "$target/.wonka/run" "$@" >/dev/null 2>&1 &
    local pid=$!
    sleep 2
    kill -INT "$pid" 2>/dev/null || true
    local rc=0
    wait "$pid" 2>/dev/null || rc=$?
    case "$rc" in
        0|130) ;;
        *) fail "wonka exited unexpectedly" "rc=$rc (expected 0 or 130)" ;;
    esac
}

# read_seed_field extracts a single field from the seeded planner task JSON.
# Routed through jq because the labels map order isn't stable across writes,
# so a grep-based check would be flaky. jq is already a hard dep elsewhere
# in the repo's tooling, so requiring it here is fair.
read_seed_field() {
    local target="$1" id="$2" path="$3"
    jq -r "$path" "$target/.wonka/run/ledger/tasks/$id.json"
}

# ----------------------------------------------------------------------
# Level 1 — CLI surface (no agents, no ledger)
# ----------------------------------------------------------------------
level1_cli_surface() {
    section "Level 1: CLI surface"

    local out
    out="$("$WONKA" run --help 2>&1 || true)"
    if echo "$out" | grep -q "wonka run <work-package>"; then
        pass "help shows positional in usage line"
    else
        fail "help missing positional" "$(echo "$out" | head -3)"
    fi

    # Missing positional → cobra ExactArgs(1) error, exit 1
    local rc=0
    "$WONKA" run --branch test >/dev/null 2>&1 || rc=$?
    if [ "$rc" = "1" ]; then
        pass "missing positional → exit 1"
    else
        fail "missing positional wrong exit" "got $rc, expected 1"
    fi

    # Bad path → exitConfigError (2). Use --repo to confine to a temp dir
    # so the test doesn't depend on the cwd's contents.
    local tmp; tmp="$(mktemp -d)"
    rc=0
    "$WONKA" run --branch test --repo "$tmp" --ledger fs nonexistent >/dev/null 2>&1 || rc=$?
    if [ "$rc" = "2" ]; then
        pass "bad work-package path → exit 2 (config error)"
    else
        fail "bad-path wrong exit" "got $rc, expected 2"
    fi
    rm -rf "$tmp"

    # Resume rejecting positional must produce a hint pointing at `wonka run`.
    # We don't pin the exact string — just the user-facing intent.
    out="$("$WONKA" resume --branch test something 2>&1 || true)"
    if echo "$out" | grep -q "wonka run"; then
        pass "resume + positional → hint points at \`wonka run\`"
    else
        fail "resume hint missing" "$out"
    fi

    # Status NoArgs — extra positional must be rejected. cobra's default
    # phrasing varies across versions, so we match on the user's token.
    out="$("$WONKA" status --branch test extra 2>&1 || true)"
    if echo "$out" | grep -q "extra"; then
        pass "status + positional → rejection echoes user input"
    else
        fail "status NoArgs not enforced" "$out"
    fi
}

# ----------------------------------------------------------------------
# Level 2 — Seed task verification
# ----------------------------------------------------------------------
level2_seed() {
    section "Level 2: Seed task creation"

    local target; target="$(seed_target)"
    run_briefly "$target" --branch feat/demo work-packages/demo/

    local seed_path="$target/.wonka/run/ledger/tasks/plan-feat-demo.json"
    if [ ! -f "$seed_path" ]; then
        fail "seed task file missing" "expected $seed_path"
        rm -rf "$target"
        return
    fi
    pass "plan-feat-demo task created in ledger"

    # Status is intentionally not asserted: the seed writes status=open, but
    # the dispatcher races to assign + spawn an agent before our SIGINT lands.
    # Without a real CLAUDE_API_KEY the agent exits non-zero and dispatch flips
    # the status. The seed's contract is the labels/body/hash — not the
    # immediate post-seed status, which is dispatch's territory.
    local id role branch crit hash body
    id="$(read_seed_field "$target" plan-feat-demo .id)"
    role="$(read_seed_field "$target" plan-feat-demo '.labels.role')"
    branch="$(read_seed_field "$target" plan-feat-demo '.labels.branch')"
    crit="$(read_seed_field "$target" plan-feat-demo '.labels.criticality')"
    hash="$(read_seed_field "$target" plan-feat-demo '.labels."wonka:work-order-hash"')"
    body="$(read_seed_field "$target" plan-feat-demo .body)"

    [ "$id" = "plan-feat-demo" ] && pass "ID is deterministic plan-<sanitized>" \
        || fail "wrong ID" "got $id"
    [ "$role" = "planner" ] && pass "role label = planner" \
        || fail "wrong role label" "got $role"
    [ "$branch" = "feat/demo" ] && pass "branch label preserves slash" \
        || fail "branch label sanitized" "got $branch (expected feat/demo)"
    [ "$crit" = "critical" ] && pass "criticality = critical" \
        || fail "wrong criticality" "got $crit"
    [ "${#hash}" = "64" ] && pass "work-order-hash is 64-char sha256 hex" \
        || fail "wrong hash length" "got ${#hash} chars"
    [ "$body" = "$target/work-packages/demo" ] && pass "body is absolute work-order path" \
        || fail "wrong body" "got $body"

    rm -rf "$target"
}

# ----------------------------------------------------------------------
# Level 3 — Replan: no-op vs reopen
# ----------------------------------------------------------------------
level3_replan() {
    section "Level 3: Replan logic"

    local target; target="$(seed_target)"
    run_briefly "$target" --branch feat/demo work-packages/demo/

    local seed_path="$target/.wonka/run/ledger/tasks/plan-feat-demo.json"
    local orig_hash; orig_hash="$(read_seed_field "$target" plan-feat-demo '.labels."wonka:work-order-hash"')"

    # Simulate Charlie having run successfully. Mark the seed completed via a
    # raw JSON edit — we're testing the seed pass, not the dispatch path.
    jq '.status = "completed"' "$seed_path" > "$seed_path.new" && mv "$seed_path.new" "$seed_path"

    # Re-run with no spec changes. Must be a no-op.
    run_briefly "$target" --branch feat/demo work-packages/demo/
    local status_a hash_a
    status_a="$(read_seed_field "$target" plan-feat-demo .status)"
    hash_a="$(read_seed_field "$target" plan-feat-demo '.labels."wonka:work-order-hash"')"
    [ "$status_a" = "completed" ] && pass "no-change re-run leaves status=completed" \
        || fail "no-change re-run mutated status" "got $status_a"
    [ "$hash_a" = "$orig_hash" ] && pass "no-change re-run leaves hash intact" \
        || fail "no-change re-run mutated hash" "$orig_hash → $hash_a"

    # Edit the spec — must reopen with a new hash. Status check is "moved off
    # completed" rather than "exactly open" because dispatch may have started
    # processing the reopened task before our SIGINT (open → assigned →
    # in_progress → ... → failed if the agent can't auth). What matters is
    # the seed flipped it out of completed; what dispatch does next is
    # dispatch's problem, not the seed's.
    echo "## CAP-2: New" >> "$target/work-packages/demo/functional-spec.md"
    run_briefly "$target" --branch feat/demo work-packages/demo/
    local status_b hash_b
    status_b="$(read_seed_field "$target" plan-feat-demo .status)"
    hash_b="$(read_seed_field "$target" plan-feat-demo '.labels."wonka:work-order-hash"')"
    [ "$status_b" != "completed" ] && pass "spec edit reopens planner (status moved off completed: now $status_b)" \
        || fail "spec edit did not reopen" "status still completed"
    [ "$hash_b" != "$orig_hash" ] && pass "spec edit produces new hash" \
        || fail "spec edit did not change hash" "still $orig_hash"

    # Body refreshed too (paranoia — usually the same path, but the contract
    # is "always rewrite on reopen" so a future caller passing a new path gets
    # the right body).
    local body_b; body_b="$(read_seed_field "$target" plan-feat-demo .body)"
    [ "$body_b" = "$target/work-packages/demo" ] && pass "body refreshed on reopen" \
        || fail "body wrong after reopen" "got $body_b"

    rm -rf "$target"
}

# ----------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------
require_tool jq
require_tool git
require_tool go

build_wonka
level1_cli_surface
level2_seed
level3_replan

section "Summary"
printf "  passed: \033[32m%d\033[0m\n" "$PASS"
printf "  failed: \033[31m%d\033[0m\n" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
    printf "\n\033[31mSMOKE FAILED\033[0m (last: %s)\n" "$LAST_FAIL"
    exit 1
fi
printf "\n\033[32mSMOKE PASSED\033[0m\n"
