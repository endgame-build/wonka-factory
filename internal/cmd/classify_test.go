package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyEngineError pins the sentinel→exit-code contract. Every case
// tested directly (no engine, no cobra) so a regression in any branch shows
// up immediately — not only along the paths exercised by higher-level
// integration tests.
//
// Spec mapping:
//   - ErrLifecycleAborted → exit 0   (BVV-ERR-04: gap tolerance is expected)
//   - context.Canceled    → exit 130 (BVV-ERR-09: signal cancellation silent)
//   - context.DeadlineExceeded → exit 130 (same path as signal)
//   - ErrResumeNoLedger   → exit 2   (BVV-ERR-07: config error, not failure)
//   - ErrLockContention   → exit 4   (BVV-ERR-06: retryable)
//   - ErrCorruptLock      → exit 3   (BVV-ERR-08: human intervention)
//   - os.ErrPermission    → exit 2   (operator fixable — chmod/chown)
//   - ErrInvalidLabelFilter/ID/EnvKey → exit 2 (bad operator input — no retry)
//   - ErrCycle            → exit 2   (ledger data defect — no retry)
//   - ErrHandoffLimitReached → exit 2 (BVV-L-04: terminal task outcome)
//   - default             → exit 1   (generic runtime failure)
//
// Also verifies the wrapped-sentinel path (errors.Is walks the chain).
func TestClassifyEngineError(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantCode     int // 0 = expect nil return (no *exitError)
		wantStderr   string
		wantNoStderr bool
	}{
		{
			name:       "gap_abort_exit_0",
			err:        orch.ErrLifecycleAborted,
			wantCode:   0,
			wantStderr: "gap tolerance",
		},
		{
			name:         "context_canceled_silent_130",
			err:          context.Canceled,
			wantCode:     exitSignalInterrupt,
			wantNoStderr: true, // BVV-ERR-09 — silent on SIGINT
		},
		{
			name:         "deadline_exceeded_silent_130",
			err:          context.DeadlineExceeded,
			wantCode:     exitSignalInterrupt,
			wantNoStderr: true,
		},
		{
			name:       "resume_no_ledger_exit_2",
			err:        orch.ErrResumeNoLedger,
			wantCode:   exitConfigError,
			wantStderr: "wonka run",
		},
		{
			name:       "lock_contention_exit_4",
			err:        orch.ErrLockContention,
			wantCode:   exitLockBusy,
			wantStderr: "already being processed",
		},
		{
			name:       "corrupt_lock_exit_3",
			err:        orch.ErrCorruptLock,
			wantCode:   exitLockCorrupt,
			wantStderr: "lock corrupt",
		},
		{
			name:       "permission_denied_exit_2_with_hint",
			err:        fmt.Errorf("engine: stat ledger dir: %w", os.ErrPermission),
			wantCode:   exitConfigError,
			wantStderr: "ownership/mode",
		},
		{
			name:       "wrapped_sentinel_still_classified",
			err:        fmt.Errorf("orch: resume: %w", orch.ErrResumeNoLedger),
			wantCode:   exitConfigError,
			wantStderr: "wonka run",
		},
		{
			name:       "invalid_label_filter_exit_2",
			err:        orch.ErrInvalidLabelFilter,
			wantCode:   exitConfigError,
			wantStderr: "invalid input",
		},
		{
			name:       "invalid_id_exit_2",
			err:        fmt.Errorf("store: %w", orch.ErrInvalidID),
			wantCode:   exitConfigError,
			wantStderr: "invalid input",
		},
		{
			name:       "invalid_env_key_exit_2",
			err:        orch.ErrInvalidEnvKey,
			wantCode:   exitConfigError,
			wantStderr: "invalid input",
		},
		{
			name:       "cycle_exit_2_with_hint",
			err:        orch.ErrCycle,
			wantCode:   exitConfigError,
			wantStderr: "dependency cycle",
		},
		{
			name:       "handoff_limit_exit_2_with_hint",
			err:        orch.ErrHandoffLimitReached,
			wantCode:   exitConfigError,
			wantStderr: "handoff limit",
		},
		{
			name:       "unknown_error_default_exit_1",
			err:        errors.New("tmux exploded"),
			wantCode:   exitRuntimeError,
			wantStderr: "lifecycle failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := classifyEngineError(tc.err, "test-branch", &stderr)

			if tc.wantCode == 0 {
				require.NoError(t, got, "gap-abort must collapse to nil")
			} else {
				require.Error(t, got)
				requireExitCode(t, got, tc.wantCode)
			}

			if tc.wantNoStderr {
				assert.Empty(t, stderr.String(), "signal cancellation must stay silent")
			} else {
				assert.Contains(t, stderr.String(), tc.wantStderr)
			}
		})
	}
}
