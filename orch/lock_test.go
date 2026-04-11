//go:build verify

package orch_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLock(t *testing.T) *orch.LifecycleLock {
	t.Helper()
	dir := t.TempDir()
	cfg := orch.LockConfig{
		Path:               filepath.Join(dir, ".wonka-test.lock"),
		StalenessThreshold: 2 * time.Second,
		RetryCount:         2,
		RetryDelay:         10 * time.Millisecond,
	}
	return orch.NewLifecycleLock(cfg)
}

// TestBVV_S01_ExclusiveLock verifies BVV-S-01: per-branch lifecycle exclusion.
// Formerly OPS-10 in the fork; the BVV ID tracks the single-lifecycle-per-branch
// invariant from the safety section of the spec.
func TestBVV_S01_ExclusiveLock(t *testing.T) {
	lock := newTestLock(t)

	err := lock.Acquire("holder-1", "feature-x")
	require.NoError(t, err)
	defer lock.Release() //nolint:errcheck // test cleanup

	// Second acquire should fail with contention.
	cfg := orch.LockConfig{
		Path:               lock.Path(),
		StalenessThreshold: 2 * time.Second,
		RetryCount:         0,
		RetryDelay:         1 * time.Millisecond,
	}
	lock2 := orch.NewLifecycleLock(cfg)
	err = lock2.Acquire("holder-2", "feature-x")
	require.Error(t, err)
	assert.ErrorIs(t, err, orch.ErrLockContention)
}

// TestBVV_ERR06_StaleLockRecovery verifies BVV-ERR-06: stale locks from dead
// orchestrator processes can be reclaimed by a resume attempt. Formerly OPS-11.
func TestBVV_ERR06_StaleLockRecovery(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wonka-test.lock")

	// Write a stale lock file.
	staleContent := `{"holder":"dead-process","branch":"feature-x","timestamp":"2000-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(staleContent+"\n"), 0o644))

	cfg := orch.LockConfig{
		Path:               lockPath,
		StalenessThreshold: 1 * time.Millisecond, // very short — file is definitely stale
		RetryCount:         1,
		RetryDelay:         1 * time.Millisecond,
	}
	lock := orch.NewLifecycleLock(cfg)
	err := lock.Acquire("new-holder", "feature-x")
	require.NoError(t, err)
	defer lock.Release() //nolint:errcheck // test cleanup
}

// TestBVV_ERR06_TimestampResetOnStaleReclaim verifies the TLA+ finding:
// stale-reclaim MUST write a fresh timestamp to prevent immediate re-stealing
// by a third process that also considers the lock stale.
func TestBVV_ERR06_TimestampResetOnStaleReclaim(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wonka-test.lock")

	// Write stale lock.
	staleContent := `{"holder":"dead","branch":"x","timestamp":"2000-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(staleContent+"\n"), 0o644))

	cfg := orch.LockConfig{
		Path:               lockPath,
		StalenessThreshold: 1 * time.Millisecond,
		RetryCount:         1,
		RetryDelay:         1 * time.Millisecond,
	}
	lock := orch.NewLifecycleLock(cfg)
	require.NoError(t, lock.Acquire("reclaimer", "feature-x"))
	defer lock.Release() //nolint:errcheck // test cleanup

	// Read the lock file and verify timestamp is fresh (not the stale "2000-01-01").
	data, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "reclaimer")
	assert.NotContains(t, string(data), "2000-01-01")
}

// TestBVV_ERR10a_ReleaseRemovesFile verifies that Release removes the lock
// file. The BVV-ERR-10a drain precondition is enforced at the dispatcher call
// site under build tag verify (Phase 5 invariant), not inside Release itself.
// Formerly OPS-12.
func TestBVV_ERR10a_ReleaseRemovesFile(t *testing.T) {
	lock := newTestLock(t)
	require.NoError(t, lock.Acquire("holder", "feature-x"))

	require.NoError(t, lock.Release())

	// File should be gone.
	_, err := os.Stat(lock.Path())
	assert.True(t, os.IsNotExist(err))
}

// TestBVV_ERR10a_ReleaseIdempotent verifies release is idempotent — the signal
// handler path calls Release regardless of whether the dispatcher already did.
func TestBVV_ERR10a_ReleaseIdempotent(t *testing.T) {
	lock := newTestLock(t)
	require.NoError(t, lock.Acquire("holder", "feature-x"))
	require.NoError(t, lock.Release())
	require.NoError(t, lock.Release()) // second release should not error
}

// TestBVV_L02_RefreshPreventsStaleness verifies BVV-L-02: Refresh updates the
// timestamp so the watchdog doesn't age the lock past the staleness threshold
// while the orchestrator is alive. Also verifies that the branch field
// updates (useful if a resume binds a different branch to the same lock path —
// though in practice lock path is scoped by branch).
func TestBVV_L02_RefreshPreventsStaleness(t *testing.T) {
	lock := newTestLock(t)
	require.NoError(t, lock.Acquire("holder", "feature-x"))
	defer lock.Release() //nolint:errcheck // test cleanup

	time.Sleep(10 * time.Millisecond)

	// Capture the refresh boundary. The file's Timestamp MUST be at or
	// after this instant, proving Refresh actually re-wrote it instead of
	// silently leaving the stale Acquire timestamp in place.
	beforeRefresh := time.Now()
	require.NoError(t, lock.Refresh("feature-x"))

	data, err := os.ReadFile(lock.Path())
	require.NoError(t, err)

	// A silent no-op Refresh would leave Timestamp at the Acquire value
	// (10ms+ earlier than beforeRefresh), failing the first assertion.
	var c orch.LockContent
	require.NoError(t, json.Unmarshal(data, &c))
	assert.Equal(t, "holder", c.Holder)
	assert.Equal(t, "feature-x", c.Branch)
	assert.False(t, c.Timestamp.Before(beforeRefresh),
		"Refresh must advance Timestamp to at least the call instant; got %s, wanted >= %s",
		c.Timestamp, beforeRefresh)
	assert.Less(t, time.Since(c.Timestamp), 1*time.Second,
		"Refresh Timestamp must be within 1s of now; got age %s", time.Since(c.Timestamp))
}

// TestBVV_ERR06_StaleLockRemoveErrorSurfaces verifies that a real filesystem
// failure during stale-lock removal is not silently reported as generic
// lock contention. A chmod'd parent directory forces os.Remove → EACCES;
// Acquire must surface the underlying FS error.
//
// Skipped as root because root bypasses directory write permissions on POSIX.
func TestBVV_ERR06_StaleLockRemoveErrorSurfaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based permission test is POSIX-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; test cannot trigger EACCES")
	}

	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wonka-test.lock")

	// Write a stale lock file.
	staleContent := `{"holder":"dead-process","branch":"feature-x","timestamp":"2000-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(staleContent+"\n"), 0o644))

	// Drop write permission on the parent dir so os.Remove on the lock
	// file fails with EACCES. Restore in t.Cleanup so the tempdir cleanup
	// can actually run.
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	cfg := orch.LockConfig{
		Path:               lockPath,
		StalenessThreshold: 1 * time.Millisecond, // lock is definitely stale
		RetryCount:         1,
		RetryDelay:         1 * time.Millisecond,
	}
	lock := orch.NewLifecycleLock(cfg)

	err := lock.Acquire("new-holder", "feature-x")
	require.Error(t, err, "stale-lock Remove must surface FS errors, not succeed")
	assert.NotErrorIs(t, err, orch.ErrLockContention,
		"real FS failure must not masquerade as lock contention")
	assert.Contains(t, err.Error(), "remove stale lock",
		"error must identify the failing operation")
}

// TestBVV_L02_RefreshRejectsWrongHolder verifies that Refresh guards against
// a different holder hijacking the lock: the holderID field on the in-memory
// lock is compared against the on-disk holder before the write.
func TestBVV_L02_RefreshRejectsWrongHolder(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wonka-test.lock")

	cfg := orch.LockConfig{
		Path:               lockPath,
		StalenessThreshold: 2 * time.Second,
		RetryCount:         0,
		RetryDelay:         1 * time.Millisecond,
	}

	// Holder A acquires.
	lockA := orch.NewLifecycleLock(cfg)
	require.NoError(t, lockA.Acquire("holder-a", "feature-x"))

	// External actor overwrites the file with a different holder (simulating
	// a stale-recovery race where another orchestrator reclaimed the lock).
	hijack := `{"holder":"holder-b","branch":"feature-x","timestamp":"` + time.Now().Format(time.RFC3339) + `"}`
	require.NoError(t, os.WriteFile(lockPath, []byte(hijack+"\n"), 0o644))

	// Holder A's refresh must fail — it thinks it still holds the lock but
	// the on-disk holder is now B.
	err := lockA.Refresh("feature-x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holder mismatch")
}
