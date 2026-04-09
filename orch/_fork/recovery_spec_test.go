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

// --- Retry Protocol Tests (ERR-01..06) ---

// TestERR01_RetryWithScaledTimeout verifies [ERR-01]: retry uses scaled timeout.
func TestERR01_RetryWithScaledTimeout(t *testing.T) {
	base := 10 * time.Second
	// attempt 0 → 1.0x, attempt 1 → 1.5x, attempt 2 → 2.0x
	assert.Equal(t, 10*time.Second, orch.ScaledTimeout(base, 0))
	assert.Equal(t, 15*time.Second, orch.ScaledTimeout(base, 1))
	assert.Equal(t, 20*time.Second, orch.ScaledTimeout(base, 2))
}

// TestERR01_RetryJitterBounded verifies RetryJitter adds jitter in [0, d/4].
func TestERR01_RetryJitterBounded(t *testing.T) {
	base := 100 * time.Millisecond
	for range 1000 {
		result := orch.RetryJitter(base)
		assert.GreaterOrEqual(t, result, base)
		assert.LessOrEqual(t, result, base+base/4)
	}
}

// TestERR01_RetryJitterZero verifies RetryJitter handles zero duration.
func TestERR01_RetryJitterZero(t *testing.T) {
	assert.Equal(t, time.Duration(0), orch.RetryJitter(0))
}

// TestERR01_RetryJitterSmallDuration verifies RetryJitter does not panic for small durations
// where d/4 rounds to zero (PR #6 review).
func TestERR01_RetryJitterSmallDuration(t *testing.T) {
	for d := time.Duration(1); d <= 4; d++ {
		result := orch.RetryJitter(d)
		assert.Equal(t, d, result, "small duration %d should be returned unchanged", d)
	}
	// Negative durations return 0.
	assert.Equal(t, time.Duration(0), orch.RetryJitter(-1))
}

// TestERR01_CanRetryExhaustion verifies [ERR-01]: retries are bounded.
func TestERR01_CanRetryExhaustion(t *testing.T) {
	cfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}
	rs := orch.NewRetryState()

	assert.True(t, rs.CanRetry("agent-a", cfg))
	rs.RecordAttempt("agent-a")
	assert.True(t, rs.CanRetry("agent-a", cfg))
	rs.RecordAttempt("agent-a")
	assert.False(t, rs.CanRetry("agent-a", cfg), "should be exhausted after 2 attempts")
}

// TestERR02_RetryDeterministic verifies [ERR-02]: retry is deterministic.
func TestERR02_RetryDeterministic(t *testing.T) {
	rs := orch.NewRetryState()
	rs.RecordAttempt("agent-x")
	rs.RecordAttempt("agent-x")
	assert.Equal(t, 2, rs.AttemptCount("agent-x"))
	assert.Equal(t, 0, rs.AttemptCount("agent-y"), "unrelated agent should be 0")
}

// TestERR06_SessionRestartNotRetry verifies [ERR-06, ERR-05]: session restart ≠ retry attempt.
// Watchdog and retry are orthogonal (ERR-05). Retry state is keyed by agent ID and only
// incremented by RecordAttempt.
func TestERR06_SessionRestartNotRetry(t *testing.T) {
	cfg := orch.RetryConfig{MaxRetries: 1}
	rs := orch.NewRetryState()

	// Simulate a session restart (watchdog) — does not call RecordAttempt.
	assert.True(t, rs.CanRetry("agent-a", cfg), "should still have retries after session restart")
	assert.Equal(t, 0, rs.AttemptCount("agent-a"))

	// Actual retry (dispatch check step) calls RecordAttempt.
	rs.RecordAttempt("agent-a")
	assert.False(t, rs.CanRetry("agent-a", cfg))
}

// --- Gap Tracker Tests (ERR-07, ERR-08, S5) ---

// TestERR07_MonotonicGapAccumulation verifies [ERR-07]: gap count is monotonically non-decreasing.
func TestERR07_MonotonicGapAccumulation(t *testing.T) {
	gt := orch.NewGapTracker(5)
	prev := gt.Count()
	for _, agent := range []string{"a", "b", "c"} {
		gt.IncrementAndCheck(agent)
		assert.GreaterOrEqual(t, gt.Count(), prev, "gaps must be monotonically non-decreasing")
		prev = gt.Count()
	}
	assert.Equal(t, []string{"a", "b", "c"}, gt.Agents())
}

// TestERR08_GapToleranceAbort verifies [ERR-08]: gap tolerance reached → abort.
func TestERR08_GapToleranceAbort(t *testing.T) {
	gt := orch.NewGapTracker(3)
	assert.False(t, gt.IncrementAndCheck("a1"))
	assert.False(t, gt.IncrementAndCheck("a2"))
	assert.True(t, gt.IncrementAndCheck("a3"), "should abort at tolerance")
	assert.Equal(t, 3, gt.Count())
}

// TestERR08_GapToleranceOne verifies abort at tolerance=1.
func TestERR08_GapToleranceOne(t *testing.T) {
	gt := orch.NewGapTracker(1)
	assert.True(t, gt.IncrementAndCheck("a1"), "tolerance=1 should abort on first gap")
}

// TestS5_GapBoundInvariant verifies [S5]: gaps < tolerance while pipeline running.
func TestS5_GapBoundInvariant(t *testing.T) {
	gt := orch.NewGapTracker(3)

	// Before tolerance: gaps < tolerance.
	gt.IncrementAndCheck("a")
	assert.Less(t, gt.Count(), 3)

	gt.IncrementAndCheck("b")
	assert.Less(t, gt.Count(), 3)

	// At tolerance: abort signal returned, so pipeline would stop.
	abort := gt.IncrementAndCheck("c")
	assert.True(t, abort)
}

// --- Crash Marker Tests (CHK-04, CHK-05) ---

// TestCHK04_WriteCrashMarker verifies [CHK-04, CHK-02]: crash marker written before analysis.
// Per-agent status markers (CHK-02) are implemented as crash markers at the output path.
func TestCHK04_WriteCrashMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.md")

	require.NoError(t, orch.WriteCrashMarker(path))

	// Verify file exists and is a crash marker.
	isCrash, err := orch.IsCrashMarker(path)
	require.NoError(t, err)
	assert.True(t, isCrash)

	// Verify it fails md validation (no '#' header, < 100 bytes).
	err = orch.ValidateOutput(path, orch.FormatMd)
	assert.Error(t, err, "crash marker should fail format validation")
}

// TestCHK05_CrashMarkerDeleteRerun verifies [CHK-05]: crash marker → delete + re-run on resume.
func TestCHK05_CrashMarkerDeleteRerun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.md")

	require.NoError(t, orch.WriteCrashMarker(path))

	// Detect.
	isCrash, err := orch.IsCrashMarker(path)
	require.NoError(t, err)
	assert.True(t, isCrash)

	// Remove.
	require.NoError(t, orch.RemoveCrashMarker(path))

	// Verify gone.
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))

	// IsCrashMarker on non-existent file returns false, nil.
	isCrash, err = orch.IsCrashMarker(path)
	require.NoError(t, err)
	assert.False(t, isCrash)
}

// TestCHK05_RemoveCrashMarkerIdempotent verifies RemoveCrashMarker is idempotent.
func TestCHK05_RemoveCrashMarkerIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.md")
	assert.NoError(t, orch.RemoveCrashMarker(path), "removing non-existent marker should not error")
}

// TestGapTracker_SetGaps verifies SetGaps for resume recovery.
func TestGapTracker_SetGaps(t *testing.T) {
	gt := orch.NewGapTracker(5)
	gt.SetGaps(3, []string{"a", "b", "c"})
	assert.Equal(t, 3, gt.Count())
	assert.Equal(t, []string{"a", "b", "c"}, gt.Agents())

	// Next gap should make it 4 (not reset).
	abort := gt.IncrementAndCheck("d")
	assert.False(t, abort)
	assert.Equal(t, 4, gt.Count())
}
