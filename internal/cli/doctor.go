package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MetaMysteries8/webshim/internal/auth"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/logging"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// check is one diagnostic result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type doctorReport struct {
	OK     bool    `json:"ok"`
	Checks []check `json:"checks"`
}

// runDoctor verifies everything webshim needs before it can do useful work.
//
// It never prints a credential: each check reports where a value came from, not
// what it is.
func runDoctor(args []string) error {
	var flags commonFlags
	if _, err := parseArgs("doctor", args, &flags); err != nil {
		return err
	}

	report := doctorReport{OK: true}
	add := func(c check) {
		if !c.OK {
			report.OK = false
		}
		report.Checks = append(report.Checks, c)
	}

	// Config.
	cfg, cfgErr := loadConfig(flags.configPath)
	switch {
	case errors.Is(cfgErr, config.ErrNoConfig):
		add(check{
			Name:   "config",
			OK:     true, // not fatal; a single-project run can work without it
			Detail: "no " + config.FileName + " found",
			Hint:   "copy projects.config.example.json to " + config.FileName + " to name your projects",
		})
	case cfgErr != nil:
		add(check{Name: "config", OK: false, Detail: cfgErr.Error()})
		cfg = &config.Config{}
	default:
		add(check{
			Name:   "config",
			OK:     true,
			Detail: fmt.Sprintf("%s (%d project(s): %v)", cfg.Path, len(cfg.Projects), cfg.Aliases()),
		})
	}

	// WebSim token.
	var projectBearer string
	if alias := cfg.DefaultProject; alias != "" {
		projectBearer = cfg.Projects[alias].Bearer
	}
	token, source, tokenErr := auth.ResolveWebSim(projectBearer)
	if tokenErr != nil {
		add(check{
			Name:   "websim token",
			OK:     false,
			Detail: "not found in any source",
			Hint:   auth.LoginHint,
		})
	} else {
		add(check{Name: "websim token", OK: true, Detail: "resolved from " + string(source)})
	}

	// Logging.
	logger, logErr := logging.New(logging.Options{Secrets: []string{token.Reveal()}})
	if logErr != nil {
		add(check{Name: "log file", OK: false, Detail: logErr.Error()})
	} else {
		defer logger.Close() //nolint:errcheck // diagnostic path
		add(check{Name: "log file", OK: true, Detail: logger.Path})
	}

	// Live session, only if we have something to try.
	if tokenErr == nil {
		client, err := websim.New(websim.Options{
			Token:     token,
			UserAgent: "webshim/" + Version,
		})
		if err != nil {
			add(check{Name: "websim session", OK: false, Detail: err.Error()})
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			session, err := client.GetSession(ctx)
			switch {
			case errors.Is(err, websim.ErrUnauthorized):
				add(check{
					Name: "websim session", OK: false,
					Detail: "the token was rejected (401)",
					Hint:   auth.LoginHint + " to get a fresh token",
				})
			case err != nil:
				add(check{Name: "websim session", OK: false, Detail: client.Sanitize(err.Error())})
			default:
				who := session.User.Username
				if who == "" {
					who = "(no username in response)"
				}
				add(check{Name: "websim session", OK: true, Detail: "signed in as " + who})
			}
		}
	}

	// Model provider key. Hyper is the default, so check it by name and fall
	// back to reporting what else is present.
	if key, keySource, ok := auth.ResolveProviderKey([]string{"HYPER_API_KEY"}); ok {
		_ = key
		add(check{Name: "model provider", OK: true, Detail: "Charm Hyper key from " + string(keySource)})
	} else {
		found := false
		for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
			if _, s, ok := auth.ResolveProviderKey([]string{name}); ok {
				add(check{Name: "model provider", OK: true, Detail: "key from " + string(s) + " (Hyper is the default; set HYPER_API_KEY to use it)"})
				found = true
				break
			}
		}
		if !found {
			add(check{
				Name: "model provider", OK: false,
				Detail: "no provider API key found",
				Hint:   "get a key at https://hyper.charm.land and export HYPER_API_KEY",
			})
		}
	}

	// Local mirror directory.
	if wd, err := os.Getwd(); err == nil {
		add(check{Name: "working directory", OK: true, Detail: filepath.Clean(wd)})
	}

	if flags.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	printDoctor(report)
	if !report.OK {
		return errors.New("some checks failed")
	}
	return nil
}

func printDoctor(r doctorReport) {
	fmt.Println("webshim doctor")
	fmt.Println()
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %s%s\n", mark, indent(c.Name, 18), c.Detail)
		if c.Hint != "" {
			fmt.Printf("         %s-> %s\n", indent("", 18), c.Hint)
		}
	}
	fmt.Println()
	if r.OK {
		fmt.Println("All checks passed.")
	} else {
		fmt.Println("Some checks failed. See the hints above.")
	}
}
