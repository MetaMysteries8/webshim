// Package catalog discovers LLM providers and models.
//
// Two sources are merged:
//
//	models.dev   a community catalog of ~180 providers, giving base URLs,
//	             environment variable names, and per-model capability flags
//	Charm Hyper  the default provider's own /v1/models endpoint, which is
//	             authoritative and fresher for Hyper's own models
//
// Both are public and need no authentication. Results are cached on disk, and a
// trimmed snapshot is embedded so a first run with no network still offers a
// usable model list.
package catalog

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MetaMysteries8/webshim/internal/config"
)

// DefaultProviderID is the provider webshim uses unless told otherwise.
const DefaultProviderID = "hyper"

// fallbackJSON is a trimmed models.dev snapshot: Hyper in full, plus the most
// recent tool-calling models from the major providers. It exists so that a cold
// offline start still has something to offer, not to be comprehensive.
//
// Refresh it by re-running the generator documented in doc.go.
//
//go:embed fallback.json
var fallbackJSON []byte

// Modalities lists what a model accepts and produces.
type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// Limit holds context and output token limits.
type Limit struct {
	Context int `json:"context,omitempty"`
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
}

// Cost is per million tokens, in US dollars.
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// ReasoningOption describes one way of configuring a model's reasoning.
type ReasoningOption struct {
	Type    string   `json:"type,omitempty"`
	Min     int      `json:"min,omitempty"`
	Values  []string `json:"values,omitempty"`
	Default string   `json:"default,omitempty"`
}

// Model is one language model.
type Model struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	Family           string            `json:"family,omitempty"`
	ToolCall         bool              `json:"tool_call"`
	Reasoning        bool              `json:"reasoning,omitempty"`
	StructuredOutput bool              `json:"structured_output,omitempty"`
	Attachment       bool              `json:"attachment,omitempty"`
	Temperature      bool              `json:"temperature,omitempty"`
	Modalities       Modalities        `json:"modalities,omitempty"`
	Limit            Limit             `json:"limit,omitempty"`
	Cost             Cost              `json:"cost,omitempty"`
	ReleaseDate      string            `json:"release_date,omitempty"`
	ReasoningOptions []ReasoningOption `json:"reasoning_options,omitempty"`

	// ProviderID is filled in when the catalog is indexed.
	ProviderID string `json:"-"`

	// ToolCallAssumed marks a model whose tool-calling support was inferred
	// rather than read from the catalog. Only Hyper models can be in this
	// state, and only when models.dev has not listed them yet.
	ToolCallAssumed bool `json:"-"`
}

// DisplayName is the friendliest available label.
func (m Model) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// Vision reports whether the model accepts images.
func (m Model) Vision() bool {
	for _, in := range m.Modalities.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// Provider is one LLM vendor or gateway.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name,omitempty"`
	Env    []string         `json:"env,omitempty"`
	API    string           `json:"api,omitempty"`
	Doc    string           `json:"doc,omitempty"`
	Models map[string]Model `json:"models,omitempty"`
}

// DisplayName is the friendliest available label.
func (p Provider) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// Source records where a catalog came from, for display in doctor and the UI.
type Source string

const (
	SourceNetwork  Source = "network"
	SourceCache    Source = "cache"
	SourceStale    Source = "stale cache (network unavailable)"
	SourceEmbedded Source = "embedded snapshot (network unavailable)"
)

// Catalog is the merged provider set.
type Catalog struct {
	Providers map[string]Provider

	// Source says where the data came from.
	Source Source

	// FetchedAt is when the underlying data was retrieved.
	FetchedAt time.Time

	// Warnings records non-fatal problems, such as Hyper being unreachable
	// while models.dev succeeded.
	Warnings []string
}

// Options configures Load.
type Options struct {
	HTTPClient *http.Client
	Logger     *slog.Logger

	// TTL is how long a cached catalog stays fresh. Defaults to 24 hours.
	TTL time.Duration

	// CacheDir overrides the default cache location. Mostly for tests.
	CacheDir string

	// Offline skips the network entirely.
	Offline bool

	// ModelsDevURL and HyperModelsURL are overridable for tests.
	ModelsDevURL   string
	HyperModelsURL string
}

const (
	defaultModelsDevURL   = "https://models.dev/api.json"
	defaultHyperModelsURL = "https://hyper.charm.land/v1/models"
	cacheFileName         = "catalog.json"
	defaultTTL            = 24 * time.Hour
)

// cacheEnvelope is what gets written to disk.
type cacheEnvelope struct {
	FetchedAt time.Time           `json:"fetched_at"`
	Providers map[string]Provider `json:"providers"`
}

func (o *Options) applyDefaults() error {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.TTL == 0 {
		o.TTL = defaultTTL
	}
	if o.ModelsDevURL == "" {
		o.ModelsDevURL = defaultModelsDevURL
	}
	if o.HyperModelsURL == "" {
		o.HyperModelsURL = defaultHyperModelsURL
	}
	if o.CacheDir == "" {
		dir, err := config.StateDir()
		if err != nil {
			return err
		}
		o.CacheDir = dir
	}
	return nil
}

// Load returns a catalog, preferring a fresh cache, then the network, then a
// stale cache, then the embedded snapshot.
//
// It only fails if every source fails, which in practice means the embedded
// snapshot could not be parsed.
func Load(ctx context.Context, opts Options) (*Catalog, error) {
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}
	cachePath := filepath.Join(opts.CacheDir, cacheFileName)

	if cached, err := readCache(cachePath); err == nil {
		if !opts.Offline && time.Since(cached.FetchedAt) < opts.TTL {
			opts.Logger.Debug("catalog: using fresh cache", "age", time.Since(cached.FetchedAt))
			return &Catalog{Providers: cached.Providers, Source: SourceCache, FetchedAt: cached.FetchedAt}, nil
		}
		if opts.Offline {
			return &Catalog{Providers: cached.Providers, Source: SourceCache, FetchedAt: cached.FetchedAt}, nil
		}
	}

	if !opts.Offline {
		cat, err := fetchAll(ctx, opts)
		if err == nil {
			if writeErr := writeCache(cachePath, cat); writeErr != nil {
				opts.Logger.Debug("catalog: could not write cache", "error", writeErr)
			}
			return cat, nil
		}
		opts.Logger.Warn("catalog: network fetch failed; falling back", "error", err)
	}

	// Stale cache beats an embedded snapshot: it is at least real data this
	// machine once saw.
	if cached, err := readCache(cachePath); err == nil {
		return &Catalog{
			Providers: cached.Providers,
			Source:    SourceStale,
			FetchedAt: cached.FetchedAt,
		}, nil
	}

	providers, err := parseModelsDev(fallbackJSON)
	if err != nil {
		return nil, fmt.Errorf("catalog: the embedded snapshot is unusable: %w", err)
	}
	return &Catalog{Providers: providers, Source: SourceEmbedded}, nil
}

// fetchAll pulls models.dev and Hyper, merging the results.
//
// models.dev is required; Hyper is best-effort, because a Hyper outage should
// not stop someone from using Anthropic.
func fetchAll(ctx context.Context, opts Options) (*Catalog, error) {
	providers, err := fetchModelsDev(ctx, opts)
	if err != nil {
		return nil, err
	}

	cat := &Catalog{
		Providers: providers,
		Source:    SourceNetwork,
		FetchedAt: time.Now(),
	}

	if err := mergeHyper(ctx, opts, cat); err != nil {
		cat.Warnings = append(cat.Warnings,
			fmt.Sprintf("Charm Hyper's model list was unreachable (%v); using models.dev data for Hyper", err))
		opts.Logger.Debug("catalog: hyper merge failed", "error", err)
	}
	return cat, nil
}

func readCache(path string) (*cacheEnvelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env cacheEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if len(env.Providers) == 0 {
		return nil, fmt.Errorf("catalog cache is empty")
	}
	return &env, nil
}

func writeCache(path string, cat *Catalog) error {
	data, err := json.Marshal(cacheEnvelope{FetchedAt: cat.FetchedAt, Providers: cat.Providers})
	if err != nil {
		return err
	}
	// Write to a temporary file and rename, so a crash mid-write cannot
	// leave a truncated cache behind.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// Provider looks up a provider by ID.
func (c *Catalog) Provider(id string) (Provider, bool) {
	p, ok := c.Providers[id]
	return p, ok
}

// Model looks up one model.
func (c *Catalog) Model(providerID, modelID string) (Provider, Model, bool) {
	p, ok := c.Providers[providerID]
	if !ok {
		return Provider{}, Model{}, false
	}
	m, ok := p.Models[modelID]
	if !ok {
		return p, Model{}, false
	}
	m.ProviderID = providerID
	return p, m, true
}

// SortedProviders returns providers ordered with the default first, then
// alphabetically.
func (c *Catalog) SortedProviders() []Provider {
	out := make([]Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == DefaultProviderID) != (out[j].ID == DefaultProviderID) {
			return out[i].ID == DefaultProviderID
		}
		return strings.ToLower(out[i].DisplayName()) < strings.ToLower(out[j].DisplayName())
	})
	return out
}

// ToolCallModels returns a provider's models that can call tools, newest first.
//
// Models without tool calling are excluded rather than shown and rejected
// later: an agent cannot function without them, so offering one would be a
// trap.
func (c *Catalog) ToolCallModels(providerID string) []Model {
	p, ok := c.Providers[providerID]
	if !ok {
		return nil
	}
	out := make([]Model, 0, len(p.Models))
	for id, m := range p.Models {
		if !m.ToolCall {
			continue
		}
		m.ID = id
		m.ProviderID = providerID
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReleaseDate != out[j].ReleaseDate {
			return out[i].ReleaseDate > out[j].ReleaseDate
		}
		return out[i].DisplayName() < out[j].DisplayName()
	})
	return out
}

// AvailableProviders returns providers for which an API key is present,
// according to the env names in the catalog.
func (c *Catalog) AvailableProviders(hasKey func(envNames []string) bool) []Provider {
	var out []Provider
	for _, p := range c.SortedProviders() {
		if len(p.Env) > 0 && hasKey(p.Env) {
			out = append(out, p)
		}
	}
	return out
}
