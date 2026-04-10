package orch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBVV_ERR09_SignalCancelsContext verifies BVV-ERR-09: the signal handler
// returns a context that is cancellable, and programmatic cancel works.
// The real signal path (SIGINT/SIGTERM) is exercised end-to-end by the
// integration suite; this unit test locks in the cancellation API.
func TestBVV_ERR09_SignalCancelsContext(t *testing.T) {
	ctx, cancel := orch.SetupSignalHandler()
	defer cancel()

	// Context should not be cancelled yet.
	select {
	case <-ctx.Done():
		t.Fatal("context should not be cancelled before signal")
	default:
	}

	// Programmatic cancel works.
	cancel()
	<-ctx.Done()
	assert.Error(t, ctx.Err())
}

// TestBVV_ERR10a_CleanupIdempotent verifies that Cleanup is idempotent —
// second call on an already-released lock must not error. This is
// load-bearing for the signal path: a deferred Cleanup in the caller plus
// a normal-exit Cleanup both need to run safely.
func TestBVV_ERR10a_CleanupIdempotent(t *testing.T) {
	dir := t.TempDir()

	logPath := filepath.Join(dir, "events.jsonl")
	el, err := orch.NewEventLog(logPath)
	require.NoError(t, err)

	lockCfg := orch.LockConfig{
		Path:               filepath.Join(dir, ".wonka-test.lock"),
		StalenessThreshold: 60,
		RetryCount:         1,
		RetryDelay:         1,
	}
	lock := orch.NewLifecycleLock(lockCfg)
	require.NoError(t, lock.Acquire("test-holder", "feature-x"))

	// First cleanup releases everything.
	orch.Cleanup(nil, lock, el, nil)

	// Lock file is gone.
	_, err = os.Stat(lockCfg.Path)
	assert.True(t, os.IsNotExist(err), "lock file should be removed")

	// Second cleanup must not panic.
	assert.NotPanics(t, func() {
		orch.Cleanup(nil, lock, el, nil)
	})
}

// TestBVV_ERR10a_CleanupNilSafe verifies that Cleanup handles nil arguments
// — important for test fixtures and partial initialisation paths where
// some dependencies may not yet be constructed when a signal arrives.
func TestBVV_ERR10a_CleanupNilSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		orch.Cleanup(nil, nil, nil, nil)
	})
}
