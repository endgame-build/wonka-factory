//go:build verify

package orch

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
