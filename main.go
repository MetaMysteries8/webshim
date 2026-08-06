// Command webshim is a WebSim agent that runs entirely in the terminal.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/MetaMysteries8/webshim/internal/cli"
)

func main() {
	args := os.Args[1:]

	// With no subcommand, or with only flags, start the interface. Anything
	// that begins with a dash is a flag for the TUI, not a command name.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if err := cli.RunTUI(args); err != nil {
			fmt.Fprintln(os.Stderr, "webshim:", err)
			os.Exit(1)
		}
		return
	}

	os.Exit(cli.Run(args))
}
