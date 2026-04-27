//go:build verify

package orch_test

import (
	"os"
	"testing"

	"github.com/endgame/wonka-factory/orch"
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
// runs against this backend without modification.
func TestBDCLIStore_Contract(t *testing.T) {
	requireBd(t)
	if os.Getenv("WONKA_REQUIRE_BD") == "" {
		// In CI WONKA_REQUIRE_BD=1 is set, so requireBd above never skips.
		// In local dev without bd it does. Surface the chosen mode in the
		// test log so a confusing skip doesn't mystify the operator.
		t.Logf("WONKA_REQUIRE_BD unset; if bd is missing this test will skip rather than fail")
	}
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
