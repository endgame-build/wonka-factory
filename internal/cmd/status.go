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

// Must agree with the ledger subdirectory orch's Engine opens under RunDir.
// TODO(orch-seam): orch currently derives this literal inside Engine.init;
// either export a LedgerSubdir constant or an OpenLedgerForRun(runDir,
// kind) helper that returns (Store, LedgerKind, error). Today a rename in
// orch would silently break `wonka status`.
const ledgerSubdir = "ledger"

// warnLedgerFallback prints a stderr warning when the store factory fell
// back to a different backend than was requested. A scripted
// `wonka status --json --ledger beads` on an FS fallback would otherwise
// see data from the wrong backend with zero signal.
func warnLedgerFallback(stderr io.Writer, requested, actual orch.LedgerKind) {
	if actual == requested {
		return
	}
	fmt.Fprintf(stderr, "warning: ledger fallback (requested: %s, using: %s)\n", requested, actual)
}

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
		Args: cobra.NoArgs,
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
// rendered output. Never acquires the lifecycle lock — safe alongside a
// live run.
func showStatus(flags CLIFlags, format statusOutputFormat, stdout, stderr io.Writer) error {
	branch := strings.TrimSpace(flags.Branch)
	if branch == "" {
		return die(stderr, exitConfigError, "status: --branch is required")
	}

	repoPath, err := resolveRepoPath(flags.RepoPath)
	if err != nil {
		return die(stderr, exitConfigError, "%s", err)
	}

	runDir := resolveRunDir(repoPath, branch, flags.RunDir)
	ledgerDir := filepath.Join(runDir, ledgerSubdir)
	// Pre-stat: opening the ledger may create the directory via the selected
	// store backend constructor, so without this a typo'd --branch silently
	// creates an empty ledger and renders an empty table.
	if _, err := os.Stat(ledgerDir); err != nil {
		if os.IsNotExist(err) {
			return die(stderr, exitConfigError, "no ledger at %s — is the branch spelled correctly, or run 'wonka run --branch %s' to create one", ledgerDir, branch)
		}
		return die(stderr, exitRuntimeError, "stat ledger %s: %s", ledgerDir, err)
	}

	ledgerKind, err := parseLedgerKind(flags.Ledger)
	if err != nil {
		return die(stderr, exitConfigError, "%s", err)
	}

	store, actualKind, err := orch.NewStore(ledgerKind, ledgerDir)
	if err != nil {
		return die(stderr, exitRuntimeError, "open ledger: %s", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintln(stderr, "warning: close ledger:", cerr)
		}
	}()

	tasks, err := store.ListTasks(orch.LabelBranch + ":" + branch)
	if err != nil {
		return die(stderr, exitRuntimeError, "list tasks: %s", err)
	}

	warnLedgerFallback(stderr, ledgerKind, actualKind)

	switch format {
	case statusFormatJSON:
		// Marshal to buffer first: json.NewEncoder(stdout) streams bytes
		// immediately and a marshal failure mid-array would leave partial
		// garbage on stdout before die() writes the error to stderr.
		payload, err := json.Marshal(tasks)
		if err != nil {
			return die(stderr, exitRuntimeError, "encode json: %s", err)
		}
		if _, err := stdout.Write(payload); err != nil {
			return die(stderr, exitRuntimeError, "write json: %s", err)
		}
		if _, err := stdout.Write([]byte{'\n'}); err != nil {
			return die(stderr, exitRuntimeError, "write json: %s", err)
		}
	default:
		// Header to stderr so `wonka status | awk` can slice table columns
		// without stripping a banner line.
		fmt.Fprintf(stderr, "branch: %s    ledger: %s\n", branch, actualKind)
		if err := renderTable(stdout, tasks); err != nil {
			return die(stderr, exitRuntimeError, "render table: %s", err)
		}
	}
	return nil
}
