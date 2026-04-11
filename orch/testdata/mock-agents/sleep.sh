#!/bin/sh
# Mock agent that sleeps briefly and exits 0. Used by Phase 3 watchdog
# tests that need to PROVE a RestartSession call actually created a fresh
# tmux session — an instant-exit agent like ok.sh leaves the session dead
# on both sides of CheckOnce, so IsAlive can never witness a restart.
# Sleep long enough for a poll loop to observe alive=true, but short enough
# to keep the test suite fast.
sleep 0.5
exit 0
