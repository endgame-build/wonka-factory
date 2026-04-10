#!/bin/sh
# Mock agent that exits 3 (handoff — new session for same task). Used by
# Phase 3 session.go tests to verify the full BVV exit-code protocol
# (BVV-DSP-14 / BVV-L-04: exit 3 → increment HandoffState, respawn).
exit 3
