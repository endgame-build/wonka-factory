package orch

// SetTestSpawnFunc overrides the dispatcher spawn function with one that
// bypasses tmux. Must be called before Run/Resume. Test-only — defined in
// a *_test.go file so production builds cannot reference it.
func (e *Engine) SetTestSpawnFunc(f SpawnFunc) {
	e.testSpawnFunc = f
}

// SetTestLedgerDir overrides the ledger directory resolution that the engine
// performs in init/initForResume. Must be called before Run/Resume. Test-only —
// defined in a *_test.go file so production builds cannot reference it.
//
// Production code derives the ledger directory from LedgerKind + RepoPath/RunDir
// via ResolveLedgerDir; tests that need to point the beads backend at a
// controlled directory (e.g. t.TempDir()/.beads) without requiring `bd` on the
// runner's PATH set this override directly. When set, EnsureBeadsInitialised
// is also bypassed — the test owns the directory layout.
func (e *Engine) SetTestLedgerDir(p string) {
	e.testLedgerDir = p
}
