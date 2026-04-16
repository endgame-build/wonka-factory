package cmd

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"text/tabwriter"

	"github.com/endgame/wonka-factory/orch"
)

// renderTable writes tasks to w as an aligned text table. Column order is a
// public contract — downstream scripts split by column position.
//
// Flush and Fprint errors are surfaced: a silent swallow would turn
// `wonka status | head` + disk-full / EIO into a truncated table with exit
// 0. EPIPE is treated as a clean close (head closed the pipe early) and
// returns nil; other errors propagate for the caller to map to exit 1.
func renderTable(w io.Writer, tasks []*orch.Task) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tROLE\tASSIGNEE\tTITLE"); err != nil {
		return wrapPipeErr(err)
	}
	for _, t := range tasks {
		assignee := t.Assignee
		if assignee == "" {
			assignee = "-"
		}
		role := t.Role()
		if role == "" {
			role = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Status, role, assignee, t.Title); err != nil {
			return wrapPipeErr(err)
		}
	}
	return wrapPipeErr(tw.Flush())
}

// wrapPipeErr collapses EPIPE to nil (reader closed the pipe — not our
// failure) and returns everything else unchanged.
func wrapPipeErr(err error) error {
	if errors.Is(err, syscall.EPIPE) {
		return nil
	}
	return err
}
