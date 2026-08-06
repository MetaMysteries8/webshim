package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MetaMysteries8/webshim/internal/agent"
	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/llm"
	"github.com/MetaMysteries8/webshim/internal/mirror"
	"github.com/MetaMysteries8/webshim/internal/permission"
)

// runAsk runs one agent turn headlessly and streams the result to stdout.
//
// This is the same agent the TUI drives; only the presentation differs. Having
// it means the agent loop can be exercised, scripted, and debugged without a
// terminal UI in the way.
func runAsk(args []string) error {
	var flags commonFlags
	var (
		modeName string
		provider string
		model    string
		yes      bool
	)
	positional, err := parseArgsWithExtra("ask", args, &flags, func(fs *flag.FlagSet) {
		fs.StringVar(&modeName, "mode", "", "permission mode: manual, normal, or yolo")
		fs.StringVar(&provider, "provider", "", "model provider id (default: hyper)")
		fs.StringVar(&model, "model", "", "model id")
		fs.BoolVar(&yes, "yes", false, "approve every prompt without asking")
	})
	if err != nil {
		return err
	}

	alias, rest := splitAliasAndRest(positional, 1)
	if len(rest) == 0 {
		return errors.New(`usage: webshim ask [alias] "what you want done"`)
	}
	prompt := strings.Join(rest, " ")

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cat, err := catalog.Load(ctx, catalog.Options{Logger: a.log.Logger})
	if err != nil {
		return err
	}
	selection, err := llm.Resolve(cat, provider, model)
	if err != nil {
		return err
	}
	languageModel, err := llm.New(ctx, selection)
	if err != nil {
		return err
	}

	mirrorDir := config.MirrorDir(a.alias)
	workingCopy, err := mirror.Open(mirrorDir)
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

	asker := terminalAsker()
	if yes {
		asker = agent.AutoApprove()
	}
	gate := agent.NewGate(mode, asker)

	ag, err := agent.New(agent.Config{
		Model:   languageModel,
		Session: session,
		Gate:    gate,
		Logger:  a.log.Logger,
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s  mode: %s\n\n", a.alias, selection, mode)

	_, err = ag.Run(ctx, prompt, printEvent)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("\n(cancelled)")
			return nil
		}
		return err
	}

	fmt.Printf("\n%d step(s), %d tokens, about $%.4f\n",
		session.Steps(), session.Usage().TotalTokens, session.Cost())
	return nil
}

// printEvent renders agent events as plain text.
func printEvent(ev agent.Event) {
	switch e := ev.(type) {
	case agent.TextDeltaEvent:
		fmt.Print(e.Text)

	case agent.ToolCallEvent:
		fmt.Printf("\n  -> %s %s\n", e.Tool, oneLine(e.Input, 120))

	case agent.ToolResultEvent:
		status := "ok"
		if e.IsError {
			status = "error"
		}
		fmt.Printf("     %s: %s\n", status, oneLine(e.Output, 200))

	case agent.ErrorEvent:
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", e.Err)

	case agent.TurnEndEvent:
		if e.Cancelled {
			fmt.Println("\n(interrupted)")
			return
		}
		fmt.Println()
	}
}

// terminalAsker prompts on stdin. It is used when a run needs approval and a
// person is present.
func terminalAsker() agent.Asker {
	reader := bufio.NewReader(os.Stdin)
	return agent.AskerFunc(func(ctx context.Context, req agent.Request) (agent.Decision, error) {
		fmt.Printf("\n  [%s] %s\n", req.Risk, req.Summary)
		if req.Detail != "" {
			for line := range strings.SplitSeq(strings.TrimRight(req.Detail, "\n"), "\n") {
				fmt.Printf("      %s\n", line)
			}
		}
		fmt.Print("  approve? [y/N] ")

		// Reading stdin cannot be cancelled mid-call, so check the context
		// before blocking rather than leaving a dead prompt on screen.
		if err := ctx.Err(); err != nil {
			return agent.Decision{}, err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return agent.Decision{Approved: false, Reason: "could not read a reply from the terminal"}, nil
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer == "y" || answer == "yes" {
			return agent.Decision{Approved: true}, nil
		}
		return agent.Decision{Approved: false, Reason: "the operator answered no"}, nil
	})
}

// oneLine collapses whitespace and truncates, for compact tool logging.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
