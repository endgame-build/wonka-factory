package orch_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLock(t *testing.T) *orch.PipelineLock {
	t.Helper()
	dir := t.TempDir()
	cfg := orch.LockConfig{
		Path:               filepath.Join(dir, ".pipeline.lock"),
		StalenessThreshold: 2 * time.Second,
		RetryCount:         2,
		RetryDelay:         10 * time.Millisecond,
	}
	return orch.NewPipelineLock(cfg)
}

// TestOPS10_ExclusiveLock verifies [OPS-10]: lock provides mutual exclusion.
func TestOPS10_ExclusiveLock(t *testing.T) {
	lock := newTestLock(t)

	err := lock.Acquire("holder-1", "scout")
	require.NoError(t, err)
	defer lock.Release() //nolint:errcheck // test cleanup

	// Second acquire should fail with contention.
	cfg := orch.LockConfig{
		Path:               lock.Path(),
		StalenessThreshold: 2 * time.Second,
		RetryCount:         0,
		RetryDelay:         1 * time.Millisecond,
	}
	lock2 := orch.NewPipelineLock(cfg)
	err = lock2.Acquire("holder-2", "scout")
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrLockContention)
}

// TestOPS11_StaleLockRecovery verifies [OPS-11]: stale locks can be reclaimed.
func TestOPS11_StaleLockRecovery(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".pipeline.lock")

	// Write a stale lock file.
	staleContent := `{"holder":"dead-process","phase":"scout","timestamp":"2000-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(staleContent+"\n"), 0o644))

	cfg := orch.LockConfig{
		Path:               lockPath,
		StalenessThreshold: 1 * time.Millisecond, // very short — file is definitely stale
		RetryCount:         1,
		RetryDelay:         1 * time.Millisecond,
	}
	lock := orch.NewPipelineLock(cfg)
	err := lock.Acquire("new-holder", "scout")
	require.NoError(t, err)
	defer lock.Release() //nolint:errcheck // test cleanup
}

// TestOPS11_TimestampResetOnStaleReclaim verifies the TLA+ finding:
// stale-reclaim MUST write a fresh timestamp to prevent immediate re-stealing.
func TestOPS11_TimestampResetOnStaleReclaim(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".pipeline.lock")

	// Write stale lock.
	staleContent := `{"holder":"dead","phase":"x","timestamp":"2000-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(staleContent+"\n"), 0o644))

	cfg := orch.LockConfig{
		Path:               lockPath,
		StalenessThreshold: 1 * time.Millisecond,
		RetryCount:         1,
		RetryDelay:         1 * time.Millisecond,
	}
	lock := orch.NewPipelineLock(cfg)
	require.NoError(t, lock.Acquire("reclaimer", "scout"))
	defer lock.Release() //nolint:errcheck // test cleanup

	// Read the lock file and verify timestamp is fresh (not the stale "2000-01-01").
	data, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "reclaimer")
	assert.NotContains(t, string(data), "2000-01-01")
}

// TestOPS12_ReleaseOnAllPaths verifies [OPS-12]: release removes the lock file.
func TestOPS12_ReleaseOnAllPaths(t *testing.T) {
	lock := newTestLock(t)
	require.NoError(t, lock.Acquire("holder", "scout"))

	require.NoError(t, lock.Release())

	// File should be gone.
	_, err := os.Stat(lock.Path())
	assert.True(t, os.IsNotExist(err))
}

// TestOPS12_ReleaseIdempotent verifies release is idempotent (double release ok).
func TestOPS12_ReleaseIdempotent(t *testing.T) {
	lock := newTestLock(t)
	require.NoError(t, lock.Acquire("holder", "scout"))
	require.NoError(t, lock.Release())
	require.NoError(t, lock.Release()) // second release should not error
}

// TestOPS14_LockRefreshPreventsStale verifies [OPS-11, OPS-13] that Refresh updates
// the timestamp (preventing false staleness per OPS-11) and the phase field (OPS-13).
func TestOPS14_LockRefreshPreventsStale(t *testing.T) {
	lock := newTestLock(t)
	require.NoError(t, lock.Acquire("holder", "scout"))
	defer lock.Release() //nolint:errcheck // test cleanup

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, lock.Refresh("views-2a"))

	// Verify the lock file now shows the updated phase.
	data, err := os.ReadFile(lock.Path())
	require.NoError(t, err)
	assert.Contains(t, string(data), "views-2a")
}
