package orch

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// EventKind enumerates the 19 mandatory emission points: 17 from BVV spec
// §10.3 plus graph_validated and graph_invalid for BVV-TG-07..10.
type EventKind string

const (
	EventTaskDispatched      EventKind = "task_dispatched"
	EventTaskCompleted       EventKind = "task_completed"
	EventTaskFailed          EventKind = "task_failed"
	EventTaskRetried         EventKind = "task_retried"
	EventTaskBlocked         EventKind = "task_blocked"
	EventTaskHandoff         EventKind = "task_handoff"
	EventWorkerSpawned       EventKind = "worker_spawned"
	EventWorkerReleased      EventKind = "worker_released"
	EventGapRecorded         EventKind = "gap_recorded"
	EventEscalationCreated   EventKind = "escalation_created"
	EventLifecycleStarted    EventKind = "lifecycle_started"
	EventLifecycleCompleted  EventKind = "lifecycle_completed"
	EventGateCreated         EventKind = "gate_created"
	EventGatePassed          EventKind = "gate_passed"
	EventGateFailed          EventKind = "gate_failed"
	EventEscalationResolved  EventKind = "escalation_resolved"
	EventHandoffLimitReached EventKind = "handoff_limit_reached"
	// EventGraphValidated is emitted when ValidateLifecycleGraph succeeds
	// after a role:planner task completes (BVV-TG-07..10). Also serves as
	// a replay-detection anchor: Engine.Resume scans the event log for
	// this kind (see resume.go:recoverFromEventLog) to detect completed
	// planner tasks whose validation hook never fired, then re-fires the
	// hook on the next Resume. Absence of this anchor for a completed
	// planner task is the signal, not a bug.
	EventGraphValidated EventKind = "graph_validated"
	// EventGraphInvalid is emitted when ValidateLifecycleGraph rejects the
	// task graph after a role:planner task completes. Accompanied by an
	// escalation task (escalation-graph-<plan-id>) and lifecycle abort.
	// Like EventGraphValidated, also functions as a replay anchor so a
	// crash-after-rejection doesn't silently re-validate on next Resume.
	// BVV-TG-07..10.
	EventGraphInvalid EventKind = "graph_invalid"
)

// AllEventKinds is the canonical set of BVV event kinds — spec §10.3's
// 17 kinds plus the two BVV-TG-07..10 replay anchors (graph_validated,
// graph_invalid). Tests iterate this slice to verify completeness and
// distinctness; any new EventKind added to the const block above MUST
// be appended here or TestEventKinds_Count catches the drift.
var AllEventKinds = []EventKind{
	EventTaskDispatched, EventTaskCompleted, EventTaskFailed, EventTaskRetried,
	EventTaskBlocked, EventTaskHandoff, EventWorkerSpawned, EventWorkerReleased,
	EventGapRecorded, EventEscalationCreated, EventLifecycleStarted,
	EventLifecycleCompleted, EventGateCreated, EventGatePassed, EventGateFailed,
	EventEscalationResolved, EventHandoffLimitReached,
	EventGraphValidated, EventGraphInvalid,
}

// Event is a single JSONL audit record (BVV spec §10.3).
// BVV-DSN-04: phase-agnostic — no Phase field; agents are identified by
// role labels carried on the referenced Task, not by an Agent field.
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
// Either log or progress may be nil. Returns the Emit error (if any) so callers
// can decide whether to propagate or log — audit trail gaps corrupt GapTracker
// recovery (BVV-ERR-03..05).
func emitAndNotify(log *EventLog, progress ProgressReporter, ev Event) error {
	if progress != nil {
		progress.OnEvent(ev)
	}
	if log != nil {
		return log.Emit(ev)
	}
	return nil
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

// Close closes the underlying file.
func (el *EventLog) Close() error {
	el.mu.Lock()
	defer el.mu.Unlock()
	if err := el.file.Close(); err != nil {
		return fmt.Errorf("eventlog: close %s: %w", el.path, err)
	}
	return nil
}

// Path returns the log file path.
func (el *EventLog) Path() string {
	return el.path
}
