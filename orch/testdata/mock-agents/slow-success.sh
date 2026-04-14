#!/bin/bash
# slow-success.sh — Mock agent that succeeds after a configurable delay.
# DELAY env var controls wait time in seconds (default: 5).
sleep "${DELAY:-5}"
exit 0
