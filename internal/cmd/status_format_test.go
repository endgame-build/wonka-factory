package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	renderTable(&buf, tasks)

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
	renderTable(&buf, nil)
	assert.Contains(t, buf.String(), "STATUS")
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	assert.Len(t, lines, 1, "header only when no tasks")
}

// fields splits on whitespace; tabwriter expands \t into padding spaces so
// strings.Fields is the right tool here.
func fields(line string) []string { return strings.Fields(line) }
