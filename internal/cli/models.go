package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/MetaMysteries8/webshim/internal/auth"
	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/llm"
)

// runModels lists the providers and tool-calling models webshim can use.
func runModels(args []string) error {
	var flags commonFlags
	var (
		offline  bool
		provider string
		all      bool
	)
	positional, err := parseArgsWithExtra("models", args, &flags, func(fs *flag.FlagSet) {
		fs.BoolVar(&offline, "offline", false, "skip the network and use the cache or embedded snapshot")
		fs.StringVar(&provider, "provider", "", "show only this provider")
		fs.BoolVar(&all, "all", false, "include providers with no API key configured")
	})
	if err != nil {
		return err
	}
	if provider == "" && len(positional) == 1 {
		provider = positional[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cat, err := catalog.Load(ctx, catalog.Options{Offline: offline})
	if err != nil {
		return err
	}

	hasKey := func(envNames []string) bool {
		_, _, ok := auth.ResolveProviderKey(envNames)
		return ok
	}

	if flags.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cat)
	}

	fmt.Printf("catalog source: %s", cat.Source)
	if !cat.FetchedAt.IsZero() {
		fmt.Printf(" (fetched %s ago)", time.Since(cat.FetchedAt).Round(time.Minute))
	}
	fmt.Println()
	for _, w := range cat.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	fmt.Println()

	shown := 0
	for _, p := range cat.SortedProviders() {
		if provider != "" && p.ID != provider {
			continue
		}
		if !llm.Supported(p) {
			continue
		}
		keyed := hasKey(p.Env)
		if !all && provider == "" && !keyed {
			continue
		}

		models := cat.ToolCallModels(p.ID)
		if len(models) == 0 {
			continue
		}
		shown++

		marker := " "
		if p.ID == catalog.DefaultProviderID {
			marker = "*"
		}
		status := "no key"
		if keyed {
			status = "key found"
		}
		fmt.Printf("%s %s (%s)  %d tool-calling model(s), %s\n",
			marker, p.DisplayName(), p.ID, len(models), status)

		limit := len(models)
		if !all && limit > 8 {
			limit = 8
		}
		for _, m := range models[:limit] {
			fmt.Printf("      %s%s\n", indent(m.ID, 34), modelSummary(m))
		}
		if limit < len(models) {
			fmt.Printf("      ... and %d more (use --all)\n", len(models)-limit)
		}
		fmt.Println()
	}

	if shown == 0 {
		if provider != "" {
			return fmt.Errorf("no usable models found for provider %q", provider)
		}
		fmt.Println("No provider has an API key configured.")
		fmt.Println("Charm Hyper is the default: get a key at https://hyper.charm.land")
		fmt.Println("and export HYPER_API_KEY, then run this again.")
		fmt.Println("Use --all to see every provider regardless of keys.")
	} else if provider == "" && !all {
		fmt.Println("Showing providers with a key configured. Use --all to see the rest.")
	}
	return nil
}

// modelSummary is a compact capability and price line.
func modelSummary(m catalog.Model) string {
	out := ""
	if m.Limit.Context > 0 {
		out += fmt.Sprintf("%s ctx", humanTokens(m.Limit.Context))
	}
	if m.Cost.Input > 0 || m.Cost.Output > 0 {
		out += fmt.Sprintf("  $%.2f/$%.2f per Mtok", m.Cost.Input, m.Cost.Output)
	}
	var tags []string
	if m.Reasoning {
		tags = append(tags, "reasoning")
	}
	if m.Vision() {
		tags = append(tags, "vision")
	}
	if m.ToolCallAssumed {
		tags = append(tags, "tool support assumed")
	}
	for _, t := range tags {
		out += "  [" + t + "]"
	}
	return out
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
