//go:build !verify

package orch

// Runtime invariant assertions — no-ops when built without -tags verify.
// Dispatch code calls these unconditionally; the build tag compiles them away.

func AssertTerminalIrreversibility(_, _ TaskStatus)       {}
func AssertSingleAssignment(_ Store, _ string)            {}
func AssertDependencyOrdering(_ Store, _ string)          {}
func AssertLifecycleExclusion(_ *LifecycleLock, _ string) {}
func AssertBoundedDegradation(_ *GapTracker, _ int)       {}
