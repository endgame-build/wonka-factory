//go:build verify

package orch_test

import (
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fsFactory(t *testing.T) (orch.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store, dir
}

func fsReopen(t *testing.T, dir string) orch.Store {
	t.Helper()
	store, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	return store
}

// TestFSStoreContract runs the full LDG contract suite against FSStore.
func TestFSStoreContract(t *testing.T) {
	RunStoreContractTests(t, fsFactory, fsReopen)
}

// TestFSStore_LabelsPersistThroughJSON creates a task with labels, reloads
// from the same directory, and verifies the labels map round-trips through
// JSON serialization.
func TestFSStore_LabelsPersistThroughJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := orch.NewFSStore(dir)
	require.NoError(t, err)

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
	store.Close()

	// Reopen and verify.
	store2, err := orch.NewFSStore(dir)
	require.NoError(t, err)
	defer store2.Close()

	got, err := store2.GetTask("labeled")
	require.NoError(t, err)
	assert.Equal(t, labels, got.Labels)
	assert.True(t, got.IsCritical())
}
