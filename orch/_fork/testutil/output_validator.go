package testutil

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ValidateRunOutput checks the operational state of a completed pipeline run.
// Validates: agent outputs per OPS-04, ledger files exist, event log is valid JSONL,
// and lock file is released.
func ValidateRunOutput(t *testing.T, runDir string, pipeline *orch.Pipeline, store orch.Store) {
	t.Helper()

	// 1. Validate agent outputs (OPS-04).
	agentIndex := orch.BuildAgentIndex(pipeline)
	for _, phase := range pipeline.Phases {
		phaseID := pipeline.ID + ":" + phase.ID
		children, err := store.GetChildren(phaseID)
		require.NoError(t, err, "get children of %s", phaseID)

		for _, task := range children {
			if task.Output == "" || task.Status != orch.StatusCompleted {
				continue
			}
			agentDef, ok := agentIndex[task.AgentID]
			if !ok {
				continue
			}
			outputPath := filepath.Join(runDir, task.Output)
			err := orch.ValidateOutput(outputPath, agentDef.Format)
			require.NoError(t, err, "OPS-04: agent %s output %s should be valid", task.AgentID, task.Output)
		}
	}

	// 2. Validate ledger directory exists.
	ledgerDir := filepath.Join(runDir, "ledger")
	assert.DirExists(t, ledgerDir, "ledger directory should exist")
	assert.DirExists(t, filepath.Join(ledgerDir, "tasks"), "tasks directory should exist")
	assert.DirExists(t, filepath.Join(ledgerDir, "workers"), "workers directory should exist")

	// 3. Validate event log is valid JSONL (OPS-19).
	eventLogPath := filepath.Join(runDir, "events.jsonl")
	if assert.FileExists(t, eventLogPath, "event log should exist") {
		f, err := os.Open(eventLogPath)
		require.NoError(t, err)
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			var e orch.Event
			err := json.Unmarshal(scanner.Bytes(), &e)
			assert.NoError(t, err, "event log line %d should be valid JSON", lineNo)
		}
		require.NoError(t, scanner.Err())
		assert.Positive(t, lineNo, "event log should have at least one event")
	}

	// 4. Validate lock file released (OPS-12).
	// In tests, runDir doubles as both the repo root and the output directory,
	// so joining with runDir is correct here. Production lock path resolution
	// is in engine.go:initCommon (resolves against RepoPath).
	lockPath := filepath.Join(runDir, pipeline.Lock.Path)
	if filepath.IsAbs(pipeline.Lock.Path) {
		lockPath = pipeline.Lock.Path
	}
	_, err := os.Stat(lockPath)
	assert.True(t, os.IsNotExist(err), "lock file should be released after completion")
}

// ValidateEventSequence checks that the event log contains the expected event kinds
// in the correct order (OPS-20 mandatory emission points).
func ValidateEventSequence(t *testing.T, eventLogPath string, expectedKinds []orch.EventKind) {
	t.Helper()

	f, err := os.Open(eventLogPath)
	require.NoError(t, err)
	defer f.Close()

	var kinds []orch.EventKind
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e orch.Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		kinds = append(kinds, e.Kind)
	}
	require.NoError(t, scanner.Err())

	// Check that each expected kind appears in order (not necessarily contiguous).
	idx := 0
	for _, expected := range expectedKinds {
		found := false
		for idx < len(kinds) {
			if kinds[idx] == expected {
				found = true
				idx++
				break
			}
			idx++
		}
		if !found {
			t.Errorf("expected event kind %q not found after position %d in sequence %v",
				expected, idx, formatKinds(kinds))
		}
	}
}

func formatKinds(kinds []orch.EventKind) string {
	s := make([]string, len(kinds))
	for i, k := range kinds {
		s[i] = string(k)
	}
	return fmt.Sprintf("[%s]", filepath.Join(s...))
}
