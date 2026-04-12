//go:build verify

package orch_test

import (
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/endgame/wonka-factory/orch/testutil"
)

// TestMockStoreContract runs the full LDG contract suite against MockStore.
// LDG01 (durability) works because mockReopen returns the same in-memory
// instance — the "persistence" guarantee is trivially satisfied within a
// single process.
func TestMockStoreContract(t *testing.T) {
	// Capture the store instance in a closure so factory and reopen share it
	// without a package-level var (avoids races if tests ever run in parallel).
	var instance *testutil.MockStore

	factory := func(t *testing.T) (orch.Store, string) {
		t.Helper()
		instance = testutil.NewMockStore()
		return instance, "mock"
	}
	reopen := func(t *testing.T, _ string) orch.Store {
		t.Helper()
		return instance
	}

	RunStoreContractTests(t, factory, reopen)
}
