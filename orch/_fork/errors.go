package orch

import "fmt"

// SubprocessError captures an agent subprocess failure with diagnostic context.
// The orchestrator wraps agent failures in this type so callers can inspect
// exit codes and raw output for debugging without violating ZFC (DSN-03) —
// the orchestrator never interprets this content, only passes it through.
type SubprocessError struct {
	TaskID   string
	AgentID  string
	ExitCode int
	Output   string // raw stdout/stderr (tail-truncated to ~4KB)
	Cause    error  // underlying error (e.g., ErrOutputInvalid)
}

func (e *SubprocessError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("agent %s (task %s) exit %d: %v", e.AgentID, e.TaskID, e.ExitCode, e.Cause)
	}
	return fmt.Sprintf("agent %s (task %s) exit %d", e.AgentID, e.TaskID, e.ExitCode)
}

func (e *SubprocessError) Unwrap() error {
	return e.Cause
}
