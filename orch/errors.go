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

	// ErrStoreUnavailable signals that the ledger backend cannot be reached.
	// Returned by BDCLIStore when a `bd` invocation times out, is killed by the
	// orchestrator's context, or exits with a transport-class signal (exec
	// failure, no such command). Distinct from ErrNotFound or domain errors —
	// this is "the store is gone, retry later" rather than "your write was
	// rejected on its merits". The CLI maps it to exitRuntimeError so wrapper
	// scripts treat it as a transient infra failure, not a config issue.
	ErrStoreUnavailable = errors.New("ledger backend unavailable")
)

// Sentinel errors for lifecycle control flow.
var (
	ErrLifecycleAborted    = errors.New("lifecycle aborted: gap tolerance reached") // BVV-ERR-04
	ErrLockContention      = errors.New("lifecycle lock held by another process")   // BVV-S-01, BVV-ERR-06
	ErrResumeNoEventLog    = errors.New("no event log found for resume")            // BVV-ERR-07
	ErrHandoffLimitReached = errors.New("handoff limit reached for task")           // BVV-L-04

	// ErrCorruptLock signals a lock file that parses as invalid JSON or
	// otherwise cannot be read as a LockContent. BVV-ERR-08 requires Resume
	// to reconnect to the surviving tmux socket via the recovered RunID,
	// so a corrupt lock is operator-intervention territory — silently
	// fabricating a fresh RunID would orphan any live sessions.
	ErrCorruptLock = errors.New("lifecycle lock file corrupt — operator intervention required")

	// ErrCorruptEventLog signals an event log whose first record fails to
	// parse as a JSON Event. Distinct from ErrResumeNoEventLog: a missing
	// log means "no prior wonka run on this branch — use `wonka run`",
	// while a corrupt first record means a prior run started and crashed
	// mid-write, which is operator-intervention territory (recovery would
	// otherwise replay an undefined event stream).
	ErrCorruptEventLog = errors.New("event log first record unparseable — operator intervention required")

	// ErrResumeLedgerMissing signals that initForResume found a parseable
	// event log but the ledger directory has been removed. Both store
	// constructors call os.MkdirAll, so without this guard the dir would
	// be silently recreated and the log replayed into an empty store —
	// state loss disguised as resume.
	ErrResumeLedgerMissing = errors.New("ledger directory missing on resume — operator intervention required")
)

// Sentinel errors for input validation.
var (
	ErrInvalidLabelFilter = errors.New("invalid label filter: expected key:value format")
	ErrInvalidID          = errors.New("invalid identifier: must not contain path separators or '..'")
	ErrInvalidEnvKey      = errors.New("invalid environment variable key: must match [A-Za-z_][A-Za-z0-9_]*")
)
