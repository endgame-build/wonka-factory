package orch_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fsFactory creates a fresh FSStore in a temp directory.
func fsFactory(t *testing.T) (orch.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	return store, dir
}

// fsReopen creates a new FSStore on the same directory (simulates restart).
func fsReopen(t *testing.T, dir string) orch.Store {
	t.Helper()
	store, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	return store
}

// TestFSStoreContract runs the full Store contract suite against FSStore.
// Covers: LDG-01, LDG-02, LDG-03, LDG-04, LDG-06, LDG-07, LDG-08, LDG-09,
// LDG-10, LDG-12, LDG-14, LDG-14a, LDG-15, LDG-16, LDG-17, CHK-01.
func TestFSStoreContract(t *testing.T) {
	RunStoreContractTests(t, fsFactory, fsReopen)
}

// TestFSStoreProp runs property-based Store tests against FSStore.
func TestFSStoreProp(t *testing.T) {
	RunStorePropTests(t, fsFactory)
}

// --- FSStore-specific tests (not in the contract suite) ---

// TestLDG10_SerializedAssignment_CrossProcess verifies [LDG-10, LDG-11]:
// concurrent assigns are serialised across FSStore instances sharing a directory.
func TestLDG10_SerializedAssignment_CrossProcess(t *testing.T) {
	dir := t.TempDir()
	store1, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	store2, err := orch.NewFSStore(dir)
	require.NoError(t, err)

	require.NoError(t, store1.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
	require.NoError(t, store1.CreateTask(&orch.Task{ID: "t2", Type: orch.TypeAgent, Status: orch.StatusOpen}))
	require.NoError(t, store1.CreateWorker(&orch.Worker{Name: "w1", Status: orch.WorkerIdle}))

	// Two FSStore instances race to assign different tasks to the same worker.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = store1.Assign("t1", "w1") }()
	go func() { defer wg.Done(); errs[1] = store2.Assign("t2", "w1") }()
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes, "exactly one cross-process assign should succeed")
}

// TestLDG07a_DependencyResolvesOnStatus verifies [LDG-07a]: dependency resolution operates
// on task status, not artefact presence. A task with output but non-terminal status still blocks.
func TestLDG07a_DependencyResolvesOnStatus(t *testing.T) {
	store := newTestStore(t)

	// dep is in_progress (non-terminal) — even though an output file might exist.
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "dep", Type: orch.TypeAgent, Status: orch.StatusInProgress,
		Output: "/some/output.md", // output path set, but status is non-terminal
	}))
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "waiter", Type: orch.TypeAgent, Status: orch.StatusOpen,
	}))
	require.NoError(t, store.AddDep("waiter", "dep"))

	ready, err := store.ReadyTasks()
	require.NoError(t, err)
	for _, r := range ready {
		assert.NotEqual(t, "waiter", r.ID, "waiter must NOT be ready — dep is non-terminal despite having output")
	}
}

// TestLDG11_SharedDirectoryVisibility verifies [LDG-11]: two FSStore instances on the same
// directory see each other's writes. This tests shared on-disk state visibility, not
// concurrent locking (which is validated by flock at the filesystem layer).
func TestLDG11_SharedDirectoryVisibility(t *testing.T) {
	dir := t.TempDir()
	store1, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	store2, err := orch.NewFSStore(dir)
	require.NoError(t, err)

	// Create a task and a worker in store1.
	require.NoError(t, store1.CreateTask(&orch.Task{
		ID: "task-1", Type: orch.TypeAgent, Status: orch.StatusOpen, Priority: 0,
	}))
	require.NoError(t, store1.CreateWorker(&orch.Worker{Name: "w-01", Status: orch.WorkerIdle}))

	// Both stores must see the same state (serialised through filesystem).
	task, err := store2.GetTask("task-1")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, task.Status)

	// Assign via store1.
	require.NoError(t, store1.Assign("task-1", "w-01"))

	// store2 must see the updated status.
	task, err = store2.GetTask("task-1")
	require.NoError(t, err)
	assert.Equal(t, orch.StatusAssigned, task.Status)
	assert.Equal(t, "w-01", task.Assignee)
}

// --- Store factory tests ---

func TestNewStore_UnknownKindReturnsError(t *testing.T) {
	_, err := orch.NewStore("bogus", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown ledger kind")
}

func TestNewStore_KindProducesFunctionalStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind orch.LedgerKind
	}{
		{"explicit_fs", orch.LedgerFS},
		{"empty_defaults_to_beads_or_fs", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := orch.NewStore(tc.kind, dir)
			require.NoError(t, err)
			t.Cleanup(func() { store.Close() })

			require.NoError(t, store.CreateTask(&orch.Task{ID: "t1", Type: orch.TypeAgent, Status: orch.StatusOpen}))
			got, err := store.GetTask("t1")
			require.NoError(t, err)
			assert.Equal(t, "t1", got.ID)
		})
	}
}

// TestNewStore_ExplicitFS_CreatesDirectories verifies FSStore creates its
// expected subdirectory layout without any Beads fallback.
func TestNewStore_ExplicitFS_CreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	store, err := orch.NewStore(orch.LedgerFS, dir)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	assert.DirExists(t, filepath.Join(dir, "tasks"))
	assert.DirExists(t, filepath.Join(dir, "workers"))
}
