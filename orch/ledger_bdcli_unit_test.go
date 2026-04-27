//go:build verify

package orch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pure-unit tests for BDCLIStore — exercise every error-mapping branch and
// argv-construction path without spawning a real bd subprocess. The contract
// suite (ledger_bdcli_test.go) covers behavior against real bd; these guard
// the error mapping that's hard to provoke from real bd in a contract test.
//
// Lives in package orch (not orch_test) so it can poke private helpers and
// inject a stub execCmd. The contract suite stays in orch_test because that's
// where the parametric RunStoreContractTests lives.

func TestMapBdError_Table(t *testing.T) {
	cases := []struct {
		name         string
		stderr       string
		wantSentinel error
	}{
		{"not_found_singular", "Error: no issue found matching \"foo\"", ErrNotFound},
		{"not_found_plural", "Error: no issues found matching the provided IDs", ErrNotFound},
		{"not_found_legacy_phrasing", "task xyz: not found", ErrNotFound},
		{"already_exists", "Error: issue already exists", ErrTaskExists},
		{"unique_constraint", "UNIQUE constraint failed: issues.id", ErrTaskExists},
		{"cycle_modern", "Error: adding dependency would create a cycle", ErrCycle},
		{"cycle_legacy", "circular dependency detected", ErrCycle},
		{"unknown_returns_nil", "something went sideways", nil},
		{"empty_stderr_returns_nil", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapBdError(tc.stderr)
			if tc.wantSentinel == nil {
				assert.NoError(t, got, "expected nil for unknown stderr")
				return
			}
			require.Error(t, got)
			assert.ErrorIs(t, got, tc.wantSentinel)
		})
	}
}

func TestOrchStatusToBdString(t *testing.T) {
	cases := []struct {
		in   TaskStatus
		want string
	}{
		{StatusOpen, "open"},
		{StatusAssigned, "open"},
		{StatusInProgress, "in_progress"},
		{StatusCompleted, "closed"},
		{StatusFailed, "closed"},
		{StatusBlocked, "blocked"},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			assert.Equal(t, tc.want, orchStatusToBdString(tc.in))
		})
	}
}

func TestOrchStatusToBdString_PanicsOnUnknown(t *testing.T) {
	assert.Panics(t, func() { orchStatusToBdString(TaskStatus("alien")) })
}

func TestBdStringToOrchStatus(t *testing.T) {
	cases := []struct {
		name   string
		bd     string
		labels []string
		want   TaskStatus
	}{
		{"open", "open", nil, StatusOpen},
		{"in_progress", "in_progress", nil, StatusInProgress},
		{"closed_no_label_is_completed", "closed", nil, StatusCompleted},
		{"closed_with_failed_label_is_failed", "closed", []string{labelFailed}, StatusFailed},
		{"closed_with_user_label_only_still_completed", "closed", []string{"role:builder"}, StatusCompleted},
		{"blocked", "blocked", nil, StatusBlocked},
		{"deferred_collapses_to_blocked", "deferred", nil, StatusBlocked},
		{"pinned_collapses_to_blocked", "pinned", nil, StatusBlocked},
		{"hooked_collapses_to_blocked", "hooked", nil, StatusBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bdStringToOrchStatus(tc.bd, tc.labels))
		})
	}
}

func TestBdStringToOrchStatus_PanicsOnUnknown(t *testing.T) {
	assert.Panics(t, func() { bdStringToOrchStatus("alien", nil) })
}

func TestTaskLabelsToBd_RoundTrip(t *testing.T) {
	t.Run("user_labels_sorted", func(t *testing.T) {
		labels, err := taskLabelsToBd(&Task{
			Labels: map[string]string{"role": "builder", "branch": "feat/x"},
			Status: StatusOpen,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"branch:feat/x", "role:builder"}, labels)
	})

	t.Run("status_failed_appends_orch_label", func(t *testing.T) {
		labels, err := taskLabelsToBd(&Task{
			Labels: map[string]string{"role": "builder"},
			Status: StatusFailed,
		})
		require.NoError(t, err)
		assert.Contains(t, labels, labelFailed)
		assert.Contains(t, labels, "role:builder")
	})

	t.Run("status_completed_no_orch_label", func(t *testing.T) {
		labels, err := taskLabelsToBd(&Task{
			Labels: map[string]string{"role": "builder"},
			Status: StatusCompleted,
		})
		require.NoError(t, err)
		assert.NotContains(t, labels, labelFailed)
	})

	t.Run("comma_in_value_returns_invalid_filter", func(t *testing.T) {
		_, err := taskLabelsToBd(&Task{
			Labels: map[string]string{"branch": "feat,with,commas"},
			Status: StatusOpen,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidLabelFilter)
	})

	t.Run("comma_in_key_returns_invalid_filter", func(t *testing.T) {
		_, err := taskLabelsToBd(&Task{
			Labels: map[string]string{"k,1": "v"},
			Status: StatusOpen,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidLabelFilter)
	})
}

func TestBdDep_Target(t *testing.T) {
	// `bd show --long --json` emits dependencies[].id; `bd export` emits
	// .depends_on_id. Either populates target() so call sites stay shape-
	// agnostic.
	assert.Equal(t, "from-show", bdDep{ID: "from-show"}.target())
	assert.Equal(t, "from-export", bdDep{DependsOnID: "from-export"}.target())
	// depends_on_id wins when both are set (export-flavored input).
	assert.Equal(t, "from-export", bdDep{ID: "from-show", DependsOnID: "from-export"}.target())
}

// stubExec captures the most recent argv and returns canned output. Used by
// argv-shape tests below — they care about *what* we ask bd to do, not what
// bd would respond.
type stubExec struct {
	calls       [][]string
	stdout      []byte
	stderr      []byte
	err         error
	returnedEnv []string
}

func (s *stubExec) fn() bdExecFunc {
	return func(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
		s.calls = append(s.calls, append([]string(nil), args...))
		s.returnedEnv = env
		return s.stdout, s.stderr, s.err
	}
}

func newStubStore(t *testing.T, stub *stubExec) *BDCLIStore {
	t.Helper()
	return &BDCLIStore{
		repoPath:  t.TempDir(),
		workerDir: t.TempDir(),
		bdPath:    "/usr/bin/bd",
		baseEnv:   []string{"BEADS_ACTOR=stub"},
		execCmd:   stub.fn(),
	}
}

func TestBDCLIStore_CreateTask_BuildsExpectedArgs(t *testing.T) {
	stub := &stubExec{
		// fetchIssue's pre-check expects ErrNotFound for a fresh ID. The
		// stub returns "no issue found" on every call so the second call
		// (the actual create) sees the same canned output — but since we
		// only assert the *create* argv, that doesn't matter.
		stderr: []byte("Error: no issue found matching \"plan-feat-x\""),
		err:    errors.New("exit status 1"),
	}
	store := newStubStore(t, stub)

	err := store.CreateTask(&Task{
		ID:       "plan-feat-x",
		Title:    "Plan feature X",
		Body:     "decompose this",
		Priority: 1,
		Status:   StatusOpen,
		Labels:   map[string]string{"role": "planner", "branch": "feat/x"},
	})
	// The pre-check returns ErrNotFound, so CreateTask proceeds to the
	// create call. The create call ALSO returns the same stub error, so
	// CreateTask itself surfaces an error here. That's fine — we're asserting
	// argv shape, which is observable regardless of the canned outcome.
	require.Error(t, err)
	require.Len(t, stub.calls, 2, "expected pre-check + create calls")

	createArgs := stub.calls[1]
	assert.Equal(t, "create", createArgs[0])

	// Required flags appear with expected values. bd create has no --status
	// flag — all issues are born "open"; non-open statuses get a separate
	// `bd update --status` follow-up after the create succeeds.
	flagSet := argMap(t, createArgs[1:])
	assert.Equal(t, "plan-feat-x", flagSet["--id"])
	assert.Contains(t, createArgs, "--force", "must always pass --force for orch IDs")
	assert.Equal(t, "Plan feature X", flagSet["--title"])
	assert.Equal(t, "decompose this", flagSet["--description"])
	assert.Equal(t, "1", flagSet["--priority"])
	assert.NotContains(t, createArgs, "--status", "bd create has no --status flag")
	assert.Equal(t, "branch:feat/x,role:planner", flagSet["--labels"], "labels must be sorted and comma-joined")
	assert.Contains(t, createArgs, "--json", "always request JSON output")
}

func TestBDCLIStore_CreateTask_ExistingReturnsErrTaskExists(t *testing.T) {
	// fetchIssue's pre-check returns success → CreateTask must short-circuit
	// to ErrTaskExists rather than running bd create, which would silently
	// partial-overwrite the title and description on the existing issue.
	stub := &stubExec{
		stdout: []byte(`[{"id":"existing","title":"already there","status":"open","priority":2,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`),
	}
	store := newStubStore(t, stub)

	err := store.CreateTask(&Task{ID: "existing", Status: StatusOpen})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskExists)
	require.Len(t, stub.calls, 1, "must NOT proceed to bd create when issue already exists")
	assert.Equal(t, "show", stub.calls[0][0])
}

func TestBDCLIStore_AddDep_SelfCycleShortCircuits(t *testing.T) {
	stub := &stubExec{}
	store := newStubStore(t, stub)

	err := store.AddDep("self", "self")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCycle)
	assert.Empty(t, stub.calls, "self-cycle must not invoke bd")
}

func TestBDCLIStore_AddDep_DelegatesToBd(t *testing.T) {
	// AddDep relies on bd dep add's native idempotency rather than probing
	// the existing graph first. One subprocess per call, no pre-check.
	stub := &stubExec{}
	store := newStubStore(t, stub)

	err := store.AddDep("a-1", "b-1")
	require.NoError(t, err)
	require.Len(t, stub.calls, 1)
	assert.Equal(t, []string{"dep", "add", "a-1", "b-1"}, stub.calls[0])
}

func TestBDCLIStore_AddDep_CycleErrorMaps(t *testing.T) {
	// bd dep add rejects new cycles with the documented stderr text;
	// mapBdError translates that into ErrCycle. Single bd call.
	stub := &stubExec{
		stderr: []byte("Error: adding dependency would create a cycle"),
		err:    errors.New("exit status 1"),
	}
	store := newStubStore(t, stub)

	err := store.AddDep("a-1", "b-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCycle)
	require.Len(t, stub.calls, 1)
}

func TestBDCLIStore_RunBd_SetsBeadsActor(t *testing.T) {
	stub := &stubExec{}
	store := newStubStore(t, stub)
	store.baseEnv = []string{"BEADS_ACTOR=test-actor"}

	_, _, _ = store.runBd(context.Background(), "list", "--json")

	require.NotEmpty(t, stub.returnedEnv)
	var found bool
	for _, kv := range stub.returnedEnv {
		if kv == "BEADS_ACTOR=test-actor" {
			found = true
			break
		}
	}
	assert.True(t, found, "BEADS_ACTOR must be set on every bd invocation; env=%v", stub.returnedEnv)
}

// TestBDCLIStore_RunBd_TimeoutReturnsStoreUnavailable verifies the wall
// clock wins: a context-deadline cancellation surfaces as
// ErrStoreUnavailable rather than as a generic exec error, so the
// dispatcher's classifyEngineError can map it to the right exit code.
func TestBDCLIStore_RunBd_TimeoutReturnsStoreUnavailable(t *testing.T) {
	stub := &stubExec{}
	store := newStubStore(t, stub)
	// Stub blocks until the supplied context is cancelled, then surfaces
	// the cancellation as the run error — exactly what exec.CommandContext
	// does on real subprocess timeout.
	store.execCmd = func(ctx context.Context, _ []string, _ ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}

	// Drive the path with a parent context that is already past its deadline
	// so the runBd-internal timeout fires immediately. No need to wait the
	// full 5 seconds.
	parent, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()

	_, _, err := store.runBd(parent, "list", "--json")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrStoreUnavailable),
		"timeout must surface as ErrStoreUnavailable; got: %v", err)
}

// --- helpers ---

// argMap turns ["--flag", "value", "--other", "v2"] into {"--flag":"value", ...}
// to keep argv assertions readable without ordering assumptions.
func argMap(t *testing.T, args []string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			continue
		}
		// Skip flags-that-take-no-value by leaving them at i++ — we treat any
		// "--foo" followed by "--bar" as a boolean flag with no value to map.
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[args[i]] = args[i+1]
			i++
		}
	}
	return out
}
