package orch

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// EventKind enumerates the 17 mandatory emission points (BVV spec §10.3).
type EventKind string

const (
	EventTaskDispatched     EventKind = "task_dispatched"
	EventTaskCompleted      EventKind = "task_completed"
	EventTaskFailed         EventKind = "task_failed"
	EventTaskRetried        EventKind = "task_retried"
	EventTaskBlocked        EventKind = "task_blocked"
	EventTaskHandoff        EventKind = "task_handoff"
	EventWorkerSpawned      EventKind = "worker_spawned"
	EventWorkerReleased     EventKind = "worker_released"
	EventGapRecorded        EventKind = "gap_recorded"
	EventEscalationCreated  EventKind = "escalation_created"
	EventLifecycleStarted   EventKind = "lifecycle_started"
	EventLifecycleCompleted EventKind = "lifecycle_completed"
	EventGateCreated        EventKind = "gate_created"
	EventGatePassed         EventKind = "gate_passed"
	EventGateFailed         EventKind = "gate_failed"
	EventEscalationResolved EventKind = "escalation_resolved"
	EventHandoffLimitReached EventKind = "handoff_limit_reached"
)

// AllEventKinds is the canonical set of BVV event kinds, used by tests
// to verify completeness against spec §10.3.
var AllEventKinds = []EventKind{
	EventTaskDispatched, EventTaskCompleted, EventTaskFailed, EventTaskRetried,
	EventTaskBlocked, EventTaskHandoff, EventWorkerSpawned, EventWorkerReleased,
	EventGapRecorded, EventEscalationCreated, EventLifecycleStarted,
	EventLifecycleCompleted, EventGateCreated, EventGatePassed, EventGateFailed,
	EventEscalationResolved, EventHandoffLimitReached,
}

// Event is a single JSONL audit record (BVV spec §10.3).
// Phase and Agent fields from the fork are removed — BVV is phase-agnostic
// (BVV-DSN-04) and agents are identified by role labels, not event fields.
type Event struct {
	Timestamp time.Time    `json:"timestamp"`
	Kind      EventKind    `json:"kind"`
	TaskID    string       `json:"task_id,omitempty"`
	Worker    string       `json:"worker,omitempty"`
	Summary   string       `json:"summary"`
	Detail    string       `json:"detail,omitempty"`
	Outcome   AgentOutcome `json:"outcome,omitempty"`
}

// ProgressReporter receives lifecycle events for real-time display.
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
