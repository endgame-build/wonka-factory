//go:build verify

package orch_test

import (
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewStore_ExplicitFS creates an FSStore explicitly by kind.
func TestNewStore_ExplicitFS(t *testing.T) {
	store, err := orch.NewStore(orch.LedgerFS, t.TempDir())
	require.NoError(t, err)
	defer store.Close()

	// Smoke: create and get a task.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "smoke", Status: orch.StatusOpen}))
	got, err := store.GetTask("smoke")
	require.NoError(t, err)
	assert.Equal(t, "smoke", got.ID)
}

// TestNewStore_DefaultsToBeads verifies the default kind triggers beads (which
// falls back to FS when Dolt is unavailable).
func TestNewStore_DefaultsToBeads(t *testing.T) {
	// Empty kind = LedgerBeads. Since Dolt is likely unavailable in test,
	// the factory falls back to FS silently.
	store, err := orch.NewStore("", t.TempDir())
	require.NoError(t, err)
	defer store.Close()

	// Smoke: the fallback store is functional.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "fallback", Status: orch.StatusOpen}))
	got, err := store.GetTask("fallback")
	require.NoError(t, err)
	assert.Equal(t, "fallback", got.ID)
}

// TestNewStore_UnknownKind verifies that an unknown kind returns an error.
func TestNewStore_UnknownKind(t *testing.T) {
	_, err := orch.NewStore("sqlite", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown ledger kind")
}
