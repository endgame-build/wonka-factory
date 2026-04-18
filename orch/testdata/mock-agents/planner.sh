#!/bin/sh
# Symbolic mock planner for future shell-driven integration.
#
# The BVV E2E tests currently seed task graphs via Go SpawnFunc injection
# (see testutil.PlannerSpawnFunc) rather than shell scripts, because the
# fixture-backed FS store the tests use has no CLI wrapper. A real shell-
# driven planner would:
#   1. Read $ORCH_WORK_PACKAGE (directory with functional/technical/vv specs).
#   2. Emit tasks via `bd create --branch $ORCH_BRANCH --label role:...`.
#   3. Wire deps via `bd dep add`.
#   4. Exit 0 (done) / 1 (retryable) / 2 (blocked) / 3 (handoff).
#
# Kept here as a placeholder so operators migrating from Go-driven tests
# to Beads-backed integration have a known anchor.

echo "mock-planner: ORCH_TASK_ID=${ORCH_TASK_ID:-unset}"
echo "mock-planner: ORCH_BRANCH=${ORCH_BRANCH:-unset}"
echo "mock-planner: ORCH_WORK_PACKAGE=${ORCH_WORK_PACKAGE:-unset}"
exit 0
