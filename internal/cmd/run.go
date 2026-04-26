package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/spf13/cobra"
)

// lifecycleFn abstracts Engine.Run vs Engine.Resume so the two subcommands
// share wiring.
type lifecycleFn func(*orch.Engine, context.Context) error

// newRunCmd builds the `wonka run` subcommand. Distinct from newLifecycleCmd
// because run takes a required positional <work-package> argument and seeds a
// planner task before dispatch — neither of which applies to resume.
func newRunCmd(flags *CLIFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <work-package>",
		Short: "Start a fresh lifecycle for a branch",
		Long: `Acquires a per-branch lifecycle lock, seeds a planner task pointing at
the supplied work-package directory, and dispatches the resulting graph
to completion or gap-tolerance abort. The work package must contain
functional-spec.md (the WHAT) and vv-spec.md (the PROOF); architectural
context is read from the target repo's CLAUDE.md, not from a per-feature
technical spec.

Re-running with the same work-package is a no-op (hash-matched). Re-running
after edits to either spec file reopens the planner task so the graph
reconciles against the new content.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Capture the positional before runLifecycle reads CLIFlags. Doing
			// this in RunE rather than PreRunE keeps the flag-to-struct
			// translation single-pass and avoids a hidden mutation seam.
			flags.WorkOrder = args[0]
			return runLifecycle(*flags, (*orch.Engine).Run, cmd.ErrOrStderr())
		},
	}
	addLifecycleFlags(cmd, flags)
	return cmd
}

// newLifecycleCmd builds a resume subcommand (run no longer uses this — see
// newRunCmd). Kept for backward compatibility with the resume wiring; if a
// future verb shares the same shape, prefer extending this helper rather than
// duplicating it.
func newLifecycleCmd(use, short, long string, invoke lifecycleFn, flags *CLIFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLifecycle(*flags, invoke, cmd.ErrOrStderr())
		},
	}
	addLifecycleFlags(cmd, flags)
	return cmd
}

// runLifecycle is the shared run/resume body. Passes context.Background to
// the engine because the engine installs its own SIGINT/SIGTERM handler
// per BVV-ERR-09; a second signal.Notify here would create two
// cancellation paths racing each other during shutdown.
func runLifecycle(flags CLIFlags, invoke lifecycleFn, stderr io.Writer) error {
	cfg, warnings, err := BuildEngineConfig(flags)
	if err != nil {
		return die(stderr, exitConfigError, "config error: %s", err)
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	fmt.Fprintf(stderr, "run dir: %s\n", cfg.RunDir)

	// Resolve and validate the work-order ahead of NewEngine. Failing here
	// keeps the lifecycle lock untouched and the telemetry pipeline cold —
	// the cheapest possible failure mode for a typo'd path or empty spec
	// file. ResolveWorkOrder reads no orch state, so this stays decoupled
	// from the engine's init order.
	var workOrderAbs string
	if flags.WorkOrder != "" {
		workOrderAbs, err = ResolveWorkOrder(cfg.RepoPath, flags.WorkOrder)
		if err != nil {
			return die(stderr, exitConfigError, "%s", err)
		}
		// Wire the seed callback. Closure captures workOrderAbs + branch so
		// orch never sees the work-order semantics — it only sees an opaque
		// "do this with the store before dispatch" hook (see Seed doc on
		// orch.EngineConfig).
		branch := cfg.Lifecycle.Branch
		cfg.Seed = func(store orch.Store) error {
			return SeedPlannerTask(store, branch, workOrderAbs)
		}
	}

	// Telemetry is optional; nil *Telemetry is treated as no-op by orch.
	// Build *before* engine init so invalid flags (unknown --otel-protocol,
	// --otel-insecure against a non-loopback endpoint) fail fast before
	// touching the lifecycle lock or the ledger. Exporter reachability is
	// lazy — an unreachable collector surfaces asynchronously via the OBS-04
	// error handler or at shutdown flush, not here.
	telem, shutdownTelem, err := BuildTelemetry(flags)
	if err != nil {
		return die(stderr, exitConfigError, "telemetry init failed: %s", err)
	}
	defer func() {
		// Flush budget: don't let a stuck collector block shutdown forever.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := shutdownTelem(ctx); shutdownErr != nil {
			fmt.Fprintln(stderr, "warning: telemetry shutdown:", shutdownErr)
		}
	}()
	cfg.Telemetry = telem

	engine, err := orch.NewEngine(cfg)
	if err != nil {
		return die(stderr, exitRuntimeError, "engine init failed: %s", err)
	}

	if err := invoke(engine, context.Background()); err != nil {
		return classifyEngineError(err, cfg.Lifecycle.Branch, stderr)
	}
	return nil
}

// classifyEngineError maps orch sentinel errors to CLI exit codes. See
// root.go for the exit-code table and the "why distinct codes" rationale;
// this function only routes. Signal cancellation stays silent (BVV-ERR-09);
// gap-tolerance abort collapses to nil (BVV-ERR-04).
func classifyEngineError(err error, branch string, stderr io.Writer) error {
	switch {
	case errors.Is(err, orch.ErrLifecycleAborted):
		fmt.Fprintln(stderr, "lifecycle aborted: gap tolerance reached")
		return nil

	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &exitError{code: exitSignalInterrupt}

	case errors.Is(err, orch.ErrResumeNoLedger):
		return die(stderr, exitConfigError, "no ledger for branch %q — run 'wonka run --branch %s' to start a fresh lifecycle (%s)", branch, branch, err)

	case errors.Is(err, orch.ErrLockContention):
		return die(stderr, exitLockBusy, "branch %q is already being processed by another wonka process — wait for it to finish, or run 'wonka status --branch %s' to inspect (%s)", branch, branch, err)

	case errors.Is(err, orch.ErrCorruptLock):
		return die(stderr, exitLockCorrupt, "lifecycle lock corrupt: %s", err)

	case errors.Is(err, os.ErrPermission):
		return die(stderr, exitConfigError, "permission denied — check ownership/mode of the run directory and its ledger subdirectory (%s)", err)

	// Validation-family sentinels come from bad input (operator-passed label
	// filters, env keys, task IDs). Retrying without fixing the data won't
	// help — exit 2 tells wrappers "do not retry, human fix required".
	case errors.Is(err, orch.ErrInvalidLabelFilter),
		errors.Is(err, orch.ErrInvalidID),
		errors.Is(err, orch.ErrInvalidEnvKey):
		return die(stderr, exitConfigError, "invalid input: %s", err)

	// A dependency cycle is a data defect in the ledger — the planner (or
	// whoever populated beads) must fix the graph before any lifecycle can
	// converge. Same "don't retry" semantics as validation errors.
	case errors.Is(err, orch.ErrCycle):
		return die(stderr, exitConfigError, "ledger has a dependency cycle — inspect the task graph and remove the offending edge (%s)", err)

	// Handoff limit (BVV-L-04) is a terminal task outcome, not a crash.
	// Retrying won't help; the task graph needs operator attention.
	case errors.Is(err, orch.ErrHandoffLimitReached):
		return die(stderr, exitConfigError, "task exceeded handoff limit — inspect with 'wonka status --branch %s' and reopen after investigating (%s)", branch, err)

	default:
		return die(stderr, exitRuntimeError, "lifecycle failed: %s", err)
	}
}
