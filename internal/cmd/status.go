package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/endgame/wonka-factory/orch"
	"github.com/spf13/cobra"
)

// Must agree with orch/engine.go:252,275. TODO(phase-8): orch should export
// this or an OpenLedgerForRun helper.
const ledgerSubdir = "ledger"

// statusOutputFormat selects how the status command renders results.
type statusOutputFormat string

const (
	statusFormatTable statusOutputFormat = "table"
	statusFormatJSON  statusOutputFormat = "json"
)

// newStatusCmd wires the `wonka status` subcommand — read-only table or JSON
// of every task labeled branch:<name>. Does NOT acquire the lifecycle lock
// (must work while a run is live). Does NOT construct an engine.
func newStatusCmd(flags *CLIFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show tasks for a branch (read-only)",
		Long: `Queries the ledger for all tasks labeled branch:<name> and prints them
as an aligned table (default) or JSON array (--json). Does not acquire
the lifecycle lock — safe to run while a lifecycle is active.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format := statusFormatTable
			if asJSON {
				format = statusFormatJSON
			}
			return showStatus(*flags, format, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON array instead of aligned table")
	return cmd
}

// showStatus opens the store, lists tasks for the branch, and writes the
// rendered output. Separate from cobra wiring so tests can exercise the
// full flow without parsing flags.
func showStatus(flags CLIFlags, format statusOutputFormat, stdout, stderr io.Writer) error {
	branch := strings.TrimSpace(flags.Branch)
	if branch == "" {
		return die(stderr, exitConfigError, "status: --branch is required")
	}

	repoPath, err := resolveRepoPath(flags.RepoPath)
	if err != nil {
		return die(stderr, exitConfigError, "%s", err)
	}

	runDir := flags.RunDir
	if runDir == "" {
		runDir = filepath.Join(repoPath, ".wonka", sanitizeBranch(branch))
	}
	ledgerDir := filepath.Join(runDir, ledgerSubdir)
	if _, err := os.Stat(ledgerDir); err != nil {
		return die(stderr, exitConfigError, "no ledger at %s — is the branch spelled correctly, or run 'wonka run --branch %s' to create one", ledgerDir, branch)
	}

	ledgerKind, err := parseLedgerKind(flags.Ledger)
	if err != nil {
		return die(stderr, exitConfigError, "%s", err)
	}

	store, actualKind, err := orch.NewStore(ledgerKind, ledgerDir)
	if err != nil {
		return die(stderr, exitRuntimeError, "open ledger: %s", err)
	}
	defer store.Close()

	tasks, err := store.ListTasks(orch.LabelBranch + ":" + branch)
	if err != nil {
		return die(stderr, exitRuntimeError, "list tasks: %s", err)
	}

	switch format {
	case statusFormatJSON:
		// Compact output — scripted consumers can re-indent via `jq .`.
		if err := json.NewEncoder(stdout).Encode(tasks); err != nil {
			return die(stderr, exitRuntimeError, "encode json: %s", err)
		}
	default:
		fmt.Fprintf(stderr, "branch: %s    ledger: %s", branch, actualKind)
		if actualKind != ledgerKind {
			fmt.Fprintf(stderr, " (requested: %s, fallback)", ledgerKind)
		}
		fmt.Fprintln(stderr)
		renderTable(stdout, tasks)
	}
	return nil
}
