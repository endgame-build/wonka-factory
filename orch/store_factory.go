package orch

import (
	"fmt"
	"os"
	"sync"
)

// StoreConstructor creates a Store backed by the given directory.
type StoreConstructor func(dir string) (Store, error)

// storeRegistry maps ledger kind names to their constructors.
var storeRegistry = map[LedgerKind]StoreConstructor{
	LedgerFS:    func(dir string) (Store, error) { return NewFSStore(dir) },
	LedgerBeads: func(dir string) (Store, error) { return NewBeadsStore(dir, "orch") },
}

// beadsFallbackOnce ensures the Beads→FS fallback warning prints only once per process.
var beadsFallbackOnce sync.Once

// NewStore creates a Store of the given kind.
// An empty kind defaults to LedgerBeads (BVV-DSP-16). Beads always falls back
// to LedgerFS when Beads/Dolt is unavailable; other explicit kinds do not fall back.
func NewStore(kind LedgerKind, dir string) (Store, error) {
	if kind == "" {
		kind = LedgerBeads
	}
	fallback := kind == LedgerBeads
	ctor, ok := storeRegistry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown ledger kind %q (available: beads, fs)", kind)
	}
	store, err := ctor(dir)
	if err != nil && fallback {
		beadsFallbackOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: beads store unavailable (%v), falling back to filesystem store\n", err)
		})
		return storeRegistry[LedgerFS](dir)
	}
	return store, err
}
