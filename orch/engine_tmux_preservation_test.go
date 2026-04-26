//go:build verify

package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngine_ResumeLockContentionPreservesLiveTmux is the C1 regression test.
// The pre-fix bug: Resume's initForResume reads the stale-looking lock,
// recovers the previous RunID, and calls tmux.StartServer on socket
// wonka-<prev>. If the previous orchestrator is still alive, StartServer
// joins the existing server (owned=false). When Acquire then fails with
// ErrLockContention, the old Cleanup call did tmux.KillServer() on the
// joined socket — destroying every session the live holder owned and
// defeating BVV-ERR-08.
//
// This test reproduces the exact scenario: it stands up a tmux server on
// socket "wonka-live-run" with a marker session, runs Resume on a second
// Engine that recovers RunID="live-run" from the lock file, confirms the
// Acquire fails, and asserts the marker session is STILL alive.
func TestEngine_ResumeLockContentionPreservesLiveTmux(t *testing.T) {
	skipWithoutTmux(t)

	runDir := t.TempDir()
	branch := "feat-preserve"
	prevRunID := "live-run"

	// 1. Stand up a "live orchestrator" tmux server with a marker session.
	liveTmux := orch.NewTmuxClient(prevRunID)
	require.NoError(t, liveTmux.StartServer())
	markerSession := prevRunID + "-marker"
	require.NoError(t, liveTmux.CreateSession(markerSession, "sleep 300", t.TempDir()))
	t.Cleanup(func() { _ = liveTmux.KillServer() })

	// 2. Write a lock file reflecting the live holder with a FRESH timestamp
	//    (so staleness recovery does not kick in — Acquire must hit genuine
	//    contention).
	lockPath := filepath.Join(runDir, ".wonka-"+branch+".lock")
	lifecycle := testutil.MockLifecycleConfig(branch, "builder")
	lifecycle.Lock.Path = lockPath
	lifecycle.Lock.StalenessThreshold = 1 * time.Hour
	lifecycle.Lock.RetryCount = 0
	live := orch.NewLifecycleLock(lifecycle.Lock)
	require.NoError(t, live.Acquire(prevRunID, branch))
	t.Cleanup(func() { _ = live.Release() })

	// 3. Pre-create event log + ledger so initForResume gets past the
	//    sentinel check (a parseable first record).
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "ledger"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "logs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "events.jsonl"),
		[]byte(`{"kind":"lifecycle_started","summary":"prior","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644))

	// 4. Attempt Resume on a second Engine that will recover RunID=prevRunID
	//    from the lock file, join the live tmux socket, then fail Acquire.
	cfg := orch.DefaultEngineConfig(lifecycle, runDir, t.TempDir())
	cfg.RunID = "different-from-live"

	e, err := orch.NewEngine(cfg)
	require.NoError(t, err)

	err = e.Resume(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrLockContention)
	// RunID should have been overwritten from the lock file.
	assert.Equal(t, prevRunID, e.RunID())

	// 5. Core assertion: the live holder's marker session MUST still exist.
	// Before C1's fix, Cleanup's unconditional KillServer would have
	// terminated the server and removed the session.
	alive, probeErr := liveTmux.HasSession(markerSession)
	require.NoError(t, probeErr)
	assert.True(t, alive,
		"BVV-ERR-08 regression: the live orchestrator's tmux session was destroyed by the contending Resume's cleanup")
}

// TestTmuxClient_OwnsServerSemantics locks down the ownership tracking
// that C1's fix relies on: a freshly-started server is owned; a second
// client that joins the same socket is not.
func TestTmuxClient_OwnsServerSemantics(t *testing.T) {
	skipWithoutTmux(t)

	runID := "ownership-test"

	creator := orch.NewTmuxClient(runID)
	require.NoError(t, creator.StartServer())
	t.Cleanup(func() { _ = creator.KillServer() })
	assert.True(t, creator.OwnsServer(), "creator must own the server it started")

	joiner := orch.NewTmuxClient(runID)
	require.NoError(t, joiner.StartServer(), "join is idempotent — no error")
	assert.False(t, joiner.OwnsServer(),
		"joiner must NOT claim ownership — would trigger the C1 bug on cleanup")
}
