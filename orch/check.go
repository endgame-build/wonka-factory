package orch

// CheckReleaseDrained returns the names of workers that are still active at
// voluntary lock release time, in any build. BVV-ERR-10a: all sessions must
// be drained before the orchestrator releases the lifecycle lock.
//
// Pairs with AssertLifecycleReleaseDrained: the Assert form panics under
// build tag verify and is a no-op otherwise. CheckReleaseDrained gives
// production builds an observable signal so the runLoop can emit a warning
// event when the invariant would have fired. Without this, a BVV-ERR-10a
// violation in a release build is silent and impossible to diagnose
// post-mortem.
//
// Returns nil on a Store error — the same conservative behavior as the
// Assert form, since failing to query workers is not itself a violation.
func CheckReleaseDrained(store Store) []string {
	workers, err := store.ListWorkers()
	if err != nil {
		return nil
	}
	var busy []string
	for _, w := range workers {
		if w.Status == WorkerActive {
			busy = append(busy, w.Name)
		}
	}
	return busy
}
