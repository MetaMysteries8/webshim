package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MetaMysteries8/webshim/internal/agent"
	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/llm"
	"github.com/MetaMysteries8/webshim/internal/mirror"
	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/tui"
)

// RunTUI starts the terminal interface.
func RunTUI(args []string) error {
	var flags commonFlags
	var (
		modeName string
		provider string
		model    string
	)
	positional, err := parseArgsWithExtra("webshim", args, &flags, func(fs *flag.FlagSet) {
		fs.StringVar(&modeName, "mode", "", "permission mode: manual, normal, or yolo")
		fs.StringVar(&provider, "provider", "", "model provider id (default: hyper)")
		fs.StringVar(&model, "model", "", "model id")
	})
	if err != nil {
		return err
	}
	alias, _ := splitAliasAndRest(positional, 0)

	a, err := newApp(alias, flags, true)
	if err != nil {
		return err
	}
	defer a.Close()

	mode := a.cfg.Mode()
	if modeName != "" {
		if mode, err = permission.ParseMode(modeName); err != nil {
			return err
		}
	}
	if provider == "" {
		provider = a.cfg.Agent.Provider
	}
	if model == "" {
		model = a.cfg.Agent.Model
	}

	// Ctrl-C during startup should exit cleanly rather than leaving a
	// half-initialized terminal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cat, err := catalog.Load(loadCtx, catalog.Options{Logger: a.log.Logger})
	if err != nil {
		return err
	}
	selection, err := llm.Resolve(cat, provider, model)
	if err != nil {
		return err
	}
	languageModel, err := llm.New(loadCtx, selection)
	if err != nil {
		return err
	}

	workingCopy, err := mirror.Open(config.MirrorDir(a.alias))
	if err != nil {
		return err
	}
	defer workingCopy.Close() //nolint:errcheck // best effort on shutdown

	session := agent.NewSession(agent.SessionConfig{
		Alias:   a.alias,
		Project: a.project,
		Mirror:  workingCopy,
		Client:  a.client,
		Logger:  a.log.Logger,
		Model:   selection.Model,
	})

	// The agent needs an Asker before the program exists, and the program
	// needs the model before it can run. The sender bridges that ordering:
	// it is created first, handed to the agent, and attached to the program
	// once Run starts.
	sender, asker := tui.NewAsker()

	ag, err := agent.New(agent.Config{
		Model:   languageModel,
		Session: session,
		Gate:    agent.NewGate(mode, asker),
		Logger:  a.log.Logger,
	})
	if err != nil {
		return err
	}

	a.log.Info("starting webshim",
		"alias", a.alias, "model", selection.String(), "mode", string(mode))

	err = tui.Run(ctx, sender, tui.Deps{
		Agent:     ag,
		Catalog:   cat,
		Config:    a.cfg,
		Logger:    a.log.Logger,
		Alias:     a.alias,
		ProjectID: a.project.ID,
		LogPath:   a.log.Path,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	fmt.Printf("\n%d step(s), %d tokens, about $%.4f this session\n",
		session.Steps(), session.Usage().TotalTokens, session.Cost())
	return nil
}
