package orch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLockContention is returned when the pipeline lock is held by another process.
var ErrLockContention = errors.New("pipeline lock held by another process")

// LockContent is the JSON structure written to the lock file.
type LockContent struct {
	Holder    string    `json:"holder"`
	Phase     string    `json:"phase"`
	Timestamp time.Time `json:"timestamp"`
}

// PipelineLock provides exclusive pipeline access using filesystem lock files.
//
// The lock protocol implements OPS-10..12 with the TLA+ findings:
//   - All acquisition paths MUST reset the timestamp (prevents immediate re-stealing)
//   - The dispatch loop MUST call Refresh on every tick (prevents false staleness)
type PipelineLock struct {
	path               string
	stalenessThreshold time.Duration
	retryCount         int
	retryDelay         time.Duration
	holderID           string // set on successful Acquire
}

// Path returns the lock file path.
func (l *PipelineLock) Path() string { return l.path }

// NewPipelineLock creates a lock from a LockConfig.
func NewPipelineLock(cfg LockConfig) *PipelineLock {
	return &PipelineLock{
		path:               cfg.Path,
		stalenessThreshold: cfg.StalenessThreshold,
		retryCount:         cfg.RetryCount,
		retryDelay:         cfg.RetryDelay,
	}
}

// Acquire attempts to acquire the exclusive pipeline lock (OPS-10, OPS-11 STRENGTHENED).
//
// Algorithm:
//  1. Exclusive-create (O_CREATE|O_EXCL). Write fresh LockContent. Done.
//  2. If file exists: read, check staleness. If stale → remove + retry create.
//     MUST write fresh timestamp on re-creation (TLA+ finding).
//  3. If not stale → return ErrLockContention.
//  4. On create race: retry up to retryCount times.
func (l *PipelineLock) Acquire(holderID, phase string) error {
	content := LockContent{
		Holder:    holderID,
		Phase:     phase,
		Timestamp: time.Now(),
	}

	for attempt := 0; attempt <= l.retryCount; attempt++ {
		if attempt > 0 {
			time.Sleep(l.retryDelay)
		}

		err := l.tryCreate(content)
		if err == nil {
			l.holderID = holderID
			return nil // acquired
		}

		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create lock: %w", err)
		}

		// Lock file exists — check staleness.
		existing, readErr := l.read()
		if readErr != nil {
			// Can't read — maybe it was deleted between stat and read. Retry.
			continue
		}

		if time.Since(existing.Timestamp) >= l.stalenessThreshold {
			// Stale lock — remove and retry create.
			// The next iteration's tryCreate will write a fresh timestamp.
			os.Remove(l.path)
			continue
		}

		// Not stale — genuine contention.
		return fmt.Errorf("%w: held by %s since %s (phase %s)",
			ErrLockContention, existing.Holder, existing.Timestamp.Format(time.RFC3339), existing.Phase)
	}

	return fmt.Errorf("failed to acquire lock after %d retries", l.retryCount+1)
}

// Refresh updates the lock timestamp to prevent false staleness detection.
// Must be called on every dispatch tick (TLA+ finding: without refresh,
// the watchdog ages the lock past staleness while the orchestrator is alive).
func (l *PipelineLock) Refresh(phase string) error {
	existing, err := l.read()
	if err != nil {
		return fmt.Errorf("read lock for refresh: %w", err)
	}
	if existing.Holder != l.holderID {
		return fmt.Errorf("lock holder mismatch: expected %q, got %q", l.holderID, existing.Holder)
	}
	existing.Phase = phase
	existing.Timestamp = time.Now()
	return l.write(*existing)
}

// Release removes the lock file (OPS-12).
func (l *PipelineLock) Release() error {
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // already released
	}
	return err
}

// tryCreate attempts to create the lock file with exclusive-create semantics.
func (l *PipelineLock) tryCreate(content LockContent) error {
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		os.Remove(l.path)
		return writeErr
	}
	return closeErr
}

func (l *PipelineLock) read() (*LockContent, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil, err
	}
	var c LockContent
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (l *PipelineLock) write(content LockContent) error {
	return atomicWriteJSON(l.path, content)
}
