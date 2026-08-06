package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

// lsReport is the machine-readable form of an inspection.
type lsReport struct {
	Alias          string         `json:"alias"`
	ProjectID      string         `json:"project_id"`
	Slug           string         `json:"slug"`
	Title          string         `json:"title"`
	CurrentVersion int            `json:"current_version"`
	Revisions      []lsRevision   `json:"revisions"`
	Assets         []websim.Asset `json:"assets"`
}

type lsRevision struct {
	Version int  `json:"version"`
	Draft   bool `json:"draft"`
	Live    bool `json:"live"`
	Parent  *int `json:"parent_version,omitempty"`
}

// runLS implements Flow A: inspect a project without changing it. It makes no
// mutating calls at all.
func runLS(args []string) error {
	var flags commonFlags
	positional, err := parseArgs("ls", args, &flags)
	if err != nil {
		return err
	}
	alias, _ := splitAliasAndRest(positional, 0)

	a, err := newApp(alias, flags, true)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	project, err := a.client.GetProject(ctx, a.project.ID)
	if err != nil {
		return err
	}
	current, err := project.RequireCurrentVersion()
	if err != nil {
		return err
	}

	revisions, err := a.client.ListRevisions(ctx, a.project.ID)
	if err != nil {
		return err
	}
	assets, err := a.client.ListAssets(ctx, a.project.ID, current)
	if err != nil {
		return err
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })

	report := lsReport{
		Alias:          a.alias,
		ProjectID:      project.ID,
		Slug:           project.Slug,
		Title:          project.Title,
		CurrentVersion: current,
		Assets:         assets,
	}
	for _, r := range revisions {
		if r.Version == nil {
			continue
		}
		report.Revisions = append(report.Revisions, lsRevision{
			Version: *r.Version,
			Draft:   r.Draft,
			Live:    *r.Version == current,
			Parent:  r.ParentRevisionVersion,
		})
	}
	sort.Slice(report.Revisions, func(i, j int) bool {
		return report.Revisions[i].Version > report.Revisions[j].Version
	})

	if flags.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printLS(report)
	return nil
}

func printLS(r lsReport) {
	fmt.Printf("%s%s\n", indent("project", 12), r.Title)
	fmt.Printf("%s%s\n", indent("alias", 12), r.Alias)
	fmt.Printf("%s%s\n", indent("id", 12), r.ProjectID)
	if r.Slug != "" {
		fmt.Printf("%shttps://websim.com/%s\n", indent("url", 12), r.Slug)
	}
	fmt.Printf("%s%d\n", indent("live", 12), r.CurrentVersion)

	fmt.Printf("\nrevisions (%d)\n", len(r.Revisions))
	shown := r.Revisions
	const maxRevisions = 15
	truncated := 0
	if len(shown) > maxRevisions {
		truncated = len(shown) - maxRevisions
		shown = shown[:maxRevisions]
	}
	for _, rev := range shown {
		state := "published"
		if rev.Draft {
			state = "draft"
		}
		marker := "  "
		if rev.Live {
			marker = "* "
		}
		parent := ""
		if rev.Parent != nil {
			parent = fmt.Sprintf("  (from %d)", *rev.Parent)
		}
		fmt.Printf("  %sv%-6d %s%s\n", marker, rev.Version, state, parent)
	}
	if truncated > 0 {
		fmt.Printf("  ... and %d older revision(s); use --json to see them all\n", truncated)
	}

	fmt.Printf("\nassets in v%d (%d)\n", r.CurrentVersion, len(r.Assets))
	if len(r.Assets) == 0 {
		fmt.Println("  (none besides the homepage)")
	}
	for _, a := range r.Assets {
		fmt.Printf("  %s%s\n", indent(a.Path, 40), humanBytes(a.Size))
	}
}

// humanBytes formats a size for display.
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
