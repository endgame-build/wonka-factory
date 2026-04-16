#!/bin/bash
# trace-requirement.sh — Forward traceability for BVV requirement IDs.
#
# Usage: scripts/trace-requirement.sh BVV-S-03
#
# Searches spec, TLA+, invariant.go, and test files for the requirement ID.
# Outputs matching locations grouped by artifact type.

set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Usage: $0 <requirement-id>"
    echo "Example: $0 BVV-S-03"
    exit 1
fi

REQ="$1"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# report runs grep and prints results or "(not found)". We capture into a
# variable so we can test for empty output explicitly — piping to `sed | head`
# swallows grep's no-match exit and the `|| echo` branch never fires.
#
# Delimiter note: sed uses `#` (rare in paths) rather than `|` because `$ROOT`
# is injected verbatim; a `|` in the path would corrupt the s-command.
report() {
    local label="$1"
    local out
    # grep's exit 1 on no-match would trigger `set -e`; tolerate it explicitly.
    out="$(grep -rFn "$REQ" "${@:2}" 2>/dev/null | head -5 | sed "s#${ROOT}/##" || true)"
    echo "$label:"
    if [ -z "$out" ]; then
        echo "  (not found)"
    else
        echo "$out"
    fi
    echo ""
}

echo "=== $REQ ==="
echo ""

report "SPEC" "$ROOT/docs/specs/"*.md
report "TLA+" "$ROOT/docs/specs/tla/"*.tla
report "INVARIANT" "$ROOT/orch/invariant.go"
report "TESTS" "$ROOT/orch/"*_test.go
