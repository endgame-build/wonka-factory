package orch_test

import (
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"
)

// TestProp_GapMonotonic verifies [ERR-07]: gap count is monotonically non-decreasing
// for any sequence of IncrementAndCheck calls.
func TestProp_GapMonotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tolerance := rapid.IntRange(1, 20).Draw(t, "tolerance")
		nGaps := rapid.IntRange(0, 30).Draw(t, "nGaps")
		gt := orch.NewGapTracker(tolerance)

		prev := 0
		for i := range nGaps {
			gt.IncrementAndCheck(rapid.StringMatching(`^[a-z]{2}-\d{2}$`).Draw(t, "agent"))
			assert.GreaterOrEqual(t, gt.Count(), prev, "gap %d: count must be non-decreasing", i)
			prev = gt.Count()
		}
	})
}

// TestProp_GapAbortExact verifies [ERR-08]: abort fires at exactly the tolerance count.
func TestProp_GapAbortExact(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tolerance := rapid.IntRange(1, 20).Draw(t, "tolerance")
		gt := orch.NewGapTracker(tolerance)

		for i := range tolerance - 1 {
			abort := gt.IncrementAndCheck("agent-" + string(rune('a'+i%26)))
			assert.False(t, abort, "gap %d/%d should not abort", i+1, tolerance)
		}
		abort := gt.IncrementAndCheck("agent-final")
		assert.True(t, abort, "gap %d/%d should abort", tolerance, tolerance)
		assert.Equal(t, tolerance, gt.Count())
	})
}

// TestProp_RetryBounded verifies [ERR-01]: retry count per agent never exceeds MaxRetries.
func TestProp_RetryBounded(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxRetries := rapid.IntRange(0, 5).Draw(t, "maxRetries")
		cfg := orch.RetryConfig{MaxRetries: maxRetries}
		rs := orch.NewRetryState()

		nAgents := rapid.IntRange(1, 10).Draw(t, "nAgents")
		agents := make([]string, nAgents)
		for i := range nAgents {
			agents[i] = rapid.StringMatching(`^[a-z]{2}-\d{2}$`).Draw(t, "agent")
		}

		// Randomly attempt retries.
		nOps := rapid.IntRange(0, 50).Draw(t, "nOps")
		for range nOps {
			agent := agents[rapid.IntRange(0, nAgents-1).Draw(t, "idx")]
			if rs.CanRetry(agent, cfg) {
				rs.RecordAttempt(agent)
			}
			assert.LessOrEqual(t, rs.AttemptCount(agent), maxRetries,
				"agent %s: attempts must not exceed MaxRetries", agent)
		}
	})
}
