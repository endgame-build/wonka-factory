package orch

// SetTestSpawnFunc overrides the dispatcher spawn function with one that
// bypasses tmux. Must be called before Run/Resume. Test-only — defined in
// a *_test.go file so production builds cannot reference it.
func (e *Engine) SetTestSpawnFunc(f SpawnFunc) {
	e.testSpawnFunc = f
}
