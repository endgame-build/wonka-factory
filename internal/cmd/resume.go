package cmd

import (
	"github.com/endgame/wonka-factory/orch"
	"github.com/spf13/cobra"
)

func newResumeCmd(flags *CLIFlags) *cobra.Command {
	return newLifecycleCmd(
		"resume",
		"Resume an interrupted lifecycle",
		`Re-acquires the per-branch lifecycle lock, reconciles state from the
prior run (stale assignments, orphan tmux sessions, recovered gap/retry/
handoff counters from the event log), then dispatches remaining ready
tasks until completion.`,
		(*orch.Engine).Resume,
		flags,
	)
}
