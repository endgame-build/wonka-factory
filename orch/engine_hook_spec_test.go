//go:build verify

package orch_test

import (
	"context"
	"sync"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatcher_PostSuccessHookFires verifies the dispatcher calls the
// post-success hook exactly once per task transitioning to completed, and
// only after the store write persists. Validates the seam used by Engine
// for BVV-TG-07..10 post-planner validation.
func TestDispatcher_PostSuccessHookFires(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	testutil.ParallelGraph(t, store, "feat/x", "builder", 2)

	var hookCalls []string
	d.SetPostSuccessHook(func(task *orch.Task) {
		hookCalls = append(hookCalls, task.ID)
	})
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	d.Tick(context.Background())
	d.Wait()
	d.Tick(context.Background()) // drain outcomes

	assert.Len(t, hookCalls, 2, "hook fires once per successful completion")
}

// TestDispatcher_PostSuccessHookNotCalledOnBlocked verifies that exit-2
// (terminal, non-retryable) outcomes do NOT fire the post-success hook.
// Prevents spurious graph validation on terminal failure paths.
func TestDispatcher_PostSuccessHookNotCalledOnBlocked(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "fail-1", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      "feat/x",
			orch.LabelRole:        "builder",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}))

	called := false
	d.SetPostSuccessHook(func(task *orch.Task) { called = true })
	// Exit code 2 = blocked (terminal, non-retryable); hook must not fire.
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(2))

	d.Tick(context.Background())
	d.Wait()
	d.Tick(context.Background())

	assert.False(t, called, "hook must not fire on blocked outcome")
}

// TestDispatcher_PostSuccessHookNotCalledOnRetry verifies that exit-1
// (retryable failure) does NOT fire the hook — the task transitions
// in_progress → open, which is not the terminal-success transition the
// hook keys off. Without this pin, a future refactor that accidentally
// fired the hook from handleFailure's retry branch would re-validate the
// graph mid-retry and double-count graph_* events in the audit trail.
func TestDispatcher_PostSuccessHookNotCalledOnRetry(t *testing.T) {
	d, store, lifecycle := newTestDispatcher(t, "feat/x", 1, "builder")
	// Retries > 0 so exit-1 goes through the retry branch, not terminal-fail.
	lifecycle.MaxRetries = 2
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "retry-1", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      "feat/x",
			orch.LabelRole:        "builder",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}))

	called := false
	d.SetPostSuccessHook(func(task *orch.Task) { called = true })
	// Exit code 1 = retryable failure; task goes in_progress → open.
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(1))

	d.Tick(context.Background())
	d.Wait()
	d.Tick(context.Background())

	assert.False(t, called, "hook must not fire on retryable failure (in_progress → open)")
	// Sanity-check that the retry actually happened — otherwise the test is
	// vacuous. The task should be back to open (or re-assigned for retry).
	got, err := store.GetTask("retry-1")
	require.NoError(t, err)
	assert.NotEqual(t, orch.StatusCompleted, got.Status,
		"task must not be completed — retry path was expected")
}

// TestDispatcher_PostSuccessHookNotCalledOnHandoff verifies that exit-3
// (handoff) does NOT fire the hook — handoff preserves status=in_progress
// (BVV-DSP-14) and is not a completion transition. Graph validation
// against a partially-completed planner task would produce spurious
// graph_invalid events on every handoff.
func TestDispatcher_PostSuccessHookNotCalledOnHandoff(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "handoff-1", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      "feat/x",
			orch.LabelRole:        "builder",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}))

	var mu sync.Mutex
	called := false
	d.SetPostSuccessHook(func(task *orch.Task) {
		mu.Lock()
		called = true
		mu.Unlock()
	})
	// Exit code 3 = handoff; task status stays in_progress.
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(3))

	// One tick dispatches; the first handoff fires and the dispatcher
	// immediately re-launches the SpawnFunc (test-mode handoff loop).
	// We don't wait for the full handoff budget — just confirm the hook
	// isn't called during the first handoff.
	d.Tick(context.Background())
	// Short drain window. The handoff machinery recycles the task; we
	// don't need Wait() because we're asserting the hook is NEVER called.
	d.Tick(context.Background())

	mu.Lock()
	sawCall := called
	mu.Unlock()
	assert.False(t, sawCall, "hook must not fire on handoff — status stays in_progress")
}

// TestDispatcher_PostSuccessHookSuppressedOnStoreFailure verifies the
// `persisted` guard at the core of the hook-fire condition: when
// UpdateTask fails during terminateAndRelease, the hook must NOT fire.
// Without this guard, a store-write retry would trigger the hook twice,
// producing duplicate graph_validated events and a duplicate escalation
// creation attempt. The escalation path's ErrTaskExists tolerance partly
// masks the issue for escalations, but duplicate graph_validated events
// in the audit trail are a real audit-integrity bug.
func TestDispatcher_PostSuccessHookSuppressedOnStoreFailure(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	require.NoError(t, store.CreateTask(&orch.Task{
		ID: "success-but-store-fail", Status: orch.StatusOpen,
		Labels: map[string]string{
			orch.LabelBranch:      "feat/x",
			orch.LabelRole:        "builder",
			orch.LabelCriticality: string(orch.NonCritical),
		},
	}))

	called := false
	d.SetPostSuccessHook(func(task *orch.Task) { called = true })
	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))

	// Dispatch assigns the task; the spawn-then-UpdateTask flow in test
	// mode writes StatusInProgress first, then processOutcome writes
	// StatusCompleted. We can't inject the failure via SetUpdateTaskErr
	// before Tick (it would also block the in_progress transition and
	// the dispatcher would hit failTaskAndRelease — never reaching the
	// success path we're testing). Instead: let Tick dispatch, then
	// inject the error before Drain processes the outcome, so the
	// terminate-path UpdateTask is the call that fails.
	d.Tick(context.Background())
	d.Wait()
	// At this point the outcome is in the channel. Inject the failure
	// now so the next Tick's Drain sees it when calling UpdateTask on
	// StatusCompleted.
	store.SetUpdateTaskErr(errStoreDown)
	d.Tick(context.Background())

	assert.False(t, called,
		"hook must not fire when UpdateTask fails — prevents double-fire on store retry (BVV-TG-07..10 audit integrity)")
}

// errStoreDown is a sentinel for TestDispatcher_PostSuccessHookSuppressedOnStoreFailure.
var errStoreDown = &storeDownErr{}

type storeDownErr struct{}

func (*storeDownErr) Error() string { return "store temporarily unavailable" }

// TestDispatcher_AbortLifecycle verifies the Dispatcher's exported abort
// method stops further dispatch and blocks remaining open tasks — the
// hook for BVV-TG-07..10 validation failures relies on this seam.
func TestDispatcher_AbortLifecycle(t *testing.T) {
	d, store, _ := newTestDispatcher(t, "feat/x", 2, "builder")
	// Three independent ready tasks.
	testutil.ParallelGraph(t, store, "feat/x", "builder", 3)

	d.SetSpawnFunc(testutil.ImmediateSpawnFunc(0))
	d.AbortLifecycle("test:forced")

	// Post-abort tick must dispatch nothing.
	result := d.Tick(context.Background())
	assert.Equal(t, 0, result.Dispatched, "dispatch after abort must be a no-op")

	// All previously-open tasks must now be blocked (abortCleanup).
	tasks, err := store.ListTasks("branch:feat/x")
	require.NoError(t, err)
	for _, task := range tasks {
		assert.Equal(t, orch.StatusBlocked, task.Status,
			"task %s should be blocked after AbortLifecycle", task.ID)
	}

	// The reason must be retrievable via AbortReason() — the terminal
	// lifecycle_completed anchor relies on this accessor to classify the
	// abort cause (graph_invalid vs. gap_tolerance_exceeded vs. other).
	assert.Equal(t, "test:forced", d.AbortReason(),
		"AbortReason() must return the reason passed to AbortLifecycle")
}

// TestDispatcher_AbortReasonEmptyByDefault verifies that a dispatcher that
// has not yet aborted returns an empty reason. Callers (emitLifecycleCompleted)
// use the empty-string signal to fall back to the legacy gap-tolerance
// default — a non-empty zero value here would break that contract.
func TestDispatcher_AbortReasonEmptyByDefault(t *testing.T) {
	d, _, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	assert.Empty(t, d.AbortReason(), "fresh Dispatcher must not carry an abort reason")
}

// TestDispatcher_AbortLifecycle_FirstWins pins the first-wins invariant:
// once a non-empty abort reason is set (by AbortLifecycle or by any of the
// internal setters in handleTerminalFailure), subsequent calls do not
// overwrite it. This gives operators the earliest and typically
// most-specific cause even when a graph_invalid abort races with a
// critical-task failure in the same Drain cycle.
func TestDispatcher_AbortLifecycle_FirstWins(t *testing.T) {
	d, _, _ := newTestDispatcher(t, "feat/x", 1, "builder")
	d.AbortLifecycle("graph_invalid:BVV-TG-09")
	d.AbortLifecycle("")                       // empty no-op
	d.AbortLifecycle("gap_tolerance_exceeded") // non-empty must also be ignored
	assert.Equal(t, "graph_invalid:BVV-TG-09", d.AbortReason(),
		"first non-empty reason wins across subsequent AbortLifecycle calls")
}
