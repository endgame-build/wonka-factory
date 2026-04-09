//go:build verify

package orch_test

import (
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfNoBeads attempts to open a BeadsStore in a temp directory. If the
// Beads/Dolt infrastructure is unavailable (e.g., no Dolt binary, no embedded
// engine), the test is skipped cleanly.
func skipIfNoBeads(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	store, err := orch.NewBeadsStore(dir, "test")
	if err != nil {
		t.Skipf("beads unavailable: %v", err)
	}
	store.Close()
}

func beadsFactory(t *testing.T) (orch.Store, string) {
	t.Helper()
	skipIfNoBeads(t)
	dir := t.TempDir()
	store, err := orch.NewBeadsStore(dir, "test")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store, dir
}

func beadsReopen(t *testing.T, dir string) orch.Store {
	t.Helper()
	store, err := orch.NewBeadsStore(dir, "test")
	require.NoError(t, err)
	return store
}

// TestBeadsStoreContract runs the full LDG contract suite against BeadsStore.
func TestBeadsStoreContract(t *testing.T) {
	skipIfNoBeads(t)
	RunStoreContractTests(t, beadsFactory, beadsReopen)
}

// TestBeadsStore_LabelRoundtrip creates a task with user labels, fetches it
// back, and verifies the Labels map round-trips through beads label encoding.
func TestBeadsStore_LabelRoundtrip(t *testing.T) {
	skipIfNoBeads(t)
	dir := t.TempDir()
	store, err := orch.NewBeadsStore(dir, "test")
	require.NoError(t, err)
	defer store.Close()

	labels := map[string]string{
		"role":        "builder",
		"branch":      "feat/login",
		"criticality": "critical",
	}
	require.NoError(t, store.CreateTask(&orch.Task{
		ID:     "labeled",
		Status: orch.StatusOpen,
		Labels: labels,
	}))

	got, err := store.GetTask("labeled")
	require.NoError(t, err)
	assert.Equal(t, labels, got.Labels)
	assert.True(t, got.IsCritical())
}
