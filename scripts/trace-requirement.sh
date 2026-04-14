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

echo "=== $REQ ==="
echo ""

# Spec
echo "SPEC:"
grep -rn "$REQ" "$ROOT/docs/specs/"*.md 2>/dev/null | head -5 | sed "s|$ROOT/||" || echo "  (not found)"
echo ""

# TLA+
echo "TLA+:"
grep -rn "$REQ" "$ROOT/docs/specs/tla/"*.tla 2>/dev/null | head -5 | sed "s|$ROOT/||" || echo "  (not found)"
echo ""

# Runtime invariants
echo "INVARIANT:"
grep -n "$REQ" "$ROOT/orch/invariant.go" 2>/dev/null | sed "s|$ROOT/||" || echo "  (not found)"
echo ""

# Tests
echo "TESTS:"
grep -rn "$REQ" "$ROOT/orch/"*_test.go 2>/dev/null | sed "s|$ROOT/||" || echo "  (not found)"
echo ""
