#!/bin/sh
# Mock agent that sleeps briefly then exits 1 (retryable failure).
# Used by fault-injection tests that need a non-instantaneous failure
# to exercise timing-sensitive recovery paths.
set -eu
sleep 2
exit 1
