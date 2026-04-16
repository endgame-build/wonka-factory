#!/bin/sh
# Mock agent that succeeds after a configurable delay.
# DELAY env var controls wait time in seconds (default: 5).
set -eu
sleep "${DELAY:-5}"
exit 0
