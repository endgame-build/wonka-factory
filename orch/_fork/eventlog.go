package orch

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// EventKind enumerates mandatory emission points (OPS-20).
type EventKind string

const (
	EventPhaseStart       EventKind = "phase_start"
	EventPhaseComplete    EventKind = "phase_complete"
	EventAgentStart       EventKind = "agent_start"
	EventAgentComplete    EventKind = "agent_complete"
	EventConsensusMerge   EventKind = "consensus_merge"
	EventConsensusVerify  EventKind = "consensus_verify"
	EventGateResult       EventKind = "gate_result"
	EventGapRecorded      EventKind = "gap_recorded"
	EventPipelineComplete EventKind = "pipeline_complete"
	EventCircuitBreaker   EventKind = "circuit_breaker"
	EventCrashDetected    EventKind = "crash_detected"
	EventSessionRestart   EventKind = "session_restart"
	EventRetryScheduled   EventKind = "retry_scheduled"
	EventShutdown         EventKind = "shutdown"
)

// Event is a single JSONL audit record (OPS-19).
type Event struct {
	Timestamp time.Time    `json:"timestamp"`
	Kind      EventKind    `json:"kind"`
	Phase     string       `json:"phase,omitempty"`
	Agent     string       `json:"agent,omitempty"`
	TaskID    string       `json:"task_id,omitempty"`
	Worker    string       `json:"worker,omitempty"`
	Summary   string       `json:"summary"`
	Detail    string       `json:"detail,omitempty"`
	Outcome   AgentOutcome `json:"outcome"`
}

// ProgressReporter receives pipeline events for real-time display.
// Implementations must be safe for concurrent calls from multiple goroutines.
// A nil ProgressReporter is valid and treated as a no-op.
type ProgressReporter interface {
	OnEvent(Event)
}

// emitAndNotify writes an event to the log and forwards it to the progress reporter.
// Either log or progress may be nil.
func emitAndNotify(log *EventLog, progress ProgressReporter, ev Event) {
	if log != nil {
		_ = log.Emit(ev)
	}
	if progress != nil {
		progress.OnEvent(ev)
	}
}

// EventLog provides append-only JSONL event logging.
// Thread-safe via mutex. Writes are append-only (O_APPEND).
type EventLog struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewEventLog creates or opens an event log at the given path.
// The file is opened with O_APPEND|O_CREATE|O_WRONLY for atomic appends.
func NewEventLog(path string) (*EventLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eventlog: open %s: %w", path, err)
	}
	return &EventLog{file: f, path: path}, nil
}

// Emit appends a single event to the log. Thread-safe.
// Sets Timestamp to now if zero.
func (el *EventLog) Emit(e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("eventlog: marshal: %w", err)
	}
	data = append(data, '\n')

	el.mu.Lock()
	defer el.mu.Unlock()

	if _, err := el.file.Write(data); err != nil {
		return fmt.Errorf("eventlog: write: %w", err)
	}
	return nil
}

// Close flushes and closes the underlying file.
func (el *EventLog) Close() error {
	el.mu.Lock()
	defer el.mu.Unlock()
	return el.file.Close()
}

// Path returns the log file path.
func (el *EventLog) Path() string {
	return el.path
}
