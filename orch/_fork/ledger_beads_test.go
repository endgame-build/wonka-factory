package orch_test

import (
	"testing"

	"github.com/endgame/facet-scan/orch"
	"github.com/stretchr/testify/require"
)

// skipIfNoBeads skips the test when the Beads/Dolt infrastructure is unavailable.
func skipIfNoBeads(t *testing.T) {
	t.Helper()
	store, err := orch.NewBeadsStore(t.TempDir())
	if err != nil {
		t.Skipf("Beads/Dolt unavailable: %v", err)
	}
	store.Close()
}

func beadsFactory(t *testing.T) (orch.Store, string) {
	t.Helper()
	skipIfNoBeads(t)
	dir := t.TempDir()
	store, err := orch.NewBeadsStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store, dir
}

func beadsReopen(t *testing.T, dir string) orch.Store {
	t.Helper()
	store, err := orch.NewBeadsStore(dir)
	require.NoError(t, err)
	return store
}

// TestBeadsStoreContract runs the full Store contract suite against BeadsStore.
// Skips when Beads/Dolt infrastructure is unavailable.
func TestBeadsStoreContract(t *testing.T) {
	RunStoreContractTests(t, beadsFactory, beadsReopen)
}

// TestBeadsStoreProp runs property-based Store tests against BeadsStore.
// Skips when Beads/Dolt infrastructure is unavailable.
func TestBeadsStoreProp(t *testing.T) {
	RunStorePropTests(t, beadsFactory)
}
