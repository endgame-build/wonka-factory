//go:build verify

package orch_test

import (
	"errors"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bdcliFactory creates a fresh BDCLIStore in an isolated bd-initialised
// directory. Each test gets its own database so cross-test interference
// can't mask real regressions. Skips when bd is not on PATH (mirrors
// requireBd in store_factory_test.go) so CI environments without bd stay
// green; CI sets WONKA_REQUIRE_BD=1 to flip the skip into a failure.
func bdcliFactory(t *testing.T) (orch.Store, string) {
	t.Helper()
	requireBd(t)
	dir := initBdRepo(t)
	store, err := orch.NewBDCLIStore(dir, "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}

// bdcliReopen returns a fresh BDCLIStore against an existing .beads/ dir.
// The contract suite's durability test (LDG01) closes the original store
// and re-opens here to assert state survives.
func bdcliReopen(t *testing.T, dir string) orch.Store {
	t.Helper()
	store, err := orch.NewBDCLIStore(dir, "test")
	require.NoError(t, err)
	return store
}

// TestBDCLIStore_Contract drops BDCLIStore into the same parametric suite
// every Store implementation must pass. Same shape as TestBeadsStoreContract
// in ledger_beads_test.go — every named subtest in RunStoreContractTests
// runs against this backend without modification. requireBd handles the
// CI vs local behavior split (skip locally, fail under WONKA_REQUIRE_BD).
func TestBDCLIStore_Contract(t *testing.T) {
	requireBd(t)
	RunStoreContractTests(t, bdcliFactory, bdcliReopen)
}

// TestBDCLIStore_LabelRoundtrip is the BDCLI counterpart to
// TestBeadsStore_LabelRoundtrip — guards the label encoding through the
// CLI specifically, since bd's --labels parsing is a comma-separated list
// where ours was a typed map. A regression that double-prefixes (e.g.
// "key:role:builder") or drops user labels would slip past the contract
// suite if every test happened to assert without label-shape sensitivity.
func TestBDCLIStore_LabelRoundtrip(t *testing.T) {
	store, _ := bdcliFactory(t)

	require.NoError(t, store.CreateTask(&orch.Task{
		ID:     "lr-1",
		Status: orch.StatusOpen,
		Labels: map[string]string{
			"role":   "builder",
			"branch": "feat/login",
		},
	}))

	got, err := store.GetTask("lr-1")
	require.NoError(t, err)
	require.Equal(t, "builder", got.Labels["role"])
	require.Equal(t, "feat/login", got.Labels["branch"])
	// Round-trip must not leak orch-internal labels into the user-facing map.
	for k := range got.Labels {
		require.NotContains(t, k, ":", "user-facing labels must not contain colons after parsing: %s", k)
	}
}

// TestBDCLIStore_MapBdError_AgainstRealBd is a Store-API regression guard
// covering ErrNotFound / ErrTaskExists / ErrCycle classification through
// real bd 1.0.x. Two coverage classes:
//
//   - Subtests that DO exercise mapBdError's stderr matchers
//     (get_missing_id, induced_cycle) — bd phrasing drift in 1.0.4+ trips
//     these and surfaces as a test failure, before a silent classification
//     regression can re-open the CreateTask pre-check bypass + --force
//     partial-overwrite vector.
//
//   - Subtests that exercise OTHER paths to the same sentinels
//     (create_duplicate, self_cycle) — both short-circuit inside
//     BDCLIStore before reaching bd, so they don't exercise the matcher,
//     but they pin the Store-API contract end-to-end regardless of which
//     internal path produces the sentinel.
//
// Both classes belong here: the API-contract subtests would also break if
// someone removed the in-process pre-checks without compensating, so
// keeping them adjacent makes that regression visible in one place.
func TestBDCLIStore_MapBdError_AgainstRealBd(t *testing.T) {
	store, _ := bdcliFactory(t)

	t.Run("get_missing_id_returns_ErrNotFound", func(t *testing.T) {
		// Goes through bd: GetTask → fetchIssue → runBdMapped("show") →
		// bd emits "Error: no issue found …" → mapBdError returns ErrNotFound.
		_, err := store.GetTask("does-not-exist")
		require.Error(t, err)
		assert.True(t, errors.Is(err, orch.ErrNotFound),
			"bd's not-found phrasing changed if this fails. got: %v", err)
	})

	t.Run("create_duplicate_returns_ErrTaskExists", func(t *testing.T) {
		// Short-circuits in BDCLIStore.CreateTask's fetchIssue pre-check
		// before reaching bd — so this validates the pre-check, NOT
		// mapBdError's "issue already exists" matcher. (CreateTask passes
		// --force, so the matcher path is dead on the current code; the
		// matcher stays in place for any future code that drops --force.)
		require.NoError(t, store.CreateTask(&orch.Task{ID: "dup-1", Status: orch.StatusOpen}))
		err := store.CreateTask(&orch.Task{ID: "dup-1", Status: orch.StatusOpen})
		require.Error(t, err)
		assert.True(t, errors.Is(err, orch.ErrTaskExists), "got: %v", err)
	})

	t.Run("self_cycle_returns_ErrCycle", func(t *testing.T) {
		// Short-circuits in BDCLIStore.AddDep before invoking bd —
		// validates the in-process check, NOT mapBdError. Paired with
		// induced_cycle below for matcher coverage of the cycle case.
		require.NoError(t, store.CreateTask(&orch.Task{ID: "cyc-self", Status: orch.StatusOpen}))
		err := store.AddDep("cyc-self", "cyc-self")
		require.Error(t, err)
		assert.True(t, errors.Is(err, orch.ErrCycle), "got: %v", err)
	})

	t.Run("induced_cycle_returns_ErrCycle", func(t *testing.T) {
		// Goes through bd: a→b, then b→a → bd emits
		// "Error: … would create a cycle" → mapBdError returns ErrCycle.
		require.NoError(t, store.CreateTask(&orch.Task{ID: "cyc-a", Status: orch.StatusOpen}))
		require.NoError(t, store.CreateTask(&orch.Task{ID: "cyc-b", Status: orch.StatusOpen}))
		require.NoError(t, store.AddDep("cyc-a", "cyc-b"))
		err := store.AddDep("cyc-b", "cyc-a")
		require.Error(t, err)
		assert.True(t, errors.Is(err, orch.ErrCycle),
			"bd's cycle phrasing changed if this fails. got: %v", err)
	})
}
