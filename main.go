// Command webshim is a WebSim agent that runs entirely in the terminal.
package main

import (
	"fmt"
	"os"

	"github.com/MetaMysteries8/webshim/internal/cli"
)

func main() {
	args := os.Args[1:]

	// With no arguments the TUI is the intended entrypoint. Until it lands,
	// point the operator at what does work rather than failing silently.
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "webshim: the TUI is not wired up yet.")
		fmt.Fprintln(os.Stderr, "Try `webshim doctor` to check your setup, or `webshim help`.")
		os.Exit(2)
	}

	os.Exit(cli.Run(args))
}
