#!/usr/bin/env bash
# level4-redux.sh — End-to-end Level 4 test for wonka against real Claude agents.
#
# Drives a complete lifecycle (Charlie → Oompa → Loompa → gate) against an
# isolated temp repo. Verifies three orthogonal signals:
#
#   - ROUTING CONTRACT: <repo>/.beads/ holds the seed; no stray <run-dir>/ledger/
#   - LIFECYCLE OUTCOME: events.jsonl shows lifecycle_completed (not aborted)
#   - BUILD ARTIFACTS: builder produced greet.go + greet_test.go on the branch
#
# This is the manual smoke that the in-repo tests cannot run (real Claude
# sessions cost money and require human-grade flake tolerance). Run it before
# merging changes that touch ledger routing, dispatch, or the planner contract.
#
# Usage:
#   scripts/level4-redux.sh [--keep]
#
# Flags:
#   --keep   Leave the temp dir on disk after the run for inspection.
#            Default: temp dir cleaned up on success, kept on failure.
#
# Cost: roughly $3-8 of Anthropic API credits per run (4 Claude sessions at
# Opus pricing). Duration: 5-15 minutes for the small "greet" work package.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WONKA="$ROOT/bin/wonka"

KEEP=0
for arg in "$@"; do
    case "$arg" in
        --keep)
            KEEP=1
            ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "unknown flag: $arg" >&2
            exit 2
            ;;
    esac
done

PASS=0
FAIL=0
LAST_FAIL=""

pass()    { printf "  \033[32m✓\033[0m %s\n" "$1"; PASS=$((PASS+1)); }
fail()    { printf "  \033[31m✗\033[0m %s\n     %s\n" "$1" "$2"; FAIL=$((FAIL+1)); LAST_FAIL="$1"; }
warn()    { printf "  \033[33m⊘\033[0m %s\n" "$1"; }
section() { printf "\n\033[1m=== %s ===\033[0m\n" "$1"; }

# require_env errors out with a clear install hint when a prerequisite is missing.
require_env() {
    local missing=0
    for tool in bd tmux gh jq go claude git; do
        if ! command -v "$tool" >/dev/null 2>&1; then
            echo "ERROR: missing required tool: $tool" >&2
            missing=1
        fi
    done
    if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
        # Claude Code may also auth via `claude /login`; check for that as a fallback.
        if ! claude --version >/dev/null 2>&1; then
            echo "ERROR: ANTHROPIC_API_KEY not set and \`claude\` CLI is not authenticated" >&2
            missing=1
        fi
    fi
    if [ "$missing" = "1" ]; then
        echo "" >&2
        echo "Level 4 requires: bd tmux gh jq go claude git, plus working Claude auth." >&2
        exit 2
    fi
}

# build_wonka rebuilds the binary so a stale build can't fool the routing test.
build_wonka() {
    section "Build"
    (cd "$ROOT" && CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/wonka ./cmd/wonka)
    pass "binary built at $WONKA"
}

# seed_target creates a fresh fake target repo with a minimal "greet" work
# package. Uses a dot-free path because bd's database-name validator rejects
# dots (a known upstream bug; out of scope for this PR per the plan).
seed_target() {
    local dir
    dir="$(mktemp -d "${TMPDIR:-/tmp}/wonka-l4-XXXXXX")"
    (
        cd "$dir"
        git init -q
        cat > CLAUDE.md <<'EOF'
# Test target

Tiny Go module. Code goes in `greet.go`; tests in `greet_test.go`.
Use the standard library only — no external dependencies.
EOF
        git add CLAUDE.md
        git -c user.email=t@t -c user.name=t commit -qm init >/dev/null
        go mod init example.com/greet >/dev/null 2>&1

        mkdir agents
        cp "$ROOT"/agents/*.md agents/

        mkdir -p work-packages/greet
        cat > work-packages/greet/functional-spec.md <<'EOF'
# CAP-1: Greeting function

## UC-1.1: Format a hello message
- AC-1.1.1: `Greet("World")` returns the string `"Hello, World!"`.
- AC-1.1.2: `Greet("")` returns the string `"Hello, friend!"` (empty input fallback).
EOF
        cat > work-packages/greet/vv-spec.md <<'EOF'
# V-1: Greet behavior
- V-1.1: AC-1.1.1 covered by a Go table test.
- V-1.2: AC-1.1.2 covered by a Go table test.
- V-1.3: `go test ./...` exits 0.
EOF
    )
    echo "$dir"
}

# run_lifecycle starts wonka and waits for it to exit. Unlike the unit-test
# smoke, we don't SIGINT here — we want a real lifecycle to run to completion
# (or abort on its own). Output streams to stdout so the operator can watch
# Claude sessions and dispatch progress in real time.
run_lifecycle() {
    local target="$1"
    local timeout="${2:-15m}"
    "$WONKA" run \
        --branch feat/greet \
        --ledger beads \
        --workers 2 \
        --timeout "$timeout" \
        --repo "$target" \
        "$target/work-packages/greet/"
}

# verify_routing pins the BVV-DSN-04 contract: <repo>/.beads/ exists (auto-init
# fired or was pre-init'd), and no stray <run-dir>/ledger/ was created.
verify_routing() {
    local target="$1"
    section "Routing contract"

    if [ -d "$target/.beads" ]; then
        pass "<repo>/.beads/ exists (auto-init or pre-init succeeded)"
    else
        fail ".beads/ missing" "expected $target/.beads"
    fi

    local stray="$target/.wonka/feat-greet/ledger"
    if [ ! -d "$stray" ]; then
        pass "no stray ledger at $stray (single-ledger contract held)"
    else
        fail "stray per-run ledger present" "regression to dual-ledger split — found $stray"
    fi

    # Tasks should be visible to bd against the same store wonka opened.
    local rows
    if rows="$(cd "$target" && bd list --label "branch:feat/greet" --json 2>/dev/null)" \
       && [ -n "$rows" ] && [ "$rows" != "null" ] && [ "$rows" != "[]" ]; then
        local count
        count="$(echo "$rows" | jq 'length')"
        pass "bd list shows $count task(s) for feat/greet (shared ledger visible)"
    else
        # Beads transport may be unavailable on this host (Dolt server issue);
        # the routing contract is asserted by the stray-dir check above. This
        # is a softer check, so warn rather than fail.
        warn "bd list could not read $target/.beads (transport issue, not routing — see plan non-goals)"
    fi
}

# verify_lifecycle pulls the lifecycle outcome from events.jsonl.
verify_lifecycle() {
    local target="$1"
    section "Lifecycle outcome"

    local log="$target/.wonka/feat-greet/events.jsonl"
    if [ ! -f "$log" ]; then
        fail "events.jsonl missing" "expected $log"
        return
    fi

    local completed
    completed="$(grep -E '"kind":"lifecycle_completed"' "$log" | tail -1 || true)"
    if [ -z "$completed" ]; then
        fail "lifecycle_completed event missing" "wonka exited without recording completion"
        return
    fi

    if echo "$completed" | grep -q "outcome=aborted"; then
        local reason
        reason="$(echo "$completed" | jq -r '.detail // empty')"
        if echo "$reason" | grep -q "BVV-TG-09"; then
            fail "lifecycle aborted at BVV-TG-09" "ROUTING REGRESSION — planner tasks invisible to dispatcher: $reason"
        else
            fail "lifecycle aborted (non-routing reason)" "$reason"
        fi
    else
        pass "lifecycle reached lifecycle_completed (not aborted)"
    fi
}

# verify_artifacts checks that the builder actually wrote code on the branch.
# This is the human-grade signal: did Claude do real work, not just sail
# through dispatch with empty tasks?
verify_artifacts() {
    local target="$1"
    section "Build artifacts"

    if git -C "$target" rev-parse --verify feat/greet >/dev/null 2>&1; then
        pass "feat/greet branch exists"
    else
        fail "feat/greet branch missing" "expected git branch created by builder"
        return
    fi

    local commits
    commits="$(git -C "$target" log --oneline main..feat/greet 2>/dev/null | wc -l | tr -d ' ')"
    if [ "$commits" -gt 0 ]; then
        pass "$commits commit(s) on feat/greet (builder wrote something)"
    else
        fail "no commits on feat/greet" "builder did not produce code"
    fi

    if git -C "$target" show feat/greet:greet.go >/dev/null 2>&1; then
        pass "greet.go committed on feat/greet"
    else
        fail "greet.go missing on feat/greet" "expected builder to create greet.go"
    fi

    if git -C "$target" show feat/greet:greet_test.go >/dev/null 2>&1; then
        pass "greet_test.go committed on feat/greet"
    else
        fail "greet_test.go missing on feat/greet" "expected verifier or builder to create greet_test.go"
    fi

    # Sanity: tests pass when checked out fresh.
    if git -C "$target" show feat/greet:greet.go >/dev/null 2>&1 \
       && git -C "$target" show feat/greet:greet_test.go >/dev/null 2>&1; then
        local probe; probe="$(mktemp -d)"
        git -C "$target" archive feat/greet | tar -x -C "$probe"
        if (cd "$probe" && go test ./... >/dev/null 2>&1); then
            pass "go test ./... passes against feat/greet snapshot"
        else
            warn "go test failed against feat/greet snapshot (Claude wrote code that doesn't pass — not a routing issue)"
        fi
        rm -rf "$probe"
    fi
}

# print_run_summary writes a compact post-mortem to stderr so the operator
# can decide whether to inspect the temp dir.
print_run_summary() {
    local target="$1"
    local log="$target/.wonka/feat-greet/events.jsonl"
    if [ -f "$log" ]; then
        section "Event log tail"
        echo "  (last 8 lines from $log)"
        tail -8 "$log" | jq -r '"  \(.timestamp) \(.kind) \(.task_id // "-") \(.detail // .summary // "-")"' 2>/dev/null \
            || tail -8 "$log"
    fi
}

# ------------------------------------------------------------------
# Main
# ------------------------------------------------------------------
require_env
build_wonka

section "Setup"
TARGET="$(seed_target)"
pass "target repo created at $TARGET"

section "Run wonka (real Claude agents — this may take 5-15 minutes)"
echo "  branch: feat/greet | ledger: beads | workers: 2 | timeout: 15m"
echo "  watch tmux sessions via:  tmux -L wonka-<runID> ls"
echo ""
LIFECYCLE_RC=0
run_lifecycle "$TARGET" 15m || LIFECYCLE_RC=$?
echo ""
echo "  wonka exit code: $LIFECYCLE_RC"

verify_routing "$TARGET"
verify_lifecycle "$TARGET"
verify_artifacts "$TARGET"
print_run_summary "$TARGET"

section "Summary"
printf "  passed: \033[32m%d\033[0m\n" "$PASS"
printf "  failed: \033[31m%d\033[0m\n" "$FAIL"

if [ "$FAIL" -gt 0 ]; then
    echo ""
    printf "\033[31mLEVEL 4 FAILED\033[0m (last: %s)\n" "$LAST_FAIL"
    echo "target preserved for inspection: $TARGET"
    exit 1
fi

if [ "$KEEP" = "1" ]; then
    echo ""
    echo "target preserved (--keep): $TARGET"
else
    rm -rf "$TARGET"
fi

echo ""
printf "\033[32mLEVEL 4 PASSED\033[0m\n"
