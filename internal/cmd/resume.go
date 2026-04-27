package cmd

import (
	"fmt"

	"github.com/endgame/wonka-factory/orch"
	"github.com/spf13/cobra"
)

func newResumeCmd(flags *CLIFlags) *cobra.Command {
	cmd := newLifecycleCmd(
		"resume",
		"Resume an interrupted lifecycle",
		`Re-acquires the per-branch lifecycle lock, reconciles state from the
prior run (stale assignments, orphan tmux sessions, recovered gap/retry/
handoff counters from the event log), then dispatches remaining ready
tasks until completion.

Resume reads the work-order from the existing planner task body — pass a
work-package only on 'wonka run', never on 'wonka resume'.`,
		(*orch.Engine).Resume,
		flags,
	)
	// Override cobra.NoArgs to point the user at `run` — an extra positional
	// here almost always means run/resume confusion, and the generic "unknown
	// command" message buries that hint.
	cmd.Args = func(_ *cobra.Command, args []string) error {
		if len(args) > 0 {
			return fmt.Errorf("resume takes no work-package argument; to start a fresh lifecycle for branch %q, run `wonka run --branch %s %s`",
				flags.Branch, flags.Branch, args[0])
		}
		return nil
	}
	return cmd
}
