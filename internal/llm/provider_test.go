package llm

import (
	"strings"
	"testing"

	"github.com/MetaMysteries8/webshim/internal/catalog"
)

func testCatalog() *catalog.Catalog {
	return &catalog.Catalog{Providers: map[string]catalog.Provider{
		catalog.DefaultProviderID: {
			ID:   catalog.DefaultProviderID,
			Name: "Charm Hyper",
			Env:  []string{"WEBSHIM_TEST_HYPER_KEY"},
			API:  catalog.HyperBaseURL,
			Doc:  "https://hyper.charm.land",
			Models: map[string]catalog.Model{
				"new-model": {ID: "new-model", ToolCall: true, ReleaseDate: "2026-07-01"},
				"old-model": {ID: "old-model", ToolCall: true, ReleaseDate: "2025-01-01"},
				"no-tools":  {ID: "no-tools", ToolCall: false, ReleaseDate: "2026-08-01"},
			},
		},
		"anthropic": {
			ID: "anthropic", Name: "Anthropic", Env: []string{"WEBSHIM_TEST_ANTHROPIC_KEY"},
			Models: map[string]catalog.Model{"claude-x": {ID: "claude-x", ToolCall: true}},
		},
		"nokeyprovider": {
			ID: "nokeyprovider", Name: "No Key", Env: []string{"WEBSHIM_TEST_ABSENT_KEY"},
			Models: map[string]catalog.Model{"m": {ID: "m", ToolCall: true}},
		},
		"toolless": {
			ID: "toolless", Name: "Toolless", Env: []string{"WEBSHIM_TEST_HYPER_KEY"},
			Models: map[string]catalog.Model{"m": {ID: "m", ToolCall: false}},
		},
	}}
}

// TestResolveDefaultsToNewestHyperModel keeps first-run setup to one
// environment variable.
func TestResolveDefaultsToNewestHyperModel(t *testing.T) {
	t.Setenv("WEBSHIM_TEST_HYPER_KEY", "sk-hyper-testvalue123456")

	sel, err := Resolve(testCatalog(), "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Provider.ID != catalog.DefaultProviderID {
		t.Errorf("provider = %q, want the default", sel.Provider.ID)
	}
	if sel.Model.ID != "new-model" {
		t.Errorf("model = %q, want the newest tool-calling model", sel.Model.ID)
	}
	if sel.String() != "hyper/new-model" {
		t.Errorf("String() = %q", sel.String())
	}
	if sel.Key.Reveal() != "sk-hyper-testvalue123456" {
		t.Error("the key was not resolved")
	}
}

func TestResolveExplicitModel(t *testing.T) {
	t.Setenv("WEBSHIM_TEST_HYPER_KEY", "k-abcdefghijkl")

	sel, err := Resolve(testCatalog(), catalog.DefaultProviderID, "old-model")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Model.ID != "old-model" {
		t.Errorf("model = %q", sel.Model.ID)
	}
}

// TestResolveDistinguishesMissingFromToollessModels: the two failures have
// different fixes, so they must not share a message.
func TestResolveDistinguishesMissingFromToollessModels(t *testing.T) {
	t.Setenv("WEBSHIM_TEST_HYPER_KEY", "k-abcdefghijkl")
	cat := testCatalog()

	_, err := Resolve(cat, catalog.DefaultProviderID, "no-tools")
	if err == nil || !strings.Contains(err.Error(), "cannot call tools") {
		t.Errorf("a tool-less model should say so: %v", err)
	}

	_, err = Resolve(cat, catalog.DefaultProviderID, "does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "no model") {
		t.Errorf("an absent model should say so: %v", err)
	}
	// Either way, suggest something that would work.
	if !strings.Contains(err.Error(), "new-model") {
		t.Errorf("the error should suggest a usable model: %v", err)
	}
}

func TestResolveReportsMissingKeyWithTheEnvName(t *testing.T) {
	t.Setenv("WEBSHIM_TEST_ABSENT_KEY", "")

	_, err := Resolve(testCatalog(), "nokeyprovider", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "WEBSHIM_TEST_ABSENT_KEY") {
		t.Errorf("the error should name the variable to set: %v", err)
	}
}

func TestResolveRejectsProviderWithNoToolCallingModels(t *testing.T) {
	t.Setenv("WEBSHIM_TEST_HYPER_KEY", "k-abcdefghijkl")

	_, err := Resolve(testCatalog(), "toolless", "")
	if err == nil || !strings.Contains(err.Error(), "no tool-calling models") {
		t.Errorf("want a clear refusal, got %v", err)
	}
}

func TestResolveUnknownProviderListsKnownOnes(t *testing.T) {
	t.Parallel()

	_, err := Resolve(testCatalog(), "notaprovider", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"notaprovider", "hyper", "anthropic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// TestSupportedCoversCatalogDrivenProviders documents the design: a provider
// with a base URL works through openaicompat without code changes, and one
// without a base URL and no dedicated package cannot be constructed.
func TestSupportedCoversCatalogDrivenProviders(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		p    catalog.Provider
		want bool
	}{
		"dedicated package":   {catalog.Provider{ID: "anthropic"}, true},
		"the default":         {catalog.Provider{ID: catalog.DefaultProviderID}, true},
		"openai-compatible":   {catalog.Provider{ID: "somegateway", API: "https://x.test/v1"}, true},
		"no package, no base": {catalog.Provider{ID: "mystery"}, false},
	}
	for name, tc := range cases {
		if got := Supported(tc.p); got != tc.want {
			t.Errorf("%s: Supported = %v, want %v", name, got, tc.want)
		}
	}
}

// TestKeyIsNeverInAnErrorMessage: error strings get logged and pasted into
// issues.
func TestKeyIsNeverInAnErrorMessage(t *testing.T) {
	const key = "sk-hyper-donotleakthisvalue"
	t.Setenv("WEBSHIM_TEST_HYPER_KEY", key)

	_, err := Resolve(testCatalog(), catalog.DefaultProviderID, "does-not-exist")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the key leaked into an error: %v", err)
	}
}
