// Package auth resolves credentials without ever revealing them.
//
// Every function here returns a Source describing where a credential came from
// so that `webshim doctor` and error messages can be specific about what is
// configured, while the value itself stays inside a redacting type.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

// Source names where a credential was found. It never contains the value.
type Source string

const (
	SourceProjectConfig Source = "projects.config.json (per-project bearer)"
	SourceEnvWebSim     Source = "WEBSIM_BEARER"
	SourceEnvBearer     Source = "bearer"
	SourceEnvToken      Source = "WEBSIM_TOKEN"
	SourceCLIConfigEnv  Source = "WEBSIM_CLI_CONFIG"
	SourceCLIConfigHome Source = "~/.websim-cli.json"
)

// LoginHint is what to tell an operator who has no token. The playbook is
// explicit that we should not ask anyone to paste a token into a chat when CLI
// login is available.
const LoginHint = "run `websim-cli login`"

// ResolveWebSim finds a WebSim bearer token.
//
// The order is fixed by the playbook, first hit wins:
//
//  1. the per-project `bearer` from projects.config.json
//  2. WEBSIM_BEARER
//  3. bearer
//  4. WEBSIM_TOKEN
//  5. authToken from the file named by WEBSIM_CLI_CONFIG
//  6. authToken from ~/.websim-cli.json
//
// projectBearer is the value from the resolved project entry, or "".
func ResolveWebSim(projectBearer string) (websim.Token, Source, error) {
	if v := strings.TrimSpace(projectBearer); v != "" {
		return websim.Token(v), SourceProjectConfig, nil
	}

	for _, candidate := range []struct {
		env    string
		source Source
	}{
		{"WEBSIM_BEARER", SourceEnvWebSim},
		{"bearer", SourceEnvBearer},
		{"WEBSIM_TOKEN", SourceEnvToken},
	} {
		if v := strings.TrimSpace(os.Getenv(candidate.env)); v != "" {
			return websim.Token(v), candidate.source, nil
		}
	}

	if path := strings.TrimSpace(os.Getenv("WEBSIM_CLI_CONFIG")); path != "" {
		tok, err := readCLIToken(path)
		if err != nil {
			return "", "", fmt.Errorf("reading the file named by WEBSIM_CLI_CONFIG: %w", err)
		}
		if tok != "" {
			return websim.Token(tok), SourceCLIConfigEnv, nil
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		tok, err := readCLIToken(filepath.Join(home, ".websim-cli.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		if tok != "" {
			return websim.Token(tok), SourceCLIConfigHome, nil
		}
	}

	return "", "", fmt.Errorf("%w; %s", websim.ErrNoToken, LoginHint)
}

// cliConfig is the shape websim-cli writes.
type cliConfig struct {
	AuthToken string `json:"authToken"`
}

// readCLIToken extracts authToken from a websim-cli config file. A missing file
// yields os.ErrNotExist so callers can distinguish it from a malformed one.
func readCLIToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var cfg cliConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	return strings.TrimSpace(cfg.AuthToken), nil
}

// ProviderKey holds an LLM provider API key. Like websim.Token it redacts
// itself when formatted.
type ProviderKey string

func (k ProviderKey) String() string {
	if k == "" {
		return "[unset]"
	}
	return "[redacted]"
}

func (k ProviderKey) GoString() string             { return k.String() }
func (k ProviderKey) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (k ProviderKey) Reveal() string               { return string(k) }

// ResolveProviderKey looks for a provider API key in the given environment
// variables, in order.
//
// The names come from the models.dev catalog's per-provider `env` array, so
// this works for any provider without hardcoding a list.
func ResolveProviderKey(envNames []string) (ProviderKey, Source, bool) {
	for _, name := range envNames {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return ProviderKey(v), Source(name), true
		}
	}
	return "", "", false
}
