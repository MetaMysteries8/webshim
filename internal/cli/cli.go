// Package cli implements webshim's non-TUI subcommands.
//
// These commands exist so the WebSim flows can be exercised, scripted, and
// debugged without a terminal UI or an LLM in the way. Every one of them drives
// the same internal/websim client the agent uses, so if a flow works here it
// works there.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/MetaMysteries8/webshim/internal/auth"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/logging"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// Version is set at build time with -ldflags.
var Version = "dev"

// Run dispatches a subcommand and returns a process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}

	var err error
	switch args[0] {
	case "doctor":
		err = runDoctor(args[1:])
	case "ls":
		err = runLS(args[1:])
	case "publish":
		err = runPublish(args[1:])
	case "rollback":
		err = runRollback(args[1:])
	case "version", "--version", "-v":
		fmt.Println("webshim", Version)
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "webshim: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "webshim:", err)
		return exitCodeFor(err)
	}
	return 0
}

// exitCodeFor maps an error onto a distinct exit code so scripts can react
// without parsing messages.
func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, websim.ErrNoToken):
		return 3
	case errors.Is(err, websim.ErrUnauthorized), errors.Is(err, websim.ErrForbidden):
		return 4
	case errors.Is(err, websim.ErrConcurrentModification):
		return 5
	case errors.Is(err, websim.ErrDryRun):
		return 0 // a dry run stopping where it should is not a failure
	default:
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `webshim - a WebSim agent for your terminal

Usage:
  webshim                          start the TUI
  webshim doctor                   check credentials and connectivity
  webshim ls [alias]               inspect a project
  webshim publish [alias] <path>   publish a file or directory
  webshim rollback [alias] <ver>   make an earlier revision current
  webshim version

Common flags:
  --json        emit the machine-readable operational record
  --dry-run     log intended mutations without sending them
  --verbose     also log to stderr
  --config PATH use a specific projects.config.json

An omitted alias uses defaultProject from projects.config.json.

Exit codes:
  0 ok   1 error   2 usage   3 no token   4 auth/permission   5 concurrent change
`)
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	json       bool
	dryRun     bool
	verbose    bool
	configPath string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&c.json, "json", false, "emit the machine-readable operational record")
	fs.BoolVar(&c.dryRun, "dry-run", false, "log intended mutations without sending them")
	fs.BoolVar(&c.verbose, "verbose", false, "also log to stderr")
	fs.StringVar(&c.configPath, "config", "", "path to projects.config.json")
}

// app is a wired-up set of dependencies for one command run.
type app struct {
	cfg     *config.Config
	log     *logging.Logger
	client  *websim.Client
	alias   string
	project config.Project
	flags   commonFlags
}

// newApp loads config, resolves the project and token, and builds a client.
//
// requireProject is false for commands like doctor that should still work when
// no project is configured.
func newApp(alias string, flags commonFlags, requireProject bool) (*app, error) {
	cfg, err := loadConfig(flags.configPath)
	if err != nil && !errors.Is(err, config.ErrNoConfig) {
		return nil, err
	}

	a := &app{cfg: cfg, flags: flags}

	// Resolving the project first matters: its optional per-project bearer is
	// the highest-priority token source.
	if requireProject || len(cfg.Projects) > 0 {
		resolvedAlias, project, resolveErr := cfg.Resolve(alias)
		if resolveErr != nil {
			if requireProject {
				return nil, resolveErr
			}
		} else {
			a.alias, a.project = resolvedAlias, project
		}
	}

	token, source, err := auth.ResolveWebSim(a.project.Bearer)
	if err != nil {
		if requireProject {
			return nil, err
		}
		// doctor reports a missing token rather than failing on it.
		token, source = "", ""
	}

	level := slog.LevelInfo
	if flags.verbose {
		level = slog.LevelDebug
	}
	logger, err := logging.New(logging.Options{
		Level:   level,
		Console: flags.verbose,
		Secrets: []string{token.Reveal()},
	})
	if err != nil {
		return nil, err
	}
	a.log = logger
	logger.Debug("resolved credentials", "source", string(source), "alias", a.alias)

	client, err := websim.New(websim.Options{
		Token:     token,
		Logger:    logger.Logger,
		UserAgent: "webshim/" + Version,
		DryRun:    flags.dryRun,
	})
	if err != nil {
		return nil, err
	}
	a.client = client
	return a, nil
}

func (a *app) Close() {
	if a.log != nil {
		_ = a.log.Close()
	}
}

// loadConfig honors an explicit --config path, else searches the defaults.
func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		return config.LoadFile(path)
	}
	return config.Load()
}

// parseArgs runs a flag set and returns the positional arguments.
func parseArgs(name string, args []string, flags *commonFlags) ([]string, error) {
	return parseArgsWithExtra(name, args, flags, nil)
}

// parseArgsWithExtra is parseArgs plus command-specific flags.
//
// Go's flag package stops parsing at the first positional argument, so flags
// must precede the alias and path. The usage text says so.
func parseArgsWithExtra(name string, args []string, flags *commonFlags, extra func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags.register(fs)
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return fs.Args(), nil
}

// splitAliasAndRest interprets the leading positional argument as an alias only
// when it is not obviously something else.
func splitAliasAndRest(positional []string, wantExtra int) (alias string, rest []string) {
	if len(positional) > wantExtra {
		return positional[0], positional[1:]
	}
	return "", positional
}

// dryRunNote returns a banner for dry-run output.
func dryRunNote(dryRun bool) string {
	if !dryRun {
		return ""
	}
	return "DRY RUN: no mutations were sent.\n"
}

// indent is a small helper for aligned human-readable output.
func indent(label string, width int) string {
	if len(label) >= width {
		return label + " "
	}
	return label + strings.Repeat(" ", width-len(label))
}
