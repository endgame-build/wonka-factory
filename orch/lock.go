package orch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// LockContent is the JSON structure written to the lock file.
// Branch replaces the fork's Phase field — BVV scopes exclusion per git
// branch, not per phase (BVV-S-01).
type LockContent struct {
	Holder    string    `json:"holder"`
	Branch    string    `json:"branch"`
	Timestamp time.Time `json:"timestamp"`
}

// LifecycleLock provides exclusive per-branch lifecycle access using
// filesystem lock files (BVV-S-01 lifecycle exclusion).
//
// The lock protocol implements BVV-ERR-06 (acquire with staleness recovery)
// and BVV-L-02 (refresh liveness) with the TLA+ findings:
//   - All acquisition paths MUST reset the timestamp (prevents immediate re-stealing)
//   - The dispatch loop MUST call Refresh on every tick (prevents false staleness)
//
// The BVV-ERR-10a precondition (all sessions drained before voluntary release)
// is enforced at the dispatcher call site via a Phase 5 runtime invariant
// (AssertLifecycleReleaseDrained), not inside Release(). Release() stays dumb
// so the signal-handler (involuntary) path can call it without triggering
// assertions under build tag verify.
type LifecycleLock struct {
	path               string
	stalenessThreshold time.Duration
	retryCount         int
	retryDelay         time.Duration
	holderID           string // set on successful Acquire
}

// Path returns the lock file path.
func (l *LifecycleLock) Path() string { return l.path }

// NewLifecycleLock creates a lock from a LockConfig.
func NewLifecycleLock(cfg LockConfig) *LifecycleLock {
	return &LifecycleLock{
		path:               cfg.Path,
		stalenessThreshold: cfg.StalenessThreshold,
		retryCount:         cfg.RetryCount,
		retryDelay:         cfg.RetryDelay,
	}
}

// Acquire attempts to acquire the exclusive lifecycle lock (BVV-ERR-06,
// BVV-S-01 STRENGTHENED).
//
// Algorithm:
//  1. Exclusive-create (O_CREATE|O_EXCL). Write fresh LockContent. Done.
//  2. If file exists: read, check staleness. If stale → remove + retry create.
//     MUST write fresh timestamp on re-creation (TLA+ finding).
//  3. If not stale → return ErrLockContention.
//  4. On create race: retry up to retryCount times.
func (l *LifecycleLock) Acquire(holderID, branch string) error {
	content := LockContent{
		Holder:    holderID,
		Branch:    branch,
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
			_ = os.Remove(l.path)
			continue
		}

		// Not stale — genuine contention.
		return fmt.Errorf("%w: held by %s since %s (branch %s)",
			ErrLockContention, existing.Holder, existing.Timestamp.Format(time.RFC3339), existing.Branch)
	}

	return fmt.Errorf("failed to acquire lock after %d retries", l.retryCount+1)
}

// Refresh updates the lock timestamp to prevent false staleness detection
// (BVV-L-02). Must be called on every dispatch tick — without refresh, the
// watchdog ages the lock past staleness while the orchestrator is alive.
func (l *LifecycleLock) Refresh(branch string) error {
	existing, err := l.read()
	if err != nil {
		return fmt.Errorf("read lock for refresh: %w", err)
	}
	if existing.Holder != l.holderID {
		return fmt.Errorf("lock holder mismatch: expected %q, got %q", l.holderID, existing.Holder)
	}
	existing.Branch = branch
	existing.Timestamp = time.Now()
	return l.write(*existing)
}

// Release removes the lock file. Idempotent: returns nil if the lock is
// already gone. BVV-ERR-10a (drain precondition) is enforced at the caller
// site, not here.
func (l *LifecycleLock) Release() error {
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // already released
	}
	return err
}

// tryCreate attempts to create the lock file with exclusive-create semantics.
func (l *LifecycleLock) tryCreate(content LockContent) error {
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
		_ = os.Remove(l.path)
		return writeErr
	}
	return closeErr
}

func (l *LifecycleLock) read() (*LockContent, error) {
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

func (l *LifecycleLock) write(content LockContent) error {
	return atomicWriteJSON(l.path, content)
}
