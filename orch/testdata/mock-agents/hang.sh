#!/bin/sh
# Mock agent that never exits. Tests timeout enforcement (BVV-ERR-02a).
# Traps SIGTERM so tmux teardown produces a deterministic exit status (143)
# instead of depending on the shell's default signal handling.
set -eu
trap 'exit 143' TERM
sleep 3600
