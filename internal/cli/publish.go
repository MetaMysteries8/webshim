package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

// maxPublishFiles caps how many files one publish will upload, so a stray
// `webshim publish .` in a node_modules-adjacent directory fails fast and
// loudly instead of uploading for an hour.
const maxPublishFiles = 200

// runPublish publishes a file or directory through the full Flow B lifecycle.
func runPublish(args []string) error {
	var flags commonFlags
	var message string
	positional, err := parseArgsWithExtra("publish", args, &flags, func(fs *flag.FlagSet) {
		fs.StringVar(&message, "m", "", "short public description of this edit")
	})
	if err != nil {
		return err
	}

	alias, rest := splitAliasAndRest(positional, 1)
	if len(rest) != 1 {
		return errors.New("usage: webshim publish [alias] <file-or-directory>")
	}
	target := rest[0]

	a, err := newApp(alias, flags, true)
	if err != nil {
		return err
	}
	defer a.Close()

	writes, err := collectChanges(target)
	if err != nil {
		return err
	}
	if message == "" {
		message = fmt.Sprintf("webshim publish: %d file(s)", len(writes))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	result, err := a.client.Publish(ctx, websim.PublishRequest{
		ProjectID:   a.project.ID,
		Writes:      writes,
		Description: message,
	})
	if err != nil {
		if errors.Is(err, websim.ErrDryRun) {
			fmt.Print(dryRunNote(true))
			fmt.Printf("Would have published %d file(s) to %s:\n", len(writes), a.alias)
			for _, w := range writes {
				fmt.Printf("  %s%s\n", indent(w.Path, 40), humanBytes(int64(len(w.Content))))
			}
			return nil
		}
		return err
	}

	if flags.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("Published %s: v%d -> v%d (verified)\n",
		a.alias, result.PreviousVersion, result.CurrentVersion)
	for _, p := range result.ChangedPaths {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("\nRoll back with:  webshim rollback %s %d\n", a.alias, result.PreviousVersion)
	return nil
}

// collectChanges turns a file or directory into a set of writes.
func collectChanges(target string) ([]websim.Change, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", target, err)
	}

	if !info.IsDir() {
		content, err := os.ReadFile(target)
		if err != nil {
			return nil, err
		}
		// A single file keeps only its base name, so `webshim publish
		// ./build/index.html` writes index.html rather than build/index.html.
		name := filepath.Base(target)
		if _, err := websim.ValidatePath(name); err != nil {
			return nil, err
		}
		return []websim.Change{{Path: name, Content: content}}, nil
	}

	var changes []websim.Change
	root := filepath.Clean(target)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if shouldSkipFile(d.Name()) {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, err := websim.ValidatePath(rel); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		changes = append(changes, websim.Change{Path: rel, Content: content})
		if len(changes) > maxPublishFiles {
			return fmt.Errorf("more than %d files under %s; publish a narrower directory",
				maxPublishFiles, root)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(changes) == 0 {
		return nil, fmt.Errorf("no publishable files found under %s", root)
	}
	// index.html first: it is the entrypoint, and writing it first makes the
	// intent obvious in the edit history.
	sort.Slice(changes, func(i, j int) bool {
		iIndex := websim.IsIndexPath(changes[i].Path)
		jIndex := websim.IsIndexPath(changes[j].Path)
		if iIndex != jIndex {
			return iIndex
		}
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

// shouldSkipDir excludes directories that are never part of a published site.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".github", "node_modules", "vendor", ".next", "dist_cache", ".cache", "__pycache__":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// shouldSkipFile excludes local-only files.
func shouldSkipFile(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", "projects.config.json":
		return true
	}
	// Dotfiles and anything that looks like a credential stay local. This is
	// belt-and-braces: publishing writes public client-side content, so a
	// stray .env would be world-readable.
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".token")
}

// runRollback makes an earlier published revision current.
func runRollback(args []string) error {
	var flags commonFlags
	positional, err := parseArgs("rollback", args, &flags)
	if err != nil {
		return err
	}

	alias, rest := splitAliasAndRest(positional, 1)
	if len(rest) != 1 {
		return errors.New("usage: webshim rollback [alias] <version>")
	}
	version, err := strconv.Atoi(rest[0])
	if err != nil {
		return fmt.Errorf("version must be an integer, got %q", rest[0])
	}

	a, err := newApp(alias, flags, true)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := a.client.Rollback(ctx, a.project.ID, version)
	if err != nil {
		if errors.Is(err, websim.ErrDryRun) {
			fmt.Print(dryRunNote(true))
			fmt.Printf("Would have made v%d current for %s.\n", version, a.alias)
			return nil
		}
		return err
	}

	if flags.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("Rolled back %s: v%d -> v%d (verified)\n",
		a.alias, result.PreRollbackVersion, result.CurrentVersion)
	fmt.Printf("\nUndo with:  webshim rollback %s %d\n", a.alias, result.PreRollbackVersion)
	return nil
}
