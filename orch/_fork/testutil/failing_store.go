package testutil

import (
	"fmt"
	"sync/atomic"

	"github.com/endgame/facet-scan/orch"
)

// Compile-time assertion: FailingStore implements Store.
var _ orch.Store = (*FailingStore)(nil)

// FailingStore wraps an FSStore and returns errors after N successful calls.
// Fails after successCount total calls (including calls that would have been forwarded).
// Used to test degraded-mode behaviour (RCV-07, RCV-08).
type FailingStore struct {
	inner     orch.Store
	remaining atomic.Int64
}

// NewFailingStore wraps a store that fails after successCount successful operations.
func NewFailingStore(inner orch.Store, successCount int) *FailingStore {
	fs := &FailingStore{inner: inner}
	fs.remaining.Store(int64(successCount))
	return fs
}

func (f *FailingStore) check() error {
	if f.remaining.Add(-1) < 0 {
		return fmt.Errorf("%w: injected failure", orch.ErrLedgerUnavailable)
	}
	return nil
}

func (f *FailingStore) CreateTask(t *orch.Task) error {
	if err := f.check(); err != nil {
		return err
	}
	return f.inner.CreateTask(t)
}

func (f *FailingStore) GetTask(id string) (*orch.Task, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return f.inner.GetTask(id)
}

func (f *FailingStore) UpdateTask(t *orch.Task) error {
	if err := f.check(); err != nil {
		return err
	}
	return f.inner.UpdateTask(t)
}

func (f *FailingStore) GetChildren(parentID string) ([]*orch.Task, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return f.inner.GetChildren(parentID)
}

func (f *FailingStore) ReadyTasks() ([]*orch.Task, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return f.inner.ReadyTasks()
}

func (f *FailingStore) Assign(taskID, workerName string) error {
	if err := f.check(); err != nil {
		return err
	}
	return f.inner.Assign(taskID, workerName)
}

func (f *FailingStore) CreateWorker(w *orch.Worker) error {
	if err := f.check(); err != nil {
		return err
	}
	return f.inner.CreateWorker(w)
}

func (f *FailingStore) GetWorker(name string) (*orch.Worker, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return f.inner.GetWorker(name)
}

func (f *FailingStore) ListWorkers() ([]*orch.Worker, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return f.inner.ListWorkers()
}

func (f *FailingStore) UpdateWorker(w *orch.Worker) error {
	if err := f.check(); err != nil {
		return err
	}
	return f.inner.UpdateWorker(w)
}

func (f *FailingStore) AddDep(taskID, dependsOn string) error {
	if err := f.check(); err != nil {
		return err
	}
	return f.inner.AddDep(taskID, dependsOn)
}

func (f *FailingStore) GetDeps(taskID string) ([]string, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return f.inner.GetDeps(taskID)
}

func (f *FailingStore) Close() error {
	return f.inner.Close()
}
