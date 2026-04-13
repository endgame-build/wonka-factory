//go:build verify

package orch

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsGHNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"no pull requests found", fmt.Errorf("pr view: exit status 1 (stderr: no pull requests found for branch feat/x)"), true},
		{"could not find pull request", fmt.Errorf("pr view: exit status 1 (stderr: could not find pull request)"), true},
		{"auth failure", fmt.Errorf("pr view: exit status 1 (stderr: HTTP 401: authentication required)"), false},
		{"network error", fmt.Errorf("pr view: dial tcp: lookup github.com: no such host"), false},
		{"permission denied", fmt.Errorf("pr view: exit status 1 (stderr: HTTP 403: Resource not accessible by integration)"), false},
		{"repo not found is not PR not found", fmt.Errorf("pr view: exit status 1 (stderr: repository not found)"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isGHNotFound(tt.err))
		})
	}
}
