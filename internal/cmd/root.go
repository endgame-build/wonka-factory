package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Exit codes returned via *exitError and mapped by main.
// 130 is the conventional SIGINT exit code — distinguishes a Ctrl-C from
// an operational failure. Lock contention and corruption use distinct codes
// so scripts can branch on "wait and retry" vs "human intervention".
const (
	exitRuntimeError    = 1
	exitConfigError     = 2
	exitLockCorrupt     = 3
	exitLockBusy        = 4
	exitSignalInterrupt = 130
)

// Execute builds the root command and runs it. Called from cmd/wonka/main.go.
func Execute() error {
	return NewRootCmd().Execute()
}

// NewRootCmd returns a fresh root command with all subcommands registered.
// A factory (rather than a package-level var) lets tests run in parallel
// without sharing flag state.
func NewRootCmd() *cobra.Command {
	flags := &CLIFlags{}

	root := &cobra.Command{
		Use:   "wonka",
		Short: "DAG-driven orchestrator for autonomous software delivery agents",
		Long: `wonka dispatches tasks from a ledger to agents, supervising workers
through a per-branch lifecycle.

'wonka run --branch <name> <work-package>' seeds a planner task pointing
at the supplied work-package directory and dispatches the resulting
graph. The work package must contain functional-spec.md (the WHAT) and
vv-spec.md (the PROOF); architectural context is read from the target
repo's CLAUDE.md. 'wonka resume' re-enters an interrupted lifecycle
without touching the ledger contents.`,
		SilenceUsage:  true, // engine errors shouldn't dump help text
		SilenceErrors: true, // main prints the error once in its own style
	}

	// Persistent flags shared by run, resume, status.
	root.PersistentFlags().StringVar(&flags.Branch, "branch", "", "lifecycle branch name (required)")
	root.PersistentFlags().StringVar(&flags.Ledger, "ledger", defaultLedger, "ledger backend: beads or fs")
	root.PersistentFlags().StringVar(&flags.AgentDir, "agent-dir", defaultAgentDir, "directory containing role instruction files (OOMPA.md, LOOMPA.md, CHARLIE.md); relative paths resolve under --repo")
	root.PersistentFlags().StringVar(&flags.RunDir, "run-dir", "", "orchestrator state directory (default: <repo>/.wonka/<sanitized-branch>)")
	root.PersistentFlags().StringVar(&flags.RepoPath, "repo", "", "repository path the agents operate against (default: current directory)")

	if err := root.MarkPersistentFlagRequired("branch"); err != nil {
		panic(fmt.Errorf("wire --branch required: %w", err)) // programmer error only
	}

	root.AddCommand(newRunCmd(flags))
	root.AddCommand(newResumeCmd(flags))
	root.AddCommand(newStatusCmd(flags))

	return root
}

// die writes an error message and returns an *exitError for main to unwrap.
// Subcommands pass cmd.ErrOrStderr() so tests can capture the output.
func die(w io.Writer, code int, format string, args ...any) error {
	fmt.Fprintf(w, format+"\n", args...)
	return &exitError{code: code}
}

type exitError struct {
	code int
}

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }
func (e *exitError) ExitCode() int { return e.code }
