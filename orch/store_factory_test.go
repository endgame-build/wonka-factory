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
	store, kind, err := orch.NewStore(orch.LedgerFS, t.TempDir())
	require.NoError(t, err)
	defer store.Close()
	assert.Equal(t, orch.LedgerFS, kind)

	// Smoke: create and get a task.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "smoke", Status: orch.StatusOpen}))
	got, err := store.GetTask("smoke")
	require.NoError(t, err)
	assert.Equal(t, "smoke", got.ID)
}

// TestNewStore_DefaultsToBeads verifies the default kind triggers beads (which
// falls back to FS when Dolt is unavailable). The returned kind tells us which
// backend is actually active.
func TestNewStore_DefaultsToBeads(t *testing.T) {
	store, kind, err := orch.NewStore("", t.TempDir())
	require.NoError(t, err)
	defer store.Close()

	// kind is either LedgerBeads (Dolt present) or LedgerFS (fallback).
	assert.Contains(t, []orch.LedgerKind{orch.LedgerBeads, orch.LedgerFS}, kind)

	// Smoke: the store is functional regardless of backend.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "fallback", Status: orch.StatusOpen}))
	got, err := store.GetTask("fallback")
	require.NoError(t, err)
	assert.Equal(t, "fallback", got.ID)
}

// TestNewStore_UnknownKind verifies that an unknown kind returns an error.
func TestNewStore_UnknownKind(t *testing.T) {
	_, _, err := orch.NewStore("sqlite", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown ledger kind")
}
