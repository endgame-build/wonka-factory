//go:build verify

package orch

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSanitizeBranchForLock verifies that branch names with path separators
// or parent-dir references collapse into a flat, filename-safe fragment so
// the derived lock path cannot nest under or escape RunDir.
func TestSanitizeBranchForLock(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"forward slash", "feat/x", "feat-x"},
		{"nested forward slashes", "release/v1/beta", "release-v1-beta"},
		{"backslash", `feat\x`, "feat-x"},
		{"mixed separators", `feat/x\y`, "feat-x-y"},
		{"parent dir", "..", "default"},
		{"current dir", ".", "default"},
		{"empty", "", "default"},
		{"whitespace only", "   ", "default"},
		{"dot-dot prefix kept literal", "..foo", "..foo"},
		{"traversal attempt flattened", "../evil", "..-evil"},
		{"plain name", "main", "main"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeBranchForLock(tt.branch))
		})
	}
}

// --- Internal tests for runLoop classification helpers (I3 fix) ---
//
// emitLifecycleCompleted and emitLifecycleFailed are only callable
// from within the package because runLoop consumes them directly. Testing
// them in isolation pins the audit-trail shape for each exit class, which
// is what callers reading events.jsonl actually depend on.

// TestEmitLifecycleFailed_WritesTerminalAnchor covers the I3 operational-error
// branch: a non-signal, non-abort runLoop exit must still write a
// lifecycle_completed event so the next Resume has a terminal anchor to key
// off. Without this, events.jsonl would terminate on whatever event the
// dispatcher wrote last — indistinguishable from a crash.
func TestEmitLifecycleFailed_WritesTerminalAnchor(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "events.jsonl")
	log, err := NewEventLog(logPath)
	require.NoError(t, err)
	defer log.Close()

	store, err := NewFSStore(filepath.Join(tmp, "ledger"))
	require.NoError(t, err)
	defer store.Close()

	e := &Engine{
		cfg: EngineConfig{
			RunID: "run-op-err",
			Lifecycle: &LifecycleConfig{
				Branch: "feat/x",
			},
		},
		log:   log,
		store: store,
	}

	e.emitLifecycleFailed(errors.New("store: I/O error"))
	require.NoError(t, log.Close())

	// Verify the audit-trail record.
	ev := readSingleEvent(t, logPath, EventLifecycleCompleted)
	require.NotNil(t, ev, "operational-error path must still emit lifecycle_completed")
	assert.Contains(t, ev.Detail, "outcome=failed", "Detail must distinguish failure from clean completion")
	assert.Contains(t, ev.Detail, "store: I/O error", "Detail must include the wrapped error message")
	assert.Contains(t, ev.Summary, "lifecycle failed", "Summary must carry the failure shape")
}

// TestEmitLifecycleCompleted_AbortHasOutcomeDetail pins the abort branch's
// Detail format ("outcome=aborted reason=gap_tolerance_exceeded"), which
// existing integration tests check opaquely via substring. Exercising the
// helper directly catches regressions that reshape the Detail string in
// ways breaking those substring checks.
func TestEmitLifecycleCompleted_AbortHasOutcomeDetail(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "events.jsonl")
	log, err := NewEventLog(logPath)
	require.NoError(t, err)
	defer log.Close()

	store, err := NewFSStore(filepath.Join(tmp, "ledger"))
	require.NoError(t, err)
	defer store.Close()

	e := &Engine{
		cfg: EngineConfig{
			RunID: "run-abort",
			Lifecycle: &LifecycleConfig{
				Branch: "feat/x",
			},
		},
		log:   log,
		store: store,
	}

	e.emitLifecycleCompleted(ErrLifecycleAborted)
	require.NoError(t, log.Close())

	ev := readSingleEvent(t, logPath, EventLifecycleCompleted)
	require.NotNil(t, ev)
	assert.Equal(t, "outcome=aborted reason=gap_tolerance_exceeded", ev.Detail)
}

// TestDiagnosticsDetail_DefaultLedgerKindFallback covers the C2 fix: when
// the caller leaves LedgerKind empty (the common default), a runtime Beads→FS
// fallback must still surface via lifecycle_started.Detail. Prior to C2,
// the guard `e.cfg.LedgerKind != ""` skipped emission on exactly this path.
func TestDiagnosticsDetail_DefaultLedgerKindFallback(t *testing.T) {
	// Simulate what initCommon does after an empty-default fallback.
	e := &Engine{
		cfg:               EngineConfig{LedgerKind: "" /* default */},
		storeFallbackFrom: LedgerBeads, // effective requested (post-default)
		storeFallbackTo:   LedgerFS,    // actually returned by NewStore
	}

	detail := e.diagnosticsDetail(nil)
	assert.Contains(t, detail, "store_fallback=beads->fs",
		"default LedgerKind must surface fallback in audit trail (C2)")
}

// readSingleEvent returns the first event of the given kind from the log,
// or nil if none matched. Kept local to this file to avoid pulling a big
// helper set into the internal-test namespace.
func readSingleEvent(t *testing.T, logPath string, kind EventKind) *Event {
	t.Helper()
	f, err := os.Open(logPath)
	require.NoError(t, err)
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil && ev.Kind == kind {
			return &ev
		}
	}
	require.NoError(t, scanner.Err())
	return nil
}

