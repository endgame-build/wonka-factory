#!/bin/sh
# Mock agent that exits 1 (retryable failure). Used by Phase 3 session.go
# tests to verify SpawnSession error-path cleanup.
exit 1
