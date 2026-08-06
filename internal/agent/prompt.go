package agent

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/MetaMysteries8/webshim/internal/mirror"
	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// promptFS holds the system prompt fragments.
//
// They live in files rather than string literals so they can be read and edited
// as prose, and so inproject.md stays a faithful copy of the platform API
// reference.
//
//go:embed all:prompts
var promptFS embed.FS

// promptFiles are concatenated in this order to build the system prompt.
var promptFiles = []string{
	"prompts/system.md",
	"prompts/safety.md",
	"prompts/inproject.md",
}

// ProjectContext is what the agent should know about the project it is working
// on. It is recomputed per turn so the model never reasons from a stale version
// number or file list.
type ProjectContext struct {
	Alias       string
	ProjectID   string
	Slug        string
	Title       string
	LiveVersion int
	MirrorDir   string
	Mode        permission.Mode

	// MirrorFiles is the local working copy's contents.
	MirrorFiles []mirror.Entry

	// LiveAssets is what the live revision contains.
	LiveAssets []websim.Asset

	// Diff summarizes uncommitted local changes, when the mirror is synced.
	Diff *mirror.Diff

	// Notes carries transient facts worth telling the model, such as "the
	// mirror has not been synced yet".
	Notes []string
}

// BuildSystemPrompt assembles the static instructions plus live project context.
func BuildSystemPrompt(pc ProjectContext) (string, error) {
	var b strings.Builder

	for _, name := range promptFiles {
		data, err := promptFS.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("agent: reading %s: %w", name, err)
		}
		b.Write(data)
		b.WriteString("\n\n")
	}

	b.WriteString(renderContext(pc))
	return b.String(), nil
}

// renderContext describes the current project state in prose the model can act
// on directly.
func renderContext(pc ProjectContext) string {
	var b strings.Builder
	b.WriteString("# Current project\n\n")

	if pc.ProjectID == "" {
		b.WriteString("No project is selected yet. Ask the person which project to work on, " +
			"or list what is configured with `websim_list_projects`.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "- alias: `%s`\n", pc.Alias)
	fmt.Fprintf(&b, "- project id: `%s`\n", pc.ProjectID)
	if pc.Title != "" {
		fmt.Fprintf(&b, "- title: %s\n", pc.Title)
	}
	if pc.Slug != "" {
		fmt.Fprintf(&b, "- public url: https://websim.com/%s\n", pc.Slug)
	}
	fmt.Fprintf(&b, "- live revision: v%d\n", pc.LiveVersion)
	fmt.Fprintf(&b, "- local mirror: `%s`\n", pc.MirrorDir)
	fmt.Fprintf(&b, "- permission mode: %s (%s)\n", pc.Mode, pc.Mode.Describe())

	if pc.Mode == permission.ModeManual {
		b.WriteString("\nThe person is approving every tool call, so keep calls purposeful " +
			"and explain what you are about to do.\n")
	}

	b.WriteString("\n## Local mirror\n\n")
	if len(pc.MirrorFiles) == 0 {
		b.WriteString("The mirror is empty. Call `websim_sync` to pull the live revision " +
			"before editing, unless you are intentionally starting fresh.\n")
	} else {
		for _, e := range pc.MirrorFiles {
			fmt.Fprintf(&b, "- `%s` (%d bytes)\n", e.Path, e.Size)
		}
	}

	if pc.Diff != nil && !pc.Diff.Empty() {
		fmt.Fprintf(&b, "\nUnpublished local changes: %s\n", pc.Diff.Summary())
		for _, p := range pc.Diff.Added {
			fmt.Fprintf(&b, "- added `%s`\n", p)
		}
		for _, p := range pc.Diff.Modified {
			fmt.Fprintf(&b, "- modified `%s`\n", p)
		}
		for _, p := range pc.Diff.Deleted {
			fmt.Fprintf(&b, "- deleted `%s`\n", p)
		}
		b.WriteString("\nCall `websim_publish` when these are ready to go live.\n")
	}

	if len(pc.LiveAssets) > 0 {
		b.WriteString("\n## Live revision contents\n\n")
		assets := append([]websim.Asset(nil), pc.LiveAssets...)
		sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
		for _, a := range assets {
			fmt.Fprintf(&b, "- `%s` (%d bytes)\n", a.Path, a.Size)
		}
		b.WriteString("\nThe homepage `index.html` is always present but is not listed here; " +
			"it is managed separately from other assets.\n")
	}

	if len(pc.Notes) > 0 {
		b.WriteString("\n## Notes\n\n")
		for _, n := range pc.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}

	return b.String()
}
