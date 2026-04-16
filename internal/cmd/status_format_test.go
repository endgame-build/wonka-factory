package cmd

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errWriter returns err on every Write. Used to exercise renderTable's
// error-propagation paths without needing a real OS pipe.
type errWriter struct{ err error }

func (w errWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestRenderTable_HeaderAndRows pins the column order and the placeholder
// ("-") used for empty Assignee / Role fields. Operators grep these tables;
// changing column order in a later refactor would break their scripts
// silently unless caught here.
func TestRenderTable_HeaderAndRows(t *testing.T) {
	var buf bytes.Buffer
	tasks := []*orch.Task{
		{
			ID:     "issue-1",
			Title:  "build user model",
			Status: orch.StatusOpen,
			Labels: map[string]string{orch.LabelRole: "builder"},
			// No assignee — should render as "-".
		},
		{
			ID:       "issue-2",
			Title:    "verify auth",
			Status:   orch.StatusInProgress,
			Assignee: "worker-1",
			Labels:   map[string]string{orch.LabelRole: "verifier"},
		},
	}
	require.NoError(t, renderTable(&buf, tasks))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3, "one header + two rows")

	// Header order is load-bearing — downstream scripts may splitStrip
	// columns by position.
	assert.Equal(t, []string{"ID", "STATUS", "ROLE", "ASSIGNEE", "TITLE"}, fields(lines[0]))

	row1 := fields(lines[1])
	assert.Equal(t, "issue-1", row1[0])
	assert.Equal(t, "open", row1[1])
	assert.Equal(t, "builder", row1[2])
	assert.Equal(t, "-", row1[3], "empty assignee must render as '-'")
	assert.Equal(t, "build", row1[4], "title starts with 'build'")

	row2 := fields(lines[2])
	assert.Equal(t, "worker-1", row2[3])
}

// TestRenderTable_Empty verifies the zero-task path still produces a header
// so downstream parsers don't choke on an empty stream.
func TestRenderTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderTable(&buf, nil))
	assert.Contains(t, buf.String(), "STATUS")
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	assert.Len(t, lines, 1, "header only when no tasks")
}

// TestRenderTable_EPIPESwallowed verifies that a reader closing the pipe
// early (SIGPIPE / EPIPE) is treated as clean exit, not a failure —
// otherwise `wonka status | head` would spuriously return exit 1.
func TestRenderTable_EPIPESwallowed(t *testing.T) {
	w := errWriter{err: syscall.EPIPE}
	err := renderTable(w, []*orch.Task{{ID: "issue-1", Title: "x", Status: orch.StatusOpen}})
	assert.NoError(t, err, "EPIPE must collapse to nil — reader closed the pipe, not our failure")
}

// TestRenderTable_PropagatesOtherErrors verifies that non-EPIPE write
// failures (disk full, EIO, closed file) do surface — swallowing these
// would mask real problems behind a truncated table and exit 0.
func TestRenderTable_PropagatesOtherErrors(t *testing.T) {
	sentinel := errors.New("disk full")
	w := errWriter{err: sentinel}
	err := renderTable(w, []*orch.Task{{ID: "issue-1", Title: "x", Status: orch.StatusOpen}})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "non-EPIPE errors must propagate for the caller to map to exit 1")
}

// fields splits on whitespace; tabwriter expands \t into padding spaces so
// strings.Fields is the right tool here.
func fields(line string) []string { return strings.Fields(line) }
