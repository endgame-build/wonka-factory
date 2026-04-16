package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/endgame/wonka-factory/orch"
)

// renderTable writes tasks to w as an aligned text table. Column order is a
// public contract — downstream scripts split by column position.
func renderTable(w io.Writer, tasks []*orch.Task) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()

	fmt.Fprintln(tw, "ID\tSTATUS\tROLE\tASSIGNEE\tTITLE")
	for _, t := range tasks {
		assignee := t.Assignee
		if assignee == "" {
			assignee = "-"
		}
		role := t.Role()
		if role == "" {
			role = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Status, role, assignee, t.Title)
	}
}
