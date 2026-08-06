package catalog

import (
	"context"
	"encoding/json"
	"fmt"
)

// HyperBaseURL is Charm Hyper's OpenAI-compatible API root.
const HyperBaseURL = "https://hyper.charm.land/v1"

// hyperModelList is the response from GET /v1/models, which is public and
// needs no authentication.
type hyperModelList struct {
	Data []hyperModel `json:"data"`
}

type hyperModel struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Capabilities    struct {
		Vision bool `json:"vision"`
	} `json:"capabilities"`
	Reasoning *struct {
		EffortLevels []struct {
			Value   string `json:"value"`
			Display string `json:"display"`
		} `json:"effort_levels"`
		DefaultEffortLevel string `json:"default_effort_level"`
	} `json:"reasoning"`
	Pricing struct {
		Input       float64 `json:"input"`
		Output      float64 `json:"output"`
		CacheCreate float64 `json:"cache_create"`
		CacheHit    float64 `json:"cache_hit"`
	} `json:"pricing"`
}

// mergeHyper overlays Hyper's own catalog onto the models.dev data.
//
// Hyper's endpoint is authoritative for Hyper: it is first-party, it is updated
// when models ship, and it carries reasoning effort levels that models.dev does
// not. models.dev still contributes the one field Hyper omits -- whether a
// model supports tool calling.
func mergeHyper(ctx context.Context, opts Options, cat *Catalog) error {
	body, err := getJSON(ctx, opts.HTTPClient, opts.HyperModelsURL)
	if err != nil {
		return err
	}

	var list hyperModelList
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("parsing Hyper's model list: %w", err)
	}
	if len(list.Data) == 0 {
		return fmt.Errorf("Hyper returned no models")
	}

	existing := cat.Providers[DefaultProviderID]
	provider := Provider{
		ID:     DefaultProviderID,
		Name:   orDefault(existing.Name, "Charm Hyper"),
		Env:    existing.Env,
		API:    orDefault(existing.API, HyperBaseURL),
		Doc:    orDefault(existing.Doc, "https://hyper.charm.land/docs"),
		Models: make(map[string]Model, len(list.Data)),
	}
	if len(provider.Env) == 0 {
		provider.Env = []string{"HYPER_API_KEY"}
	}

	for _, hm := range list.Data {
		m := Model{
			ID:    hm.ID,
			Name:  orDefault(hm.DisplayName, hm.ID),
			Limit: Limit{Context: hm.ContextWindow, Output: hm.MaxOutputTokens},
			Cost: Cost{
				Input:      hm.Pricing.Input,
				Output:     hm.Pricing.Output,
				CacheRead:  hm.Pricing.CacheHit,
				CacheWrite: hm.Pricing.CacheCreate,
			},
			ProviderID: DefaultProviderID,
		}
		if hm.Capabilities.Vision {
			m.Modalities.Input = []string{"text", "image"}
		} else {
			m.Modalities.Input = []string{"text"}
		}
		m.Modalities.Output = []string{"text"}

		if hm.Reasoning != nil && len(hm.Reasoning.EffortLevels) > 0 {
			m.Reasoning = true
			opt := ReasoningOption{Type: "effort", Default: hm.Reasoning.DefaultEffortLevel}
			for _, lvl := range hm.Reasoning.EffortLevels {
				opt.Values = append(opt.Values, lvl.Value)
			}
			m.ReasoningOptions = []ReasoningOption{opt}
		}

		// Hyper's endpoint does not report tool-calling support. Take it from
		// models.dev when that catalog knows the model. When it does not --
		// which happens for a model Hyper shipped more recently than
		// models.dev indexed -- assume true and record the assumption, since
		// every model in Hyper's catalog is selected for agentic coding.
		if prior, ok := existing.Models[hm.ID]; ok {
			m.ToolCall = prior.ToolCall
			m.StructuredOutput = prior.StructuredOutput
			m.Temperature = prior.Temperature
			m.Family = prior.Family
			m.ReleaseDate = prior.ReleaseDate
			m.Attachment = prior.Attachment
		} else {
			m.ToolCall = true
			m.ToolCallAssumed = true
		}

		provider.Models[hm.ID] = m
	}

	cat.Providers[DefaultProviderID] = provider
	opts.Logger.Debug("catalog: merged Hyper", "models", len(provider.Models))
	return nil
}

func orDefault(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
