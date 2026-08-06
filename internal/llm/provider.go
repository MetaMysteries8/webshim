// Package llm turns a catalog entry plus an API key into a Fantasy language
// model.
//
// The mapping is driven by the catalog rather than hardcoded: any provider
// models.dev knows about that exposes an OpenAI-compatible base URL works
// without changes here. Providers with a dedicated Fantasy package get one,
// because those packages carry provider-specific behavior (Anthropic's cache
// control and thinking blocks, Google's safety settings) that the generic
// OpenAI-compatible path cannot express.
package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"

	"github.com/MetaMysteries8/webshim/internal/auth"
	"github.com/MetaMysteries8/webshim/internal/catalog"
)

// Selection is a resolved provider, model, and credential.
type Selection struct {
	Provider catalog.Provider
	Model    catalog.Model
	Key      auth.ProviderKey

	// KeySource names the environment variable the key came from.
	KeySource auth.Source
}

// String is a short label like "hyper/kimi-k2.7-code".
func (s Selection) String() string {
	return s.Provider.ID + "/" + s.Model.ID
}

// Resolve picks a provider and model and finds a key for it.
//
// An empty providerID uses the catalog default (Charm Hyper). An empty modelID
// picks that provider's newest tool-calling model, which keeps first-run setup
// to a single environment variable.
func Resolve(cat *catalog.Catalog, providerID, modelID string) (Selection, error) {
	if providerID == "" {
		providerID = catalog.DefaultProviderID
	}

	provider, ok := cat.Provider(providerID)
	if !ok {
		return Selection{}, fmt.Errorf("unknown provider %q; known providers: %s",
			providerID, strings.Join(providerIDs(cat), ", "))
	}

	models := cat.ToolCallModels(providerID)
	if len(models) == 0 {
		return Selection{}, fmt.Errorf(
			"provider %q offers no tool-calling models, so it cannot run an agent",
			provider.DisplayName())
	}

	var model catalog.Model
	if modelID == "" {
		model = models[0]
	} else {
		found := false
		for _, m := range models {
			if m.ID == modelID {
				model, found = m, true
				break
			}
		}
		if !found {
			// Distinguish "no such model" from "that model cannot call
			// tools", because the fix is different.
			if _, raw, exists := cat.Model(providerID, modelID); exists && !raw.ToolCall {
				return Selection{}, fmt.Errorf(
					"model %q cannot call tools, so it cannot run an agent; try one of: %s",
					modelID, sampleModelIDs(models))
			}
			return Selection{}, fmt.Errorf("provider %q has no model %q; try one of: %s",
				providerID, modelID, sampleModelIDs(models))
		}
	}

	envNames := provider.Env
	if len(envNames) == 0 {
		return Selection{}, fmt.Errorf(
			"the catalog lists no environment variable for provider %q, so no key can be found",
			providerID)
	}
	key, source, ok := auth.ResolveProviderKey(envNames)
	if !ok {
		hint := ""
		if provider.Doc != "" {
			hint = " Get one at " + provider.Doc + "."
		}
		return Selection{}, fmt.Errorf("no API key for %s: set %s.%s",
			provider.DisplayName(), strings.Join(envNames, " or "), hint)
	}

	return Selection{Provider: provider, Model: model, Key: key, KeySource: source}, nil
}

// New builds a Fantasy language model for a selection.
func New(ctx context.Context, sel Selection) (fantasy.LanguageModel, error) {
	provider, err := newProvider(sel)
	if err != nil {
		return nil, err
	}
	model, err := provider.LanguageModel(ctx, sel.Model.ID)
	if err != nil {
		return nil, fmt.Errorf("building model %s: %w", sel, err)
	}
	return model, nil
}

// newProvider maps a catalog provider ID onto a Fantasy provider.
//
// The default branch is what makes the catalog worth consulting: any
// OpenAI-compatible provider models.dev knows the base URL for works with no
// code change here.
func newProvider(sel Selection) (fantasy.Provider, error) {
	key := sel.Key.Reveal()

	switch sel.Provider.ID {
	case "anthropic":
		return anthropic.New(anthropic.WithAPIKey(key))

	case "openai":
		return openai.New(openai.WithAPIKey(key))

	case "google":
		// Google's Fantasy provider distinguishes the Gemini API from Vertex;
		// an API key always means the former.
		return google.New(google.WithGeminiAPIKey(key))

	case "openrouter":
		return openrouter.New(openrouter.WithAPIKey(key))

	case catalog.DefaultProviderID:
		// Charm Hyper speaks OpenAI. Its own docs recommend exactly this.
		baseURL := sel.Provider.API
		if baseURL == "" {
			baseURL = catalog.HyperBaseURL
		}
		return openaicompat.New(
			openaicompat.WithName("Charm Hyper"),
			openaicompat.WithBaseURL(baseURL),
			openaicompat.WithAPIKey(key),
		)

	default:
		if sel.Provider.API == "" {
			return nil, fmt.Errorf(
				"provider %q has no API base URL in the catalog and no built-in support; "+
					"webshim cannot construct a client for it",
				sel.Provider.ID)
		}
		return openaicompat.New(
			openaicompat.WithName(sel.Provider.DisplayName()),
			openaicompat.WithBaseURL(sel.Provider.API),
			openaicompat.WithAPIKey(key),
		)
	}
}

// Supported reports whether webshim can build a client for a provider, so the
// model picker can avoid offering one it would fail on.
func Supported(p catalog.Provider) bool {
	switch p.ID {
	case "anthropic", "openai", "google", "openrouter", catalog.DefaultProviderID:
		return true
	}
	return p.API != ""
}

func providerIDs(cat *catalog.Catalog) []string {
	var out []string
	for _, p := range cat.SortedProviders() {
		if Supported(p) {
			out = append(out, p.ID)
		}
	}
	sort.Strings(out)
	return out
}

// sampleModelIDs lists a few model IDs for an error message.
func sampleModelIDs(models []catalog.Model) string {
	const max = 5
	ids := make([]string, 0, max)
	for i, m := range models {
		if i == max {
			ids = append(ids, "...")
			break
		}
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}
