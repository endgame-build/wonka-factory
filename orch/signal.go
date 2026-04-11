package orch

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// SetupSignalHandler sets up SIGINT/SIGTERM handling for graceful shutdown
// (BVV-ERR-09). Returns a context that is cancelled when a signal is
// received, and a cancel function for programmatic cancellation.
//
// On signal:
//  1. Cancel the context (stops dispatch loop + watchdog).
//  2. The caller is responsible for calling Cleanup after the loops exit.
//
// The ledger is NOT modified during shutdown (BVV-S-02a: terminal
// irreversibility is preserved across interrupted runs, so signal-time
// never transitions tasks).
func SetupSignalHandler() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
	}()

	return ctx, cancel
}

// Cleanup performs shutdown actions: kill tmux sessions, release lock,
// close event log, and release store resources. Idempotent — safe to call
// multiple times. Called by both the signal path and normal exit.
//
// BVV-ERR-10a interaction: this is the INVOLUNTARY release path. The lock
// is released unconditionally, without the "all sessions drained" check
// that the dispatcher enforces (AssertLifecycleReleaseDrained at the
// voluntary release site, under build tag verify). A signal mid-run
// legitimately leaves sessions alive, and the caller's responsibility is
// to cancel the dispatch context first so goroutines can exit cleanly
// before Cleanup runs.
//
// Every shutdown action is best-effort and logged to stderr: a silent
// KillServer failure can leave zombie tmux sessions that race the next
// run and double-commit against the ledger.
//
// No shutdown event is emitted. BVV's 17 event kinds don't include one —
// the last entry in the log is whatever the dispatcher emitted before the
// signal arrived, which is semantically correct (the lifecycle didn't
// complete; it was interrupted).
func Cleanup(tmux *TmuxClient, lock *LifecycleLock, log *EventLog, store Store) {
	if tmux != nil {
		if err := tmux.KillServer(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: tmux.KillServer failed (sessions may be leaked): %v\n", err)
		}
	}
	if lock != nil {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: lock.Release failed (stale lock may remain): %v\n", err)
		}
	}
	if log != nil {
		if err := log.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: event log Close failed: %v\n", err)
		}
	}
	if store != nil {
		if err := store.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: store.Close failed: %v\n", err)
		}
	}
}
