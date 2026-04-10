//go:build verify

package orch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestEventKinds_Count verifies exactly 17 event kinds are defined,
// matching BVV spec §10.3.
func TestEventKinds_Count(t *testing.T) {
	if got := len(AllEventKinds); got != 17 {
		t.Errorf("len(AllEventKinds) = %d, want 17", got)
	}

	// Verify all kinds are distinct.
	seen := make(map[EventKind]bool)
	for _, k := range AllEventKinds {
		if seen[k] {
			t.Errorf("duplicate event kind: %q", k)
		}
		seen[k] = true
	}
}

// TestEventLog_EmitRoundtrip creates an event log, emits 3 events with
// different outcomes, closes, reads back, and verifies JSONL content.
//
// Covers: BVV-SS (audit trail), LDG-02 (durability).
func TestEventLog_EmitRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	el, err := NewEventLog(path)
	if err != nil {
		t.Fatalf("NewEventLog: %v", err)
	}

	events := []Event{
		{Kind: EventTaskDispatched, TaskID: "t1", Worker: "w1", Summary: "dispatched", Outcome: OutcomeSuccess},
		{Kind: EventTaskFailed, TaskID: "t2", Summary: "retries exhausted", Outcome: OutcomeFailure},
		{Kind: EventTaskHandoff, TaskID: "t3", Worker: "w2", Summary: "new session", Outcome: OutcomeHandoff},
	}
	for _, e := range events {
		if err := el.Emit(e); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if err := el.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read back and verify.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var decoded []Event
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		decoded = append(decoded, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(decoded) != len(events) {
		t.Fatalf("got %d events, want %d", len(decoded), len(events))
	}
	for i, d := range decoded {
		if d.Kind != events[i].Kind {
			t.Errorf("event[%d].Kind = %q, want %q", i, d.Kind, events[i].Kind)
		}
		if d.TaskID != events[i].TaskID {
			t.Errorf("event[%d].TaskID = %q, want %q", i, d.TaskID, events[i].TaskID)
		}
		if d.Outcome != events[i].Outcome {
			t.Errorf("event[%d].Outcome = %q, want %q", i, d.Outcome, events[i].Outcome)
		}
	}
}

// TestEventLog_EmitZeroTimestamp verifies that Emit backfills Timestamp when zero.
func TestEventLog_EmitZeroTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	el, err := NewEventLog(path)
	if err != nil {
		t.Fatalf("NewEventLog: %v", err)
	}

	before := time.Now()
	if err := el.Emit(Event{Kind: EventLifecycleStarted, Summary: "start"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	after := time.Now()
	el.Close()

	data, _ := os.ReadFile(path)
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if e.Timestamp.Before(before) || e.Timestamp.After(after) {
		t.Errorf("Timestamp %v not in [%v, %v]", e.Timestamp, before, after)
	}
}

// TestProgressReporter_NilSafe verifies emitAndNotify handles nil log, nil
// progress, both nil, and both non-nil without panicking.
func TestProgressReporter_NilSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	el, err := NewEventLog(path)
	if err != nil {
		t.Fatalf("NewEventLog: %v", err)
	}
	defer el.Close()

	ev := Event{Kind: EventTaskCompleted, Summary: "test"}

	// nil log, nil progress — no error
	err = emitAndNotify(nil, nil, ev)
	if err != nil {
		t.Errorf("nil/nil: unexpected error: %v", err)
	}

	// non-nil log, nil progress — no error
	err = emitAndNotify(el, nil, ev)
	if err != nil {
		t.Errorf("log/nil: unexpected error: %v", err)
	}

	// nil log, non-nil progress — no error, progress called
	pr := &countReporter{}
	err = emitAndNotify(nil, pr, ev)
	if err != nil {
		t.Errorf("nil/progress: unexpected error: %v", err)
	}
	if pr.count != 1 {
		t.Errorf("progress count = %d, want 1", pr.count)
	}

	// both non-nil — no error, progress called
	err = emitAndNotify(el, pr, ev)
	if err != nil {
		t.Errorf("both: unexpected error: %v", err)
	}
	if pr.count != 2 {
		t.Errorf("progress count = %d, want 2", pr.count)
	}
}

type countReporter struct {
	mu    sync.Mutex
	count int
}

func (r *countReporter) OnEvent(Event) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
}

// TestEventLog_ConcurrentEmit spawns 10 goroutines each emitting 100 events,
// then verifies exactly 1000 lines in the file. Catches lock regressions.
//
// Covers: BVV-SS (concurrent event emission must not corrupt the audit trail).
func TestEventLog_ConcurrentEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	el, err := NewEventLog(path)
	if err != nil {
		t.Fatalf("NewEventLog: %v", err)
	}

	const goroutines = 10
	const eventsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerGoroutine; i++ {
				_ = el.Emit(Event{Kind: EventTaskDispatched, Summary: "concurrent"})
			}
		}()
	}
	wg.Wait()

	if err := el.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Count lines.
	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	want := goroutines * eventsPerGoroutine
	if lines != want {
		t.Errorf("got %d lines, want %d", lines, want)
	}
}
