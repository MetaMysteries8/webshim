package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveAlias(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"defaultProject": "demo",
		"projects": {
			"demo":  {"id": "proj_demo",  "slug": "demo-slug"},
			"other": {"id": "proj_other", "slug": "other-slug"}
		}
	}`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	alias, p, err := cfg.Resolve("other")
	if err != nil || alias != "other" || p.ID != "proj_other" {
		t.Errorf("explicit alias: %q %+v %v", alias, p, err)
	}

	// An empty alias falls back to defaultProject.
	alias, p, err = cfg.Resolve("")
	if err != nil || alias != "demo" || p.ID != "proj_demo" {
		t.Errorf("default alias: %q %+v %v", alias, p, err)
	}
}

// TestUnknownAliasListsOptions covers the playbook rule that an unknown alias
// must stop and list what exists rather than guessing an ID.
func TestUnknownAliasListsOptions(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"projects": {
		"demo":  {"id": "p1", "slug": "s1"},
		"other": {"id": "p2", "slug": "s2"}
	}}`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	_, _, err = cfg.Resolve("typo")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"typo", "demo", "other"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// TestSingleProjectNeedsNoDefault is a small convenience: with exactly one
// project configured, there is nothing to disambiguate.
func TestSingleProjectNeedsNoDefault(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"projects": {"only": {"id": "p1", "slug": "s1"}}}`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	alias, p, err := cfg.Resolve("")
	if err != nil || alias != "only" || p.ID != "p1" {
		t.Errorf("got %q %+v %v", alias, p, err)
	}
}

func TestValidationRejectsBrokenConfigs(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"missing id":          `{"projects": {"a": {"slug": "s"}}}`,
		"placeholder id":      `{"projects": {"a": {"id": "REPLACE_WITH_PROJECT_ID"}}}`,
		"default not in list": `{"defaultProject": "ghost", "projects": {"a": {"id": "p"}}}`,
		"bad permission mode": `{"projects": {}, "agent": {"mode": "turbo"}}`,
		"malformed json":      `{"projects":`,
	}
	for name, body := range cases {
		if _, err := LoadFile(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestModeDefaultsToNormal(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"projects": {"a": {"id": "p"}}}`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := cfg.Mode(); got != permission.ModeNormal {
		t.Errorf("Mode() = %q, want %q", got, permission.ModeNormal)
	}

	path = writeConfig(t, `{"projects": {"a": {"id": "p"}}, "agent": {"mode": "yolo"}}`)
	cfg, err = LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := cfg.Mode(); got != permission.ModeYOLO {
		t.Errorf("Mode() = %q, want %q", got, permission.ModeYOLO)
	}
}

// TestExampleConfigParses keeps the shipped template honest: it must stay a
// valid document even though its placeholder IDs are rejected by validation.
func TestExampleConfigParses(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "projects.config.example.json"))
	if err != nil {
		t.Skipf("example config not found: %v", err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = LoadFile(path)
	if err == nil {
		t.Fatal("the example should be rejected because of its placeholder ids")
	}
	// Rejected for the right reason -- a placeholder, not a syntax error.
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("example config failed for the wrong reason: %v", err)
	}
}
