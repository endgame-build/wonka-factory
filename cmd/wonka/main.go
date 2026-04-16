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
		// A non-nil error implementing ExitCode() means the subcommand
		// already printed a user-facing message; preserve its code and
		// exit silently. Anything else is a cobra parse error that
		// SilenceErrors swallowed — print it ourselves, with a help
		// pointer that SilenceUsage suppressed.
		var ex interface{ ExitCode() int }
		if errors.As(err, &ex) {
			os.Exit(ex.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "run 'wonka --help' for usage")
		os.Exit(1)
	}
}
