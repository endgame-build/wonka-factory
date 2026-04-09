package orch_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOPS19_StructuredAuditEvents verifies [OPS-19]: events are structured JSONL.
func TestOPS19_StructuredAuditEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	el, err := orch.NewEventLog(path)
	require.NoError(t, err)
	defer el.Close()

	err = el.Emit(orch.Event{
		Kind:    orch.EventAgentStart,
		Phase:   "scout",
		Agent:   "00-scout",
		TaskID:  "00-scout",
		Worker:  "w-01",
		Summary: "agent started",
	})
	require.NoError(t, err)

	// Read back and verify it's valid JSON.
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var e orch.Event
	err = json.Unmarshal(data[:len(data)-1], &e) // strip trailing newline
	require.NoError(t, err)

	assert.Equal(t, orch.EventAgentStart, e.Kind)
	assert.Equal(t, "scout", e.Phase)
	assert.Equal(t, "00-scout", e.Agent)
	assert.Equal(t, "w-01", e.Worker)
	assert.False(t, e.Timestamp.IsZero(), "timestamp should be auto-set")
}

// TestOPS19_TimestampAutoSet verifies Emit sets Timestamp when zero.
func TestOPS19_TimestampAutoSet(t *testing.T) {
	dir := t.TempDir()
	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	before := time.Now()
	err = el.Emit(orch.Event{Kind: orch.EventPhaseStart, Summary: "test"})
	require.NoError(t, err)
	after := time.Now()

	data, err := os.ReadFile(el.Path())
	require.NoError(t, err)

	var e orch.Event
	require.NoError(t, json.Unmarshal(data[:len(data)-1], &e))
	assert.True(t, !e.Timestamp.Before(before) && !e.Timestamp.After(after))
}

// TestOPS19_TimestampPreserved verifies Emit preserves a non-zero Timestamp.
func TestOPS19_TimestampPreserved(t *testing.T) {
	dir := t.TempDir()
	el, err := orch.NewEventLog(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer el.Close()

	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	err = el.Emit(orch.Event{Timestamp: fixed, Kind: orch.EventShutdown, Summary: "bye"})
	require.NoError(t, err)

	data, err := os.ReadFile(el.Path())
	require.NoError(t, err)

	var e orch.Event
	require.NoError(t, json.Unmarshal(data[:len(data)-1], &e))
	assert.True(t, e.Timestamp.Equal(fixed))
}

// TestOPS19_ConcurrentEmit verifies thread-safety of Emit under concurrent writes.
func TestOPS19_ConcurrentEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	el, err := orch.NewEventLog(path)
	require.NoError(t, err)
	defer el.Close()

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_ = el.Emit(orch.Event{
				Kind:    orch.EventAgentComplete,
				Summary: "done",
				TaskID:  string(rune('A' + i%26)),
			})
		}(i)
	}
	wg.Wait()

	// Verify all lines are valid JSON.
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		var e orch.Event
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &e), "line %d should be valid JSON", count)
		count++
	}
	require.NoError(t, scanner.Err())
	assert.Equal(t, n, count, "all events should be persisted")
}

// TestOPS19_ReopenAppend verifies that reopening the log appends, not overwrites.
func TestOPS19_ReopenAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// First session.
	el1, err := orch.NewEventLog(path)
	require.NoError(t, err)
	require.NoError(t, el1.Emit(orch.Event{Kind: orch.EventPhaseStart, Summary: "first"}))
	require.NoError(t, el1.Close())

	// Second session (reopen).
	el2, err := orch.NewEventLog(path)
	require.NoError(t, err)
	require.NoError(t, el2.Emit(orch.Event{Kind: orch.EventPhaseComplete, Summary: "second"}))
	require.NoError(t, el2.Close())

	// Count lines.
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count++
	}
	assert.Equal(t, 2, count, "both events should be present")
}

// TestOPS20_MandatoryEventKinds verifies [OPS-20]: all mandatory event kinds are defined.
func TestOPS20_MandatoryEventKinds(t *testing.T) {
	mandatory := []orch.EventKind{
		orch.EventPhaseStart,
		orch.EventPhaseComplete,
		orch.EventAgentStart,
		orch.EventAgentComplete,
		orch.EventConsensusMerge,
		orch.EventConsensusVerify,
		orch.EventGateResult,
		orch.EventGapRecorded,
		orch.EventPipelineComplete,
		orch.EventCircuitBreaker,
		orch.EventCrashDetected,
		orch.EventSessionRestart,
		orch.EventRetryScheduled,
		orch.EventShutdown,
	}
	for _, kind := range mandatory {
		assert.NotEmpty(t, string(kind), "event kind should be non-empty")
	}
}

// TestOPS17b_CleanupFailureNotPipelineFailure verifies [OPS-17b]: cleanup failure does not
// fail the pipeline. Cleanup errors are logged but pipeline.status stays completed.
func TestOPS17b_CleanupFailureNotPipelineFailure(t *testing.T) {
	dir := t.TempDir()

	// Create an event log.
	logPath := filepath.Join(dir, "events.jsonl")
	el, err := orch.NewEventLog(logPath)
	require.NoError(t, err)

	// Cleanup with nil tmux/lock — simulates "cleanup of already-cleaned resources".
	// Per OPS-17b and OPS-18 (best-effort), Cleanup must not panic or return errors
	// that would change the pipeline status. It suppresses all errors internally.
	assert.NotPanics(t, func() {
		orch.Cleanup(nil, nil, el, nil)
	}, "Cleanup must not panic even with nil arguments")

	// Verify shutdown event was logged.
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), string(orch.EventShutdown), "Cleanup should log a shutdown event")
}
