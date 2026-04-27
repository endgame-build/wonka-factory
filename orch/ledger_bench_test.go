//go:build verify && bench

package orch_test

import (
	"fmt"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/require"
)

// Benchmarks exist to catch regressions against the per-call budget the plan
// committed to: ReadyTasks p95 ≤ 200 ms, ListTasks p95 ≤ 200 ms, CreateTask
// p95 ≤ 120 ms, Assign uncontended p95 ≤ 250 ms — all on ubuntu-latest.
//
// Run via:  go test -bench=. -tags 'verify bench' ./orch/...
//
// The numbers reported by `go test -bench` are mean wall time per iteration,
// not p95 — but a regression that pushes mean past the budget is the only
// kind of regression a benchmark can catch deterministically. p95 deltas
// require statistical comparison via benchstat across runs, which is the
// operator's job.

func newBDCLIForBench(b *testing.B) (orch.Store, string) {
	b.Helper()
	if !orch.BeadsCLIAvailable() {
		b.Skip("bd CLI not on PATH")
	}
	dir := initBdRepo(b)
	store, err := orch.NewBDCLIStore(dir, "bench")
	require.NoError(b, err)
	b.Cleanup(func() { _ = store.Close() })
	return store, dir
}

// preloadTasks creates count tasks with deterministic IDs and minimal
// per-task content. Used as the b.SetupSubTests hook for List/Ready
// benchmarks — those operations are O(N) so we measure against realistic
// graph sizes.
func preloadTasks(b *testing.B, store orch.Store, count int) {
	b.Helper()
	for i := 0; i < count; i++ {
		require.NoError(b, store.CreateTask(&orch.Task{
			ID:     fmt.Sprintf("bench-%d", i),
			Title:  fmt.Sprintf("task %d", i),
			Status: orch.StatusOpen,
			Labels: map[string]string{"branch": "feat/bench"},
		}))
	}
}

func BenchmarkBDCLI_CreateTask(b *testing.B) {
	store, _ := newBDCLIForBench(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := store.CreateTask(&orch.Task{
			ID:     fmt.Sprintf("c-%d", i),
			Title:  "create",
			Status: orch.StatusOpen,
			Labels: map[string]string{"branch": "feat/x"},
		})
		require.NoError(b, err)
	}
}

func BenchmarkBDCLI_GetTask(b *testing.B) {
	store, _ := newBDCLIForBench(b)
	require.NoError(b, store.CreateTask(&orch.Task{ID: "g-1", Title: "g", Status: orch.StatusOpen}))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.GetTask("g-1")
		require.NoError(b, err)
	}
}

func BenchmarkBDCLI_ReadyTasks_100(b *testing.B) {
	store, _ := newBDCLIForBench(b)
	preloadTasks(b, store, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.ReadyTasks("branch:feat/bench")
		require.NoError(b, err)
	}
}

func BenchmarkBDCLI_ListTasks_100(b *testing.B) {
	store, _ := newBDCLIForBench(b)
	preloadTasks(b, store, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.ListTasks("branch:feat/bench")
		require.NoError(b, err)
	}
}

func BenchmarkBDCLI_Assign(b *testing.B) {
	store, _ := newBDCLIForBench(b)
	// Pre-create N tasks and 1 worker. Each iteration assigns and unassigns
	// to bring the worker back to idle without paying CreateWorker cost.
	for i := 0; i < b.N; i++ {
		require.NoError(b, store.CreateTask(&orch.Task{
			ID: fmt.Sprintf("a-%d", i), Title: "a", Status: orch.StatusOpen,
		}))
	}
	require.NoError(b, store.CreateWorker(&orch.Worker{Name: "wb-1", Status: orch.WorkerIdle}))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := store.Assign(fmt.Sprintf("a-%d", i), "wb-1")
		require.NoError(b, err)
		// Reset worker to idle for next iteration. No need to also
		// unassign the task — different task IDs each loop.
		w, _ := store.GetWorker("wb-1")
		w.Status = orch.WorkerIdle
		w.CurrentTaskID = ""
		require.NoError(b, store.UpdateWorker(w))
	}
}
