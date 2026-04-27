//go:build verify && differential

package orch_test

import (
	"fmt"
	"sort"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Layer 3: differential parity — runs the same scripted scenarios against
// BeadsStore and BDCLIStore, then compares the resulting state side-by-side.
// Catches semantic drift the contract suite can't see (different round-trip
// of priority, label ordering, terminal-status interaction with deps).
//
// Build tag `differential` keeps these out of CI's default unit-test pass —
// they are migration-window scaffolding. Plan PR-B deletes this file in the
// same diff that deletes ledger_beads.go (the differential test imports both
// stores; deleting one without the other breaks the build).
//
// Skips when bd is unavailable or when BeadsStore can't open. CI sets
// WONKA_REQUIRE_BD=1 only against the contract suite, so a CI run without
// these tags doesn't notice a missing bd.

type scenarioFn func(t *testing.T, store orch.Store)

// stateSnapshot captures the parts of a Store's contents that the
// differential test compares for equality. Only the fields wonka actually
// uses are compared — timestamps move in nanoseconds even between two
// equivalent stores and would create perpetual diff noise.
type stateSnapshot struct {
	tasks   []taskRecord
	workers []workerRecord
}

type taskRecord struct {
	ID       string
	Title    string
	Body     string
	Status   orch.TaskStatus
	Priority int
	Assignee string
	Labels   map[string]string
	Deps     []string // sorted for deterministic comparison
}

type workerRecord struct {
	Name          string
	Status        orch.WorkerStatus
	CurrentTaskID string
}

func snapshot(t *testing.T, store orch.Store) stateSnapshot {
	t.Helper()
	tasks, err := store.ListTasks()
	require.NoError(t, err)

	taskRecs := make([]taskRecord, 0, len(tasks))
	for _, task := range tasks {
		deps, err := store.GetDeps(task.ID)
		require.NoError(t, err)
		sort.Strings(deps)
		taskRecs = append(taskRecs, taskRecord{
			ID:       task.ID,
			Title:    task.Title,
			Body:     task.Body,
			Status:   task.Status,
			Priority: task.Priority,
			Assignee: task.Assignee,
			Labels:   task.Labels,
			Deps:     deps,
		})
	}

	workers, err := store.ListWorkers()
	require.NoError(t, err)
	workerRecs := make([]workerRecord, 0, len(workers))
	for _, w := range workers {
		workerRecs = append(workerRecs, workerRecord{
			Name:          w.Name,
			Status:        w.Status,
			CurrentTaskID: w.CurrentTaskID,
		})
	}
	return stateSnapshot{tasks: taskRecs, workers: workerRecs}
}

func newBeadsForDiff(t *testing.T) orch.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := orch.NewBeadsStore(dir, "diff")
	if err != nil {
		t.Skipf("beads unavailable: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newBDCLIForDiff(t *testing.T) orch.Store {
	t.Helper()
	requireBd(t)
	dir := initBdRepo(t)
	store, err := orch.NewBDCLIStore(dir, "diff")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// scenarios script identical sequences against both stores. Each function
// must use deterministic IDs (no time.Now-based names) so the resulting
// state snapshots can be compared byte-for-byte.
var scenarios = map[string]scenarioFn{
	"create_get_roundtrip": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: "diff-1", Title: "first", Body: "body", Status: orch.StatusOpen, Priority: 1,
			Labels: map[string]string{"role": "builder", "branch": "feat/x"},
		}))
	},

	"update_status_through_lifecycle": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{ID: "lc-1", Title: "lifecycle", Status: orch.StatusOpen}))
		got, _ := store.GetTask("lc-1")
		got.Status = orch.StatusInProgress
		require.NoError(t, store.UpdateTask(got))
		got.Status = orch.StatusCompleted
		require.NoError(t, store.UpdateTask(got))
	},

	"failed_status_round_trip": func(t *testing.T, store orch.Store) {
		// orch:failed disambiguator is a known divergence trap — both stores
		// must read back StatusFailed (not StatusCompleted) after closing
		// with the failed label.
		require.NoError(t, store.CreateTask(&orch.Task{ID: "f-1", Title: "fail", Status: orch.StatusOpen}))
		got, _ := store.GetTask("f-1")
		got.Status = orch.StatusFailed
		require.NoError(t, store.UpdateTask(got))
	},

	"blocked_terminal_unblocks_downstream": func(t *testing.T, store orch.Store) {
		// BVV-ERR-04a invariant — bd's native bd ready treats blocked as
		// still-blocking, so BDCLIStore computes readiness locally to match
		// BeadsStore. This scenario asserts the snapshots agree.
		require.NoError(t, store.CreateTask(&orch.Task{ID: "blk-1", Title: "b", Status: orch.StatusBlocked}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "down-1", Title: "d", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("down-1", "blk-1"))
	},

	"add_dep_idempotent": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep-a-1", Title: "a", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dep-b-1", Title: "b", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("dep-a-1", "dep-b-1"))
		require.NoError(t, store.AddDep("dep-a-1", "dep-b-1")) // idempotent
	},

	"assign_then_complete": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{ID: "asn-1", Title: "x", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "wd-1", Status: orch.WorkerIdle}))
		require.NoError(t, store.Assign("asn-1", "wd-1"))
		got, _ := store.GetTask("asn-1")
		got.Status = orch.StatusInProgress
		require.NoError(t, store.UpdateTask(got))
		got.Status = orch.StatusCompleted
		require.NoError(t, store.UpdateTask(got))
	},

	"label_replacement_on_update": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{
			ID: "lbl-1", Title: "labels", Status: orch.StatusOpen,
			Labels: map[string]string{"role": "builder", "branch": "feat/x"},
		}))
		got, _ := store.GetTask("lbl-1")
		got.Labels = map[string]string{"role": "verifier", "branch": "main"}
		require.NoError(t, store.UpdateTask(got))
	},

	"priority_round_trip": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{ID: "pri-1", Title: "p", Status: orch.StatusOpen, Priority: 0}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "pri-2", Title: "p", Status: orch.StatusOpen, Priority: 4}))
	},

	"multi_dep_chain": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateTask(&orch.Task{ID: "ch-a-1", Title: "a", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "ch-b-1", Title: "b", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "ch-c-1", Title: "c", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("ch-c-1", "ch-b-1"))
		require.NoError(t, store.AddDep("ch-b-1", "ch-a-1"))
	},

	"completed_terminal_round_trip": func(t *testing.T, store orch.Store) {
		// Pair to failed_status_round_trip: completed must NOT carry
		// orch:failed. A bug that swapped the two would surface here.
		require.NoError(t, store.CreateTask(&orch.Task{ID: "cmp-1", Title: "c", Status: orch.StatusOpen}))
		got, _ := store.GetTask("cmp-1")
		got.Status = orch.StatusCompleted
		require.NoError(t, store.UpdateTask(got))
	},

	"worker_idle_to_active_to_idle": func(t *testing.T, store orch.Store) {
		require.NoError(t, store.CreateWorker(&orch.Worker{Name: "wf-1", Status: orch.WorkerIdle}))
		w, _ := store.GetWorker("wf-1")
		w.Status = orch.WorkerActive
		w.CurrentTaskID = "phantom"
		require.NoError(t, store.UpdateWorker(w))
		w.Status = orch.WorkerIdle
		w.CurrentTaskID = ""
		require.NoError(t, store.UpdateWorker(w))
	},

	"empty_store_listing": func(t *testing.T, store orch.Store) {
		// No mutations — both stores must report empty snapshots identically.
		_ = store
	},
}

// TestStoresAgree runs each scenario against a fresh BeadsStore and a fresh
// BDCLIStore, then asserts the resulting snapshots are equal. Failures here
// are migration-blocking — wonka must not flip the default ledger constructor
// in PR-B until every scenario passes.
func TestStoresAgree(t *testing.T) {
	requireBd(t)

	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			beads := newBeadsForDiff(t)
			bdcli := newBDCLIForDiff(t)

			sc(t, beads)
			sc(t, bdcli)

			beadsSnap := snapshot(t, beads)
			bdcliSnap := snapshot(t, bdcli)

			assert.Equal(t, beadsSnap.tasks, bdcliSnap.tasks,
				"task snapshots differ for scenario %q", name)
			assert.Equal(t, beadsSnap.workers, bdcliSnap.workers,
				"worker snapshots differ for scenario %q", name)

			// Visual aid for failing snapshots — print first divergent task
			// so the diff is readable in test output rather than a wall of
			// reflect.DeepEqual hex.
			if len(beadsSnap.tasks) != len(bdcliSnap.tasks) {
				t.Logf("task counts differ: beads=%d bdcli=%d",
					len(beadsSnap.tasks), len(bdcliSnap.tasks))
			} else {
				for i := range beadsSnap.tasks {
					if fmt.Sprintf("%+v", beadsSnap.tasks[i]) != fmt.Sprintf("%+v", bdcliSnap.tasks[i]) {
						t.Logf("first divergent task at index %d:\n  beads: %+v\n  bdcli: %+v",
							i, beadsSnap.tasks[i], bdcliSnap.tasks[i])
						break
					}
				}
			}
		})
	}
}
