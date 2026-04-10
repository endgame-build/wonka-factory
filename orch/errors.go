package orch

import "errors"

// Sentinel errors for Store operations.
// Match with errors.Is(); wrap with %w for context.
var (
	ErrNotFound        = errors.New("not found")
	ErrTaskExists      = errors.New("task already exists")
	ErrWorkerExists    = errors.New("worker already exists")
	ErrCycle           = errors.New("dependency cycle detected")
	ErrAlreadyAssigned = errors.New("task already assigned")
	ErrTaskNotReady    = errors.New("task not ready for assignment")
	ErrWorkerBusy      = errors.New("worker is not idle")
	ErrPoolExhausted   = errors.New("worker pool exhausted") // returned by the dispatcher when every worker slot is busy
)

// Sentinel errors for lifecycle control flow.
var (
	ErrLifecycleAborted    = errors.New("lifecycle aborted: gap tolerance reached") // BVV-ERR-04
	ErrLockContention      = errors.New("lifecycle lock held by another process")   // BVV-S-01, BVV-ERR-06
	ErrResumeNoLedger      = errors.New("no ledger found for resume")               // BVV-ERR-07
	ErrHandoffLimitReached = errors.New("handoff limit reached for task")           // BVV-L-04
)

// Sentinel errors for input validation.
var (
	ErrInvalidLabelFilter = errors.New("invalid label filter: expected key:value format")
	ErrInvalidID          = errors.New("invalid identifier: must not contain path separators or '..'")
	ErrInvalidEnvKey      = errors.New("invalid environment variable key: must match [A-Za-z_][A-Za-z0-9_]*")
)
