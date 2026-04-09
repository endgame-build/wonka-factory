//go:build verify

package orch

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestErrorSentinels_DistinctAndWrappable verifies that all sentinel errors
// are distinct values and survive wrapping via errors.Is.
//
// Covers: LDG-02 (ledger as single source of truth — errors must be matchable).
func TestErrorSentinels_DistinctAndWrappable(t *testing.T) {
	sentinels := []error{
		ErrNotFound,
		ErrTaskExists,
		ErrWorkerExists,
		ErrCycle,
		ErrAlreadyAssigned,
		ErrTaskNotReady,
		ErrWorkerBusy,
		ErrPoolExhausted,
		ErrLifecycleAborted,
		ErrLockContention,
		ErrResumeNoLedger,
		ErrHandoffLimitReached,
		ErrInvalidLabelFilter,
		ErrInvalidID,
	}

	// Verify distinctness: no two sentinels should match via errors.Is.
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinels[%d] (%v) matches sentinels[%d] (%v) — must be distinct", i, a, j, b)
			}
		}
	}

	// Verify wrapping survives errors.Is.
	for i, sentinel := range sentinels {
		wrapped := fmt.Errorf("operation failed: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("sentinels[%d] (%v) not recoverable after wrapping", i, sentinel)
		}
	}
}

// TestErrInvalidLabelFilter_Format verifies the error message is descriptive.
//
// Covers: malformed label filter returns error, not silent match.
func TestErrInvalidLabelFilter_Format(t *testing.T) {
	msg := ErrInvalidLabelFilter.Error()
	if msg == "" {
		t.Error("ErrInvalidLabelFilter.Error() is empty")
	}
	// The message should hint at the expected format.
	if !strings.Contains(msg, "key:value") {
		t.Errorf("ErrInvalidLabelFilter message %q does not mention expected format", msg)
	}
}
