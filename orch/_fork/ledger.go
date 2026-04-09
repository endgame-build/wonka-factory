package orch

import "errors"

// Sentinel errors for Store operations.
var (
	ErrNotFound        = errors.New("not found")
	ErrTaskExists      = errors.New("task already exists")
	ErrCycle           = errors.New("dependency cycle detected")
	ErrAlreadyAssigned = errors.New("task already assigned")
	ErrTaskNotReady    = errors.New("task not ready for assignment")
	ErrWorkerBusy      = errors.New("worker is not idle")
)

// Sentinel errors for agent invocation and worker pool.
var (
	ErrInputMissing  = errors.New("required input file missing or empty")
	ErrOutputMissing = errors.New("output file not found")
	ErrOutputInvalid = errors.New("output file failed structural validation")
	ErrPoolExhausted = errors.New("worker pool exhausted")
	ErrEnvKeyInvalid = errors.New("invalid environment variable key")
)

// Sentinel errors for engine operations.
var (
	ErrPipelineAborted   = errors.New("pipeline aborted: gap tolerance reached")
	ErrGateHalt          = errors.New("pipeline halted: quality gate failed")
	ErrRetriesExhausted  = errors.New("all retries exhausted for critical agent")
	ErrLedgerUnavailable = errors.New("ledger store unavailable")
	ErrResumeNoLedger    = errors.New("no ledger found for resume")
)

// Store is the interface for the assignment ledger — the single durable store
// for all orchestration state (DSN-07).
type Store interface {
	// Task operations
	CreateTask(t *Task) error
	GetTask(id string) (*Task, error)
	UpdateTask(t *Task) error
	GetChildren(parentID string) ([]*Task, error)

	// ReadyTasks returns tasks where status=open, all deps are terminal,
	// and assignee is empty. Results are sorted by priority (ascending),
	// then lexicographic task ID for deterministic tiebreaking (LDG-07).
	ReadyTasks() ([]*Task, error)

	// Assign atomically sets task.Status=assigned, task.Assignee=workerName,
	// and worker.CurrentTaskID=taskID. Returns error if task is not open/unassigned
	// or worker is not idle (LDG-08).
	Assign(taskID, workerName string) error

	// Worker operations
	CreateWorker(w *Worker) error
	GetWorker(name string) (*Worker, error)
	ListWorkers() ([]*Worker, error)
	UpdateWorker(w *Worker) error

	// Dependency operations. AddDep rejects edges that would create a cycle (LDG-06).
	AddDep(taskID, dependsOn string) error
	GetDeps(taskID string) ([]string, error)

	// Close releases any resources held by the store.
	// Implementations with no resources to release return nil.
	Close() error
}

// DeriveParentStatus computes what a parent task's status should be based on
// its children's statuses (LDG-16..19).
//
// Returns StatusCompleted if all children are terminal and none failed (LDG-17).
// Returns StatusFailed if any child failed and all are terminal (LDG-18).
// Returns the parent's current status if any children are non-terminal (LDG-19).
func DeriveParentStatus(store Store, parentID string) (TaskStatus, error) {
	parent, err := store.GetTask(parentID)
	if err != nil {
		return "", err
	}

	children, err := store.GetChildren(parentID)
	if err != nil {
		return "", err
	}
	if len(children) == 0 {
		return parent.Status, nil
	}

	allTerminal := true
	anyFailed := false
	for _, c := range children {
		if !c.Status.Terminal() {
			allTerminal = false
		}
		if c.Status == StatusFailed {
			anyFailed = true
		}
	}

	if !allTerminal {
		return parent.Status, nil // LDG-19
	}
	if anyFailed {
		return StatusFailed, nil // LDG-18
	}
	return StatusCompleted, nil // LDG-17
}
