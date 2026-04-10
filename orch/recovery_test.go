//go:build verify

package orch_test

import (
	"sync"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Retry Protocol Tests (BVV-ERR-01, BVV-ERR-02a) ---

// TestBVV_ERR02a_ScaledTimeout verifies retry uses scaled timeout:
//
//	timeout(attempt) = base_timeout * (1.0 + 0.5 * attempt)
func TestBVV_ERR02a_ScaledTimeout(t *testing.T) {
	base := 10 * time.Second
	assert.Equal(t, 10*time.Second, orch.ScaledTimeout(base, 0))
	assert.Equal(t, 15*time.Second, orch.ScaledTimeout(base, 1))
	assert.Equal(t, 20*time.Second, orch.ScaledTimeout(base, 2))
}

// TestBVV_ERR02a_RetryJitterBounded verifies RetryJitter adds jitter in [0, d/4].
func TestBVV_ERR02a_RetryJitterBounded(t *testing.T) {
	base := 100 * time.Millisecond
	for range 1000 {
		result := orch.RetryJitter(base)
		assert.GreaterOrEqual(t, result, base)
		assert.LessOrEqual(t, result, base+base/4)
	}
}

// TestBVV_ERR02a_RetryJitterZero verifies RetryJitter handles zero duration.
func TestBVV_ERR02a_RetryJitterZero(t *testing.T) {
	assert.Equal(t, time.Duration(0), orch.RetryJitter(0))
}

// TestBVV_ERR02a_RetryJitterSmallDuration verifies RetryJitter does not panic
// for small durations where d/4 rounds to zero.
func TestBVV_ERR02a_RetryJitterSmallDuration(t *testing.T) {
	for d := time.Duration(1); d <= 4; d++ {
		result := orch.RetryJitter(d)
		assert.Equal(t, d, result, "small duration %d should be returned unchanged", d)
	}
	assert.Equal(t, time.Duration(0), orch.RetryJitter(-1))
}

// TestBVV_ERR01_CanRetryExhaustion verifies BVV-ERR-01: retries are bounded
// per task. Note the rekeying from agent ID (fork) to task ID (BVV) — BVV
// retries reset the same task entity instead of creating a new one.
func TestBVV_ERR01_CanRetryExhaustion(t *testing.T) {
	cfg := orch.RetryConfig{MaxRetries: 2, BaseTimeout: time.Second}
	rs := orch.NewRetryState()

	assert.True(t, rs.CanRetry("task-001", cfg))
	rs.RecordAttempt("task-001")
	assert.True(t, rs.CanRetry("task-001", cfg))
	rs.RecordAttempt("task-001")
	assert.False(t, rs.CanRetry("task-001", cfg), "should be exhausted after 2 attempts")
}

// TestBVV_ERR01_RetryDeterministic verifies that retry counts are per-task
// and don't leak between tasks.
func TestBVV_ERR01_RetryDeterministic(t *testing.T) {
	rs := orch.NewRetryState()
	rs.RecordAttempt("task-x")
	rs.RecordAttempt("task-x")
	assert.Equal(t, 2, rs.AttemptCount("task-x"))
	assert.Equal(t, 0, rs.AttemptCount("task-y"), "unrelated task should be 0")
}

// TestBVV_ERR11_SessionRestartNotRetry verifies the orthogonality of
// watchdog-driven session restarts and retry attempts (BVV-ERR-11 vs
// BVV-ERR-01). A session restart does not consume retry budget — only the
// dispatcher's exit-code-1 path calls RecordAttempt.
func TestBVV_ERR11_SessionRestartNotRetry(t *testing.T) {
	cfg := orch.RetryConfig{MaxRetries: 1}
	rs := orch.NewRetryState()

	// Simulate a watchdog session restart — does not call RecordAttempt.
	assert.True(t, rs.CanRetry("task-a", cfg), "session restart must not consume retry budget")
	assert.Equal(t, 0, rs.AttemptCount("task-a"))

	// Actual retry (dispatch check step) calls RecordAttempt.
	rs.RecordAttempt("task-a")
	assert.False(t, rs.CanRetry("task-a", cfg))
}

// TestBVV_ERR01_DefaultRetryConfig locks in the CLI defaults that the Phase 7
// flag binding will rely on.
func TestBVV_ERR01_DefaultRetryConfig(t *testing.T) {
	cfg := orch.DefaultRetryConfig()
	assert.Equal(t, 2, cfg.MaxRetries)
	assert.Equal(t, 30*time.Minute, cfg.BaseTimeout)
}

// --- Gap Tracker Tests (BVV-ERR-03, BVV-ERR-04, BVV-ERR-05, S7) ---

// TestBVV_ERR05_MonotonicGapAccumulation verifies BVV-ERR-05: gap count is
// monotonically non-decreasing within a lifecycle.
func TestBVV_ERR05_MonotonicGapAccumulation(t *testing.T) {
	gt := orch.NewGapTracker(5)
	prev := gt.Count()
	for _, taskID := range []string{"t1", "t2", "t3"} {
		gt.IncrementAndCheck(taskID)
		assert.GreaterOrEqual(t, gt.Count(), prev, "gaps must be monotonically non-decreasing")
		prev = gt.Count()
	}
	assert.Equal(t, []string{"t1", "t2", "t3"}, gt.TaskIDs())
}

// TestBVV_ERR04_GapToleranceAbort verifies BVV-ERR-04: gap tolerance reached
// triggers lifecycle abort.
func TestBVV_ERR04_GapToleranceAbort(t *testing.T) {
	gt := orch.NewGapTracker(3)
	assert.False(t, gt.IncrementAndCheck("t1"))
	assert.False(t, gt.IncrementAndCheck("t2"))
	assert.True(t, gt.IncrementAndCheck("t3"), "should abort at tolerance")
	assert.Equal(t, 3, gt.Count())
}

// TestBVV_ERR04_GapToleranceOne verifies abort at tolerance=1.
func TestBVV_ERR04_GapToleranceOne(t *testing.T) {
	gt := orch.NewGapTracker(1)
	assert.True(t, gt.IncrementAndCheck("t1"), "tolerance=1 should abort on first gap")
}

// TestBVV_S7_GapBoundInvariant verifies S7: gaps < tolerance while the
// lifecycle is running.
func TestBVV_S7_GapBoundInvariant(t *testing.T) {
	gt := orch.NewGapTracker(3)

	gt.IncrementAndCheck("t1")
	assert.Less(t, gt.Count(), 3)

	gt.IncrementAndCheck("t2")
	assert.Less(t, gt.Count(), 3)

	abort := gt.IncrementAndCheck("t3")
	assert.True(t, abort)
}

// TestBVV_ERR05_GapTrackerSetGaps verifies SetGaps restores state from a
// prior session's event log (Phase 5 Resume path).
func TestBVV_ERR05_GapTrackerSetGaps(t *testing.T) {
	gt := orch.NewGapTracker(5)
	gt.SetGaps(3, []string{"t1", "t2", "t3"})
	assert.Equal(t, 3, gt.Count())
	assert.Equal(t, []string{"t1", "t2", "t3"}, gt.TaskIDs())

	// Next gap should make it 4 (not reset).
	abort := gt.IncrementAndCheck("t4")
	assert.False(t, abort)
	assert.Equal(t, 4, gt.Count())
}

// --- HandoffState Tests (BVV-L-04, BVV-ERR-11a, BVV-S-02a) ---

// TestBVV_L04_HandoffStateCanHandoff verifies the limit boundary: a task at
// the limit cannot hand off again.
func TestBVV_L04_HandoffStateCanHandoff(t *testing.T) {
	h := orch.NewHandoffState(2)

	assert.True(t, h.CanHandoff("task-1"))
	h.RecordHandoff("task-1")
	assert.True(t, h.CanHandoff("task-1"))
	h.RecordHandoff("task-1")
	assert.False(t, h.CanHandoff("task-1"), "at limit, further handoffs must be rejected")
}

// TestBVV_L04_HandoffStateRecordAndCount verifies RecordHandoff + Count
// produce the expected counter, and counts are keyed independently per task.
func TestBVV_L04_HandoffStateRecordAndCount(t *testing.T) {
	h := orch.NewHandoffState(10)

	h.RecordHandoff("task-a")
	h.RecordHandoff("task-a")
	h.RecordHandoff("task-b")

	assert.Equal(t, 2, h.Count("task-a"))
	assert.Equal(t, 1, h.Count("task-b"))
	assert.Equal(t, 0, h.Count("task-c"), "untouched task should be 0")
}

// TestBVV_L04_HandoffStateZeroLimit verifies edge case: limit 0 means "no
// handoffs allowed" — CanHandoff returns false on the very first call.
func TestBVV_L04_HandoffStateZeroLimit(t *testing.T) {
	h := orch.NewHandoffState(0)
	assert.False(t, h.CanHandoff("task-1"))
}

// TestBVV_L04_HandoffStateConcurrentWrites stress-tests the mutex contract:
// two goroutines each call RecordHandoff N times on the same task ID. The
// final count must be exactly 2N. This test is load-bearing for the BVV
// ownership contract that dispatcher (exit-3 tick) and watchdog (dead-session
// tick) are both writers — running under -race catches mutex regressions.
func TestBVV_L04_HandoffStateConcurrentWrites(t *testing.T) {
	h := orch.NewHandoffState(10000)
	const n = 500

	var wg sync.WaitGroup
	wg.Add(2)

	worker := func() {
		defer wg.Done()
		for range n {
			h.RecordHandoff("task-hot")
		}
	}
	go worker()
	go worker()
	wg.Wait()

	assert.Equal(t, 2*n, h.Count("task-hot"), "concurrent RecordHandoff must not lose increments")
}

// TestBVV_S02a_HandoffStateReset verifies BVV-S-02a: human re-opening a
// terminal task zeros the handoff counter for that task without leaking to
// other task IDs.
func TestBVV_S02a_HandoffStateReset(t *testing.T) {
	h := orch.NewHandoffState(3)

	h.RecordHandoff("task-reopened")
	h.RecordHandoff("task-reopened")
	h.RecordHandoff("task-untouched")
	require.Equal(t, 2, h.Count("task-reopened"))
	require.Equal(t, 1, h.Count("task-untouched"))

	h.Reset("task-reopened")

	assert.Equal(t, 0, h.Count("task-reopened"), "reset task should be 0")
	assert.Equal(t, 1, h.Count("task-untouched"), "other task must be unaffected by Reset")
	assert.True(t, h.CanHandoff("task-reopened"), "reset task has full budget again")
}

// TestBVV_ERR11a_HandoffStateSetCountsNil verifies that SetCounts(nil) is
// safe: the internal map is normalised to empty (not left nil), so the next
// RecordHandoff call doesn't panic on a nil-map write. This covers the
// normalisation branch introduced when SetCounts was refactored to use
// maps.Clone (which returns nil on nil input).
func TestBVV_ERR11a_HandoffStateSetCountsNil(t *testing.T) {
	h := orch.NewHandoffState(5)
	h.SetCounts(nil)

	// Internal map must be non-nil — a subsequent RecordHandoff must not
	// panic. Assertion is by behavior, not by inspection.
	assert.NotPanics(t, func() {
		h.RecordHandoff("task-after-nil-reset")
	})
	assert.Equal(t, 1, h.Count("task-after-nil-reset"))
}

// TestBVV_ERR11a_HandoffStateSetCounts verifies SetCounts restores state from
// a prior session's event log (Phase 5 Resume path for BVV-L-04 persistence
// across orchestrator restarts).
func TestBVV_ERR11a_HandoffStateSetCounts(t *testing.T) {
	h := orch.NewHandoffState(5)
	h.SetCounts(map[string]int{
		"task-a": 2,
		"task-b": 4,
	})

	assert.Equal(t, 2, h.Count("task-a"))
	assert.Equal(t, 4, h.Count("task-b"))
	assert.True(t, h.CanHandoff("task-a"), "still has budget after replay")
	assert.True(t, h.CanHandoff("task-b"), "exactly at limit-1 after replay")

	// Next handoff on task-b hits the limit.
	h.RecordHandoff("task-b")
	assert.False(t, h.CanHandoff("task-b"), "replayed count + new increment hit limit")
}
