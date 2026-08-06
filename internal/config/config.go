// Package config loads webshim's configuration and resolves project aliases.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

// FileName is the config file webshim looks for.
const FileName = "projects.config.json"

// ErrNoConfig means no config file was found in any search location.
var ErrNoConfig = errors.New("config: no " + FileName + " found")

// Project is one configured project alias.
type Project struct {
	// ID is the stable project ID used in API paths and content-host
	// domains. Required.
	ID string `json:"id"`

	// Slug is the human-facing slug, used in the Referer header that comment
	// endpoints require.
	Slug string `json:"slug"`

	// Bearer optionally overrides every other token source for this project.
	// Prefer leaving it empty and using `websim-cli login`; a token in a
	// config file is a token that can be committed by accident.
	Bearer string `json:"bearer,omitempty"`
}

// Agent holds model and autonomy defaults.
type Agent struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// Config is the parsed config file.
type Config struct {
	DefaultProject string             `json:"defaultProject,omitempty"`
	Projects       map[string]Project `json:"projects,omitempty"`
	Agent          Agent              `json:"agent,omitempty"`

	// Path is where this config was loaded from. Empty for a default config.
	Path string `json:"-"`
}

// SearchPaths lists, in priority order, where a config file may live.
func SearchPaths() []string {
	var paths []string
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, FileName))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "webshim", FileName))
	}
	return paths
}

// Load reads the first config file found in SearchPaths.
//
// A missing config is not an error: Load returns an empty config with
// ErrNoConfig so a caller can decide whether that matters. A malformed config
// is an error, because silently ignoring it would hide a typo in a project ID.
func Load() (*Config, error) {
	for _, p := range SearchPaths() {
		cfg, err := LoadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}
	return &Config{Projects: map[string]Project{}}, ErrNoConfig
}

// LoadFile reads a config from an explicit path.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	cfg.Path = path
	if cfg.Projects == nil {
		cfg.Projects = map[string]Project{}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// validate rejects a config that would produce confusing failures later.
func (c *Config) validate() error {
	for alias, p := range c.Projects {
		if p.ID == "" {
			return fmt.Errorf("project %q has no id", alias)
		}
		if strings.HasPrefix(p.ID, "REPLACE_") {
			return fmt.Errorf("project %q still has the placeholder id %q; fill it in", alias, p.ID)
		}
	}
	if c.DefaultProject != "" {
		if _, ok := c.Projects[c.DefaultProject]; !ok {
			return fmt.Errorf("defaultProject %q is not in projects", c.DefaultProject)
		}
	}
	if _, err := permission.ParseMode(c.Agent.Mode); err != nil {
		return fmt.Errorf("agent.mode: %w", err)
	}
	return nil
}

// Aliases returns the configured aliases, sorted.
func (c *Config) Aliases() []string {
	out := make([]string, 0, len(c.Projects))
	for a := range c.Projects {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Resolve looks up a project by alias.
//
// An empty alias selects DefaultProject. An unknown alias is an error that
// lists what is available -- the playbook is explicit that an ID must never be
// guessed from a title or slug.
func (c *Config) Resolve(alias string) (string, Project, error) {
	if alias == "" {
		if c.DefaultProject == "" {
			if len(c.Projects) == 1 {
				for a, p := range c.Projects {
					return a, p, nil
				}
			}
			return "", Project{}, fmt.Errorf(
				"no project specified and no defaultProject is set%s", c.availableSuffix())
		}
		alias = c.DefaultProject
	}

	p, ok := c.Projects[alias]
	if !ok {
		return "", Project{}, fmt.Errorf("unknown project alias %q%s", alias, c.availableSuffix())
	}
	return alias, p, nil
}

func (c *Config) availableSuffix() string {
	aliases := c.Aliases()
	if len(aliases) == 0 {
		if c.Path == "" {
			return fmt.Sprintf("; no %s was found (looked in %s)",
				FileName, strings.Join(SearchPaths(), ", "))
		}
		return fmt.Sprintf("; %s defines no projects", c.Path)
	}
	return "; known aliases: " + strings.Join(aliases, ", ")
}

// Mode returns the configured permission mode, defaulting to normal.
func (c *Config) Mode() permission.Mode {
	m, err := permission.ParseMode(c.Agent.Mode)
	if err != nil {
		return permission.ModeNormal
	}
	return m
}

// MirrorDir is the local working copy directory for an alias.
func MirrorDir(alias string) string {
	return filepath.Join("projects", alias)
}

// StateDir returns the directory for logs and other local state, creating it if
// necessary.
func StateDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "webshim")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
