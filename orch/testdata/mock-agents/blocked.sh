#!/bin/sh
# Mock agent that exits 2 (blocked — terminal, non-retryable). Used by
# Phase 3 session.go tests to verify the full BVV exit-code protocol
# (BVV-DSP-04: exit 2 → StatusBlocked).
exit 2
