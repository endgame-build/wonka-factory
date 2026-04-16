// Command wonka runs the BVV DAG-driven task dispatcher. See `wonka --help`.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/endgame/wonka-factory/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// *exitError means die() already printed the user-facing message;
		// any other error is a cobra flag-parse error that SilenceErrors
		// swallowed — print it ourselves or the process exits silently.
		var ex interface{ ExitCode() int }
		if errors.As(err, &ex) {
			os.Exit(ex.ExitCode())
		}
		// Cobra parse failures lose the help-pointer SilenceUsage suppressed.
		// Re-add it so a typo'd flag tells the user where to look.
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "run 'wonka --help' for usage")
		os.Exit(1)
	}
}
