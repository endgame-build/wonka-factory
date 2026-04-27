package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedWorkOrder writes a minimal valid work-order to a temp dir and returns
// its absolute path. Tests that need to mutate spec content can write over the
// returned files directly to simulate spec edits between runs.
func seedWorkOrder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "functional-spec.md"),
		[]byte("# CAP-1\n## UC-1.1\nAC-1.1.1: ok.\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vv-spec.md"),
		[]byte("# V-1\n- V-1.1: AC-1.1.1 covered.\n"), 0o644))
	return dir
}

// freshStore returns a FSStore over a temp directory. FS backend keeps tests
// independent of the Beads/Dolt CGO toolchain; the Store contract is the same.
func freshStore(t *testing.T) orch.Store {
	t.Helper()
	store, err := orch.NewFSStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestSeedPlannerTask_CreatesWhenMissing covers the cold-start path: no prior
// planner task, work-order is fresh. Asserts the seed carries the contract
// labels (role/branch/criticality) plus the work-order-hash sentinel that
// later runs compare against.
func TestSeedPlannerTask_CreatesWhenMissing(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))

	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, "plan-feat-x", got.ID, "deterministic ID derived from sanitized branch")
	assert.Equal(t, wo, got.Body, "body carries the absolute work-order path")
	assert.Equal(t, orch.StatusOpen, got.Status)
	assert.Equal(t, orch.RolePlanner, got.Labels[orch.LabelRole])
	assert.Equal(t, "feat/x", got.Labels[orch.LabelBranch], "branch label preserves slashes for ReadyTasks filter")
	assert.Equal(t, string(orch.Critical), got.Labels[orch.LabelCriticality])
	assert.NotEmpty(t, got.Labels[orch.LabelWorkOrderHash], "hash label is required for change detection")
	assert.Len(t, got.Labels[orch.LabelWorkOrderHash], 64, "sha256 hex digest is 64 chars")
}

// TestSeedPlannerTask_NoOpWhenOpen guards the BVV-TG-03 contract: an existing
// planner task that hasn't terminated must not be touched. This catches a
// regression where a re-run during a live planner session would race the
// agent's own writes to the task body.
func TestSeedPlannerTask_NoOpWhenOpen(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	// Pre-seed an open planner task with a stale body — represents the state
	// where the dispatcher has marked it open but the agent hasn't run yet.
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: plannerTaskID("feat/x"), Title: "plan-feat-x",
		Body: "/old/path", Status: orch.StatusOpen,
		Labels:    map[string]string{orch.LabelRole: orch.RolePlanner, orch.LabelBranch: "feat/x", orch.LabelCriticality: string(orch.Critical), orch.LabelWorkOrderHash: "stale"},
		CreatedAt: now, UpdatedAt: now,
	}))

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))

	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, "/old/path", got.Body, "open task body must not be touched")
	assert.Equal(t, "stale", got.Labels[orch.LabelWorkOrderHash], "open task hash must not be touched")
}

// TestSeedPlannerTask_NoOpWhenInProgress is the same invariant as Open but
// stricter: in-progress means an agent is actively writing. Rerunning while
// in-progress is operator confusion; the seed path must be a quiet no-op,
// not a corruption.
func TestSeedPlannerTask_NoOpWhenInProgress(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: plannerTaskID("feat/x"), Title: "plan-feat-x",
		Body: "/old/path", Status: orch.StatusInProgress,
		Assignee:  "worker-1",
		Labels:    map[string]string{orch.LabelRole: orch.RolePlanner, orch.LabelBranch: "feat/x", orch.LabelCriticality: string(orch.Critical), orch.LabelWorkOrderHash: "stale"},
		CreatedAt: now, UpdatedAt: now,
	}))

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))

	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, orch.StatusInProgress, got.Status)
	assert.Equal(t, "worker-1", got.Assignee, "assignee must survive — touching this would orphan the worker's session")
	assert.Equal(t, "/old/path", got.Body)
	assert.Equal(t, "stale", got.Labels[orch.LabelWorkOrderHash], "in-progress task hash must not be touched")
}

// TestSeedPlannerTask_NoOpWhenAssigned closes the gap between Open and
// InProgress in the no-op matrix. SeedPlannerTask keys on
// !Status.Terminal(), which covers all three mid-flight states; without
// this test, a regression flipping the predicate to
// `== StatusOpen || == StatusInProgress` would silently corrupt assigned
// tasks (orphaning the worker that's about to start on respawn).
func TestSeedPlannerTask_NoOpWhenAssigned(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: plannerTaskID("feat/x"), Title: "plan-feat-x",
		Body: "/old/path", Status: orch.StatusAssigned,
		Assignee:  "worker-1",
		Labels:    map[string]string{orch.LabelRole: orch.RolePlanner, orch.LabelBranch: "feat/x", orch.LabelCriticality: string(orch.Critical), orch.LabelWorkOrderHash: "stale"},
		CreatedAt: now, UpdatedAt: now,
	}))

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))

	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, orch.StatusAssigned, got.Status)
	assert.Equal(t, "worker-1", got.Assignee, "assignee must survive — touching this would orphan the worker's session")
	assert.Equal(t, "/old/path", got.Body)
	assert.Equal(t, "stale", got.Labels[orch.LabelWorkOrderHash], "assigned-task hash must not be touched")
}

// TestSeedPlannerTask_NoOpWhenCompletedHashMatches is the steady-state
// re-run case: nothing changed, nothing to do. Verifies the hash-equality
// fast path doesn't falsely reopen — the most common UX path, and the one
// that breaks user trust if it spams replans.
func TestSeedPlannerTask_NoOpWhenCompletedHashMatches(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	// Seed via the canonical path so the hash is computed exactly the same
	// way the production code will compare it.
	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))
	first, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	originalHash := first.Labels[orch.LabelWorkOrderHash]

	// Mark as completed (Charlie ran successfully).
	first.Status = orch.StatusCompleted
	require.NoError(t, store.UpdateTask(first))

	// Re-seed with the same work-order — must be a silent no-op.
	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))
	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, orch.StatusCompleted, got.Status, "completed task must stay completed")
	assert.Equal(t, originalHash, got.Labels[orch.LabelWorkOrderHash], "hash unchanged")
}

// TestSeedPlannerTask_ReopensWhenCompletedHashDiffers is the replan path.
// Editing functional-spec.md and re-running should reopen the planner so
// Charlie's BVV-TG-02 idempotent reconciliation can update the graph.
func TestSeedPlannerTask_ReopensWhenCompletedHashDiffers(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))
	first, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	oldHash := first.Labels[orch.LabelWorkOrderHash]

	first.Status = orch.StatusCompleted
	require.NoError(t, store.UpdateTask(first))

	// Mutate the functional-spec — simulating a spec edit between runs.
	require.NoError(t, os.WriteFile(filepath.Join(wo, "functional-spec.md"),
		[]byte("# CAP-1\n## UC-1.1\n## UC-1.2 NEW\n"), 0o644))

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))
	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, got.Status, "spec change must reopen planner")
	assert.NotEqual(t, oldHash, got.Labels[orch.LabelWorkOrderHash], "hash must reflect new content")
	assert.Equal(t, wo, got.Body, "body refreshed (still the same path here, but the contract is 'always rewrite')")
}

// TestSeedPlannerTask_ReopensWhenFailedHashDiffers extends the replan path
// to the failure-recovery case: prior planner run failed, operator edited
// the spec to fix whatever made it fail, re-runs. The reopen must trigger
// regardless of whether the prior terminal state was completed or failed.
func TestSeedPlannerTask_ReopensWhenFailedHashDiffers(t *testing.T) {
	store := freshStore(t)
	wo := seedWorkOrder(t)

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))
	first, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)

	first.Status = orch.StatusFailed
	require.NoError(t, store.UpdateTask(first))

	require.NoError(t, os.WriteFile(filepath.Join(wo, "vv-spec.md"),
		[]byte("# V-1\n- V-1.1: AC-1.1.1 covered.\n- V-1.2 NEW\n"), 0o644))

	require.NoError(t, SeedPlannerTask(store, "feat/x", wo))
	got, err := store.GetTask(plannerTaskID("feat/x"))
	require.NoError(t, err)
	assert.Equal(t, orch.StatusOpen, got.Status, "failed planner with new spec must be reopened for retry")
}

// TestSeedPlannerTask_RejectsRelativePath defends against a caller bug. The
// planner task body is consumed by Charlie running from $ORCH_PROJECT, which
// may differ from the CLI's cwd; a relative path here would silently bind to
// the wrong directory. Production callers (runLifecycle) always resolve via
// ResolveWorkOrder first, so this is belt-and-braces.
func TestSeedPlannerTask_RejectsRelativePath(t *testing.T) {
	store := freshStore(t)
	err := SeedPlannerTask(store, "feat/x", "work-packages/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

// TestSeedPlannerTask_RejectsEmptyBranch + RejectsEmptyWorkOrder are the
// trivial precondition guards. Cheap to test, expensive to debug if they
// regress (a nil-deref in production would surface as a confusing engine-
// init failure rather than a clean config error).
func TestSeedPlannerTask_RejectsEmptyBranch(t *testing.T) {
	store := freshStore(t)
	require.Error(t, SeedPlannerTask(store, "", seedWorkOrder(t)))
}

func TestSeedPlannerTask_RejectsEmptyWorkOrder(t *testing.T) {
	store := freshStore(t)
	require.Error(t, SeedPlannerTask(store, "feat/x", ""))
}

// TestHashWorkOrder_Stable confirms the hash is content-derived and stable
// across calls. If this regresses, every re-run would falsely detect a spec
// change and trigger spurious replans — a major UX bug.
func TestHashWorkOrder_Stable(t *testing.T) {
	wo := seedWorkOrder(t)
	a, err := hashWorkOrder(wo)
	require.NoError(t, err)
	b, err := hashWorkOrder(wo)
	require.NoError(t, err)
	assert.Equal(t, a, b, "hashing the same content twice must yield the same digest")
}

// TestHashWorkOrder_DetectsBoundaryShift verifies the NUL separator does its
// job. Without the separator, moving content from end-of-A to start-of-B
// would produce the same hash — a "boundary attack" that would silently mask
// real spec edits. We construct two layouts whose concatenation is identical
// and assert the hashes still differ.
func TestHashWorkOrder_DetectsBoundaryShift(t *testing.T) {
	dir1 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir1, "functional-spec.md"), []byte("AB"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir1, "vv-spec.md"), []byte("CD"), 0o644))

	dir2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "functional-spec.md"), []byte("ABC"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "vv-spec.md"), []byte("D"), 0o644))

	h1, err := hashWorkOrder(dir1)
	require.NoError(t, err)
	h2, err := hashWorkOrder(dir2)
	require.NoError(t, err)
	assert.NotEqual(t, h1, h2, "boundary shift between files must change the hash")
}
