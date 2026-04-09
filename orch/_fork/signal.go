package orch

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// SetupSignalHandler sets up SIGINT/SIGTERM handling for graceful shutdown (OPS-12, RCV-13, RCV-14).
// Returns a context that is cancelled when a signal is received,
// and a cancel function for programmatic cancellation.
//
// On signal:
//  1. Cancel the context (stops dispatch loop + watchdog).
//  2. The caller is responsible for calling Cleanup after the loops exit.
//
// The ledger is NOT modified during shutdown (RCV-14).
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

// Cleanup performs shutdown actions: kill tmux sessions, release lock, close event log,
// and release store resources.
// Idempotent — safe to call multiple times. Called by both signal path and normal exit.
func Cleanup(tmux *TmuxClient, lock *PipelineLock, log *EventLog, store Store) {
	if tmux != nil {
		_ = tmux.KillServer()
	}
	if lock != nil {
		_ = lock.Release()
	}
	if log != nil {
		_ = log.Emit(Event{Kind: EventShutdown, Summary: "cleanup"})
		_ = log.Close()
	}
	if store != nil {
		if err := store.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: store.Close failed: %v\n", err)
		}
	}
}
