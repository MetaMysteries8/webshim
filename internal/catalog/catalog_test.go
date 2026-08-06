package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedFallbackIsUsable is the guarantee behind offline first runs: the
// snapshot must parse, and it must contain a working default provider with at
// least one tool-calling model.
func TestEmbeddedFallbackIsUsable(t *testing.T) {
	t.Parallel()

	providers, err := parseModelsDev(fallbackJSON)
	if err != nil {
		t.Fatalf("the embedded snapshot does not parse: %v", err)
	}

	hyper, ok := providers[DefaultProviderID]
	if !ok {
		t.Fatalf("the embedded snapshot has no %q provider", DefaultProviderID)
	}
	if hyper.API != HyperBaseURL {
		t.Errorf("hyper API = %q, want %q", hyper.API, HyperBaseURL)
	}
	if len(hyper.Env) == 0 {
		t.Error("hyper has no env var names, so no key could ever be found")
	}

	cat := &Catalog{Providers: providers}
	models := cat.ToolCallModels(DefaultProviderID)
	if len(models) == 0 {
		t.Fatal("the embedded snapshot offers no tool-calling Hyper models")
	}
	for _, m := range models {
		if !m.ToolCall {
			t.Errorf("ToolCallModels returned %s, which cannot call tools", m.ID)
		}
		if m.ID == "" {
			t.Error("a model has no id")
		}
	}
}

func TestParseModelsDevKeysWinOverIDFields(t *testing.T) {
	t.Parallel()

	providers, err := parseModelsDev([]byte(`{
		"acme": {"name": "Acme", "env": ["ACME_KEY"], "api": "https://acme.test/v1",
		         "models": {"acme-1": {"name": "Acme One", "tool_call": true}}}
	}`))
	if err != nil {
		t.Fatalf("parseModelsDev: %v", err)
	}
	p := providers["acme"]
	if p.ID != "acme" {
		t.Errorf("provider id = %q, want the map key", p.ID)
	}
	m := p.Models["acme-1"]
	if m.ID != "acme-1" || m.ProviderID != "acme" {
		t.Errorf("model identity = %q/%q", m.ProviderID, m.ID)
	}
}

func TestParseModelsDevRejectsEmpty(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{}`, `not json`} {
		if _, err := parseModelsDev([]byte(body)); err == nil {
			t.Errorf("parseModelsDev(%q) should have failed", body)
		}
	}
}

// TestToolCallModelsExcludesNonToolModels is the point of consulting models.dev
// at all: an agent cannot drive a model that cannot call tools, so those are
// never offered.
func TestToolCallModelsExcludesNonToolModels(t *testing.T) {
	t.Parallel()

	cat := &Catalog{Providers: map[string]Provider{
		"acme": {ID: "acme", Models: map[string]Model{
			"good":  {ID: "good", ToolCall: true, ReleaseDate: "2026-01-01"},
			"newer": {ID: "newer", ToolCall: true, ReleaseDate: "2026-06-01"},
			"bad":   {ID: "bad", ToolCall: false, ReleaseDate: "2026-07-01"},
		}},
	}}

	got := cat.ToolCallModels("acme")
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	// Newest first.
	if got[0].ID != "newer" {
		t.Errorf("models are not sorted newest-first: %v", got)
	}
	for _, m := range got {
		if m.ID == "bad" {
			t.Error("a non-tool-calling model was offered")
		}
	}
	if cat.ToolCallModels("nope") != nil {
		t.Error("an unknown provider should yield nothing")
	}
}

func TestSortedProvidersPutsDefaultFirst(t *testing.T) {
	t.Parallel()

	cat := &Catalog{Providers: map[string]Provider{
		"anthropic":       {ID: "anthropic", Name: "Anthropic"},
		DefaultProviderID: {ID: DefaultProviderID, Name: "Charm Hyper"},
		"openai":          {ID: "openai", Name: "OpenAI"},
	}}
	got := cat.SortedProviders()
	if got[0].ID != DefaultProviderID {
		t.Errorf("first provider = %q, want the default %q", got[0].ID, DefaultProviderID)
	}
	if got[1].ID != "anthropic" || got[2].ID != "openai" {
		t.Errorf("the rest should be alphabetical: %v", []string{got[1].ID, got[2].ID})
	}
}

// fakeCatalogServer serves both endpoints.
func fakeCatalogServer(t *testing.T, modelsDev, hyper string, hyperStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("models.dev is a public endpoint; no credentials should be sent")
		}
		_, _ = w.Write([]byte(modelsDev))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Hyper's model list is public; no credentials should be sent")
		}
		if hyperStatus != 0 && hyperStatus != http.StatusOK {
			http.Error(w, "nope", hyperStatus)
			return
		}
		_, _ = w.Write([]byte(hyper))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const sampleModelsDev = `{
	"hyper": {"id":"hyper","name":"Charm Hyper","env":["HYPER_API_KEY"],
	          "api":"https://hyper.charm.land/v1",
	          "models":{"known-model":{"name":"Known","tool_call":true,"structured_output":true,
	                                   "family":"known","release_date":"2026-01-01"}}},
	"anthropic": {"id":"anthropic","name":"Anthropic","env":["ANTHROPIC_API_KEY"],
	              "models":{"claude-x":{"name":"Claude X","tool_call":true}}}
}`

const sampleHyper = `{"object":"list","data":[
	{"id":"known-model","display_name":"Known Model","context_window":1000000,
	 "max_output_tokens":64000,"capabilities":{"vision":false},
	 "pricing":{"input":0.2,"output":0.8,"cache_hit":0.04}},
	{"id":"brand-new","display_name":"Brand New","context_window":500000,
	 "max_output_tokens":32000,"capabilities":{"vision":true},
	 "reasoning":{"effort_levels":[{"value":"high"},{"value":"xhigh"}],"default_effort_level":"high"},
	 "pricing":{"input":2.4,"output":4.8}}
]}`

// TestHyperOverlaysModelsDev covers the merge rules: Hyper wins on its own
// fields, models.dev supplies tool_call, and a model Hyper has but models.dev
// has not indexed yet is assumed to support tools and flagged as such.
func TestHyperOverlaysModelsDev(t *testing.T) {
	t.Parallel()

	srv := fakeCatalogServer(t, sampleModelsDev, sampleHyper, http.StatusOK)
	cat, err := Load(context.Background(), Options{
		CacheDir:       t.TempDir(),
		ModelsDevURL:   srv.URL + "/api.json",
		HyperModelsURL: srv.URL + "/v1/models",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cat.Source != SourceNetwork {
		t.Errorf("source = %q, want %q", cat.Source, SourceNetwork)
	}

	_, known, ok := cat.Model("hyper", "known-model")
	if !ok {
		t.Fatal("known-model is missing")
	}
	// Hyper is authoritative for its own numbers.
	if known.Limit.Context != 1_000_000 || known.Cost.Input != 0.2 {
		t.Errorf("Hyper's data did not win: %+v", known)
	}
	// models.dev supplies what Hyper omits.
	if !known.ToolCall || !known.StructuredOutput || known.Family != "known" {
		t.Errorf("models.dev data was not carried over: %+v", known)
	}
	if known.ToolCallAssumed {
		t.Error("tool_call was known, so it must not be marked as assumed")
	}

	_, fresh, ok := cat.Model("hyper", "brand-new")
	if !ok {
		t.Fatal("brand-new is missing")
	}
	if !fresh.ToolCall || !fresh.ToolCallAssumed {
		t.Errorf("an unindexed Hyper model should assume tool support and say so: %+v", fresh)
	}
	if !fresh.Vision() {
		t.Error("vision capability was dropped")
	}
	if !fresh.Reasoning || len(fresh.ReasoningOptions) != 1 {
		t.Errorf("reasoning effort levels were dropped: %+v", fresh)
	}

	// Other providers survive the merge untouched.
	if _, _, ok := cat.Model("anthropic", "claude-x"); !ok {
		t.Error("merging Hyper dropped another provider")
	}
}

// TestHyperOutageIsNotFatal: a Hyper problem must not stop someone using
// Anthropic.
func TestHyperOutageIsNotFatal(t *testing.T) {
	t.Parallel()

	srv := fakeCatalogServer(t, sampleModelsDev, "", http.StatusInternalServerError)
	cat, err := Load(context.Background(), Options{
		CacheDir:       t.TempDir(),
		ModelsDevURL:   srv.URL + "/api.json",
		HyperModelsURL: srv.URL + "/v1/models",
	})
	if err != nil {
		t.Fatalf("Load should have succeeded: %v", err)
	}
	if len(cat.Warnings) == 0 {
		t.Error("the Hyper failure should be reported as a warning")
	}
	if _, _, ok := cat.Model("anthropic", "claude-x"); !ok {
		t.Error("anthropic should still be usable")
	}
	// The models.dev view of Hyper survives.
	if _, _, ok := cat.Model("hyper", "known-model"); !ok {
		t.Error("hyper should fall back to its models.dev entry")
	}
}

func TestCacheIsUsedThenRefreshed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := fakeCatalogServer(t, sampleModelsDev, sampleHyper, http.StatusOK)
	opts := Options{
		CacheDir:       dir,
		ModelsDevURL:   srv.URL + "/api.json",
		HyperModelsURL: srv.URL + "/v1/models",
	}

	first, err := Load(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if first.Source != SourceNetwork {
		t.Errorf("first load source = %q", first.Source)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheFileName)); err != nil {
		t.Fatalf("no cache was written: %v", err)
	}

	second, err := Load(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if second.Source != SourceCache {
		t.Errorf("second load source = %q, want %q", second.Source, SourceCache)
	}

	// An expired cache goes back to the network.
	opts.TTL = time.Nanosecond
	third, err := Load(context.Background(), opts)
	if err != nil {
		t.Fatalf("third Load: %v", err)
	}
	if third.Source != SourceNetwork {
		t.Errorf("an expired cache should refresh, got %q", third.Source)
	}
}

// TestNetworkFailureFallsBackThroughStaleCacheToEmbedded walks the whole
// degradation ladder.
func TestNetworkFailureFallsBackThroughStaleCacheToEmbedded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A stale cache on disk.
	stale := cacheEnvelope{
		FetchedAt: time.Now().Add(-100 * time.Hour),
		Providers: map[string]Provider{"acme": {ID: "acme", Models: map[string]Model{
			"a": {ID: "a", ToolCall: true},
		}}},
	}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(dir, cacheFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	dead := Options{
		CacheDir:       dir,
		ModelsDevURL:   "http://127.0.0.1:1/api.json", // nothing listens here
		HyperModelsURL: "http://127.0.0.1:1/v1/models",
		HTTPClient:     &http.Client{Timeout: time.Second},
	}
	cat, err := Load(context.Background(), dead)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cat.Source != SourceStale {
		t.Errorf("source = %q, want %q", cat.Source, SourceStale)
	}
	if _, ok := cat.Provider("acme"); !ok {
		t.Error("the stale cache contents were not returned")
	}

	// With no cache at all, the embedded snapshot takes over.
	dead.CacheDir = t.TempDir()
	cat, err = Load(context.Background(), dead)
	if err != nil {
		t.Fatalf("Load with no cache: %v", err)
	}
	if cat.Source != SourceEmbedded {
		t.Errorf("source = %q, want %q", cat.Source, SourceEmbedded)
	}
	if len(cat.ToolCallModels(DefaultProviderID)) == 0 {
		t.Error("the embedded fallback produced no usable models")
	}
}

func TestCorruptCacheIsIgnored(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, cacheFileName), []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeCatalogServer(t, sampleModelsDev, sampleHyper, http.StatusOK)

	cat, err := Load(context.Background(), Options{
		CacheDir:       dir,
		ModelsDevURL:   srv.URL + "/api.json",
		HyperModelsURL: srv.URL + "/v1/models",
	})
	if err != nil {
		t.Fatalf("a corrupt cache should not be fatal: %v", err)
	}
	if cat.Source != SourceNetwork {
		t.Errorf("source = %q, want a fresh fetch", cat.Source)
	}
}

func TestAvailableProvidersFiltersByKey(t *testing.T) {
	t.Parallel()

	cat := &Catalog{Providers: map[string]Provider{
		"hyper":     {ID: "hyper", Env: []string{"HYPER_API_KEY"}},
		"anthropic": {ID: "anthropic", Env: []string{"ANTHROPIC_API_KEY"}},
		"nokey":     {ID: "nokey"},
	}}

	got := cat.AvailableProviders(func(names []string) bool {
		return len(names) > 0 && strings.HasPrefix(names[0], "HYPER")
	})
	if len(got) != 1 || got[0].ID != "hyper" {
		t.Errorf("got %v, want just hyper", got)
	}
}
