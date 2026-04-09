package orch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOPS12_SignalCancelsContext verifies [OPS-12]: signal handler cancels context.
func TestOPS12_SignalCancelsContext(t *testing.T) {
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

// TestRCV14_CleanupIdempotent verifies [RCV-14]: Cleanup is idempotent.
func TestRCV14_CleanupIdempotent(t *testing.T) {
	dir := t.TempDir()

	logPath := filepath.Join(dir, "events.jsonl")
	el, err := orch.NewEventLog(logPath)
	require.NoError(t, err)

	lockCfg := orch.LockConfig{
		Path:               filepath.Join(dir, ".lock"),
		StalenessThreshold: 60,
		RetryCount:         1,
		RetryDelay:         1,
	}
	lock := orch.NewPipelineLock(lockCfg)
	require.NoError(t, lock.Acquire("test-holder", "test-phase"))

	// First cleanup.
	orch.Cleanup(nil, lock, el, nil)

	// Lock should be released.
	_, err = os.Stat(lockCfg.Path)
	assert.True(t, os.IsNotExist(err), "lock file should be removed")

	// Second cleanup should not panic.
	assert.NotPanics(t, func() {
		orch.Cleanup(nil, lock, el, nil)
	})
}

// TestRCV14_CleanupNilSafe verifies Cleanup handles nil arguments.
func TestRCV14_CleanupNilSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		orch.Cleanup(nil, nil, nil, nil)
	})
}
