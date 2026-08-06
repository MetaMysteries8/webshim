package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

// clearEnv unsets every variable the resolver consults, so a developer's real
// environment cannot influence the result.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"WEBSIM_BEARER", "bearer", "WEBSIM_TOKEN", "WEBSIM_CLI_CONFIG"} {
		t.Setenv(k, "")
	}
	// Point HOME at an empty directory so ~/.websim-cli.json cannot be found.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir on Windows
}

func TestResolveWebSimPrecedence(t *testing.T) {
	clearEnv(t)

	cliPath := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(cliPath, []byte(`{"authToken":"from-cli-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Set every source at once, then remove them from the top down. Each
	// step must fall through to exactly the next source in the playbook's
	// order.
	t.Setenv("WEBSIM_BEARER", "from-websim-bearer")
	t.Setenv("bearer", "from-bearer")
	t.Setenv("WEBSIM_TOKEN", "from-websim-token")
	t.Setenv("WEBSIM_CLI_CONFIG", cliPath)

	steps := []struct {
		projectBearer string
		wantToken     string
		wantSource    Source
		clear         string
	}{
		{"from-config", "from-config", SourceProjectConfig, ""},
		{"", "from-websim-bearer", SourceEnvWebSim, "WEBSIM_BEARER"},
		{"", "from-bearer", SourceEnvBearer, "bearer"},
		{"", "from-websim-token", SourceEnvToken, "WEBSIM_TOKEN"},
		{"", "from-cli-file", SourceCLIConfigEnv, ""},
	}
	for i, step := range steps {
		tok, src, err := ResolveWebSim(step.projectBearer)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if tok.Reveal() != step.wantToken {
			t.Errorf("step %d: token from %s, want %s", i, src, step.wantSource)
		}
		if src != step.wantSource {
			t.Errorf("step %d: source = %q, want %q", i, src, step.wantSource)
		}
		if step.clear != "" {
			t.Setenv(step.clear, "")
		}
	}
}

func TestResolveWebSimFallsBackToHomeCLIConfig(t *testing.T) {
	clearEnv(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, ".websim-cli.json"),
		[]byte(`{"authToken":"from-home"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tok, src, err := ResolveWebSim("")
	if err != nil {
		t.Fatalf("ResolveWebSim: %v", err)
	}
	if tok.Reveal() != "from-home" {
		t.Error("did not read the token from ~/.websim-cli.json")
	}
	if src != SourceCLIConfigHome {
		t.Errorf("source = %q, want %q", src, SourceCLIConfigHome)
	}
}

func TestResolveWebSimWithNoTokenPointsAtCLILogin(t *testing.T) {
	clearEnv(t)

	_, _, err := ResolveWebSim("")
	if !errors.Is(err, websim.ErrNoToken) {
		t.Fatalf("want ErrNoToken, got %v", err)
	}
	if !strings.Contains(err.Error(), "websim-cli login") {
		t.Errorf("the error should tell the operator what to run: %v", err)
	}
}

func TestMalformedCLIConfigIsReported(t *testing.T) {
	clearEnv(t)

	bad := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBSIM_CLI_CONFIG", bad)

	// A corrupt config should say so rather than silently falling through to
	// "no token found", which would send the operator chasing the wrong
	// problem.
	if _, _, err := ResolveWebSim(""); err == nil || !strings.Contains(err.Error(), "WEBSIM_CLI_CONFIG") {
		t.Errorf("want an error naming WEBSIM_CLI_CONFIG, got %v", err)
	}
}

func TestBlankValuesAreSkipped(t *testing.T) {
	clearEnv(t)

	// Whitespace-only values are common when an export goes wrong. They must
	// not be mistaken for a real token.
	t.Setenv("WEBSIM_BEARER", "   ")
	t.Setenv("WEBSIM_TOKEN", "real-token")

	tok, src, err := ResolveWebSim("  ")
	if err != nil {
		t.Fatalf("ResolveWebSim: %v", err)
	}
	if tok.Reveal() != "real-token" || src != SourceEnvToken {
		t.Errorf("got %s from %q, want the WEBSIM_TOKEN value", tok, src)
	}
}

func TestProviderKeyRedactsItself(t *testing.T) {
	t.Parallel()

	k := ProviderKey("sk-hyper-abcdef123456")
	if got := fmt.Sprintf("%v %s %#v", k, k, k); strings.Contains(got, "abcdef") {
		t.Errorf("formatting leaked the key: %s", got)
	}
	if b, _ := k.MarshalJSON(); strings.Contains(string(b), "abcdef") {
		t.Errorf("MarshalJSON leaked the key: %s", b)
	}
	if k.Reveal() != "sk-hyper-abcdef123456" {
		t.Error("Reveal must return the real value")
	}
}

func TestResolveProviderKeyUsesCatalogEnvNames(t *testing.T) {
	clearEnv(t)
	t.Setenv("HYPER_API_KEY", "")
	t.Setenv("SOME_OTHER_KEY", "value-here")

	// models.dev lists several env names for some providers; the first
	// non-empty one wins.
	k, src, ok := ResolveProviderKey([]string{"HYPER_API_KEY", "SOME_OTHER_KEY"})
	if !ok {
		t.Fatal("expected to find a key")
	}
	if k.Reveal() != "value-here" || src != Source("SOME_OTHER_KEY") {
		t.Errorf("got source %q", src)
	}

	if _, _, ok := ResolveProviderKey([]string{"DEFINITELY_NOT_SET_XYZ"}); ok {
		t.Error("expected no key")
	}
}
