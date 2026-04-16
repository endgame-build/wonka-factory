package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/endgame/wonka-factory/orch"
	"github.com/spf13/cobra"
)

// lifecycleFn abstracts Engine.Run vs Engine.Resume so the two subcommands
// share wiring.
type lifecycleFn func(*orch.Engine, context.Context) error

// newLifecycleCmd builds a run-or-resume subcommand. The only thing that
// varies between `wonka run` and `wonka resume` is the method invoked on
// the engine and the help text.
func newLifecycleCmd(use, short, long string, invoke lifecycleFn, flags *CLIFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLifecycle(*flags, invoke, cmd.ErrOrStderr())
		},
	}
	addLifecycleFlags(cmd, flags)
	return cmd
}

func newRunCmd(flags *CLIFlags) *cobra.Command {
	return newLifecycleCmd(
		"run",
		"Start a fresh lifecycle for a branch",
		`Acquires a per-branch lifecycle lock and dispatches ready tasks from
the ledger until completion or gap-tolerance abort. Expects the ledger
to be pre-populated with tasks labeled branch:<name> and role:<role> —
populate via 'bd' or your own tooling.`,
		(*orch.Engine).Run,
		flags,
	)
}

// runLifecycle is the shared run/resume body. Passes context.Background to
// the engine because orch.SetupSignalHandler (called from Engine.runLoop)
// already installs SIGINT/SIGTERM handling for graceful shutdown
// (BVV-ERR-09); a second signal.Notify here would create two cancellation
// paths racing each other during shutdown.
func runLifecycle(flags CLIFlags, invoke lifecycleFn, stderr io.Writer) error {
	cfg, warnings, err := BuildEngineConfig(flags)
	if err != nil {
		return die(stderr, exitConfigError, "config error: %s", err)
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	fmt.Fprintf(stderr, "run dir: %s\n", cfg.RunDir)

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

	default:
		return die(stderr, exitRuntimeError, "lifecycle failed: %s", err)
	}
}
