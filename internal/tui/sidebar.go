package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
)

// sidebar shows the project's files and unpublished changes.
type sidebar struct {
	width, height int

	files       []fileRow
	liveVersion int
	diffSummary string
	dirty       bool
}

// fileRow is one file in the sidebar.
type fileRow struct {
	path   string
	size   int64
	status rune // ' ' unchanged, '+' added, '~' modified, '-' deleted
}

func newSidebar() sidebar { return sidebar{} }

func (s *sidebar) setSize(w, h int) {
	s.width, s.height = w, h
}

// refresh re-reads the mirror and live state.
//
// It reads through the session rather than making its own API calls, so opening
// the sidebar never costs a request.
func (s *sidebar) refresh(deps Deps) {
	session := deps.Agent.Session()
	s.liveVersion = session.LiveVersion()

	m := session.Mirror()
	if m == nil {
		return
	}

	entries, err := m.List()
	if err != nil {
		return
	}

	status := map[string]rune{}
	s.dirty = false
	s.diffSummary = ""
	if diff, _, err := m.Diff(); err == nil {
		s.diffSummary = diff.Summary()
		s.dirty = !diff.Empty()
		for _, p := range diff.Added {
			status[p] = '+'
		}
		for _, p := range diff.Modified {
			status[p] = '~'
		}
		for _, p := range diff.Deleted {
			status[p] = '-'
		}
	}

	s.files = s.files[:0]
	for _, e := range entries {
		mark, ok := status[e.Path]
		if !ok {
			mark = ' '
		}
		s.files = append(s.files, fileRow{path: e.Path, size: e.Size, status: mark})
	}
	// Deleted files are gone from the mirror listing but still worth showing,
	// because they are part of the next publish.
	for p, mark := range status {
		if mark != '-' {
			continue
		}
		s.files = append(s.files, fileRow{path: p, status: '-'})
	}
}

// view renders the sidebar.
func (s sidebar) view(t Theme) string {
	if s.width <= 0 {
		return ""
	}
	inner := s.width - 3
	if inner < 10 {
		inner = 10
	}

	var b strings.Builder

	title := "files"
	if s.liveVersion > 0 {
		title = fmt.Sprintf("files · v%d", s.liveVersion)
	}
	b.WriteString(t.Title.Render(title))
	b.WriteString("\n\n")

	if len(s.files) == 0 {
		b.WriteString(t.Faint.Render("empty — /sync to pull"))
	} else {
		b.WriteString(s.fileTree(t, inner))
	}

	if s.dirty {
		b.WriteString("\n\n")
		b.WriteString(t.Warning.Render("unpublished: " + s.diffSummary))
		b.WriteString("\n")
		b.WriteString(t.Faint.Render("/publish to go live"))
	}

	return lipgloss.NewStyle().
		Width(s.width).
		Height(s.height).
		Padding(0, 1).
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(t.Border.GetForeground()).
		Render(b.String())
}

// fileTree renders the files as a directory tree.
func (s sidebar) fileTree(t Theme, width int) string {
	root := tree.Root(".")

	// Group by directory so nesting is visible rather than a flat list of
	// slash-separated paths.
	//
	// ensure is declared before it is assigned because it recurses: creating
	// "a/b/c" has to create "a/b" first.
	dirs := map[string]*tree.Tree{"": root}
	var ensure func(dir string) *tree.Tree
	ensure = func(dir string) *tree.Tree {
		if node, ok := dirs[dir]; ok {
			return node
		}
		parent := ""
		name := dir
		if i := strings.LastIndex(dir, "/"); i >= 0 {
			parent, name = dir[:i], dir[i+1:]
		}
		node := tree.Root(name)
		ensure(parent).Child(node)
		dirs[dir] = node
		return node
	}

	for _, f := range s.files {
		dir := ""
		name := f.path
		if i := strings.LastIndex(f.path, "/"); i >= 0 {
			dir, name = f.path[:i], f.path[i+1:]
		}
		ensure(dir).Child(s.label(t, name, f, width))
	}

	return root.
		Enumerator(tree.RoundedEnumerator).
		EnumeratorStyle(t.Border).
		String()
}

// label renders one file entry with its change marker.
func (s sidebar) label(t Theme, name string, f fileRow, width int) string {
	style := t.Base
	switch f.status {
	case '+':
		style = t.Added
	case '~':
		style = t.Modified
	case '-':
		style = t.Deleted
	}

	label := name
	if f.status != ' ' {
		label = string(f.status) + " " + name
	}
	if lipgloss.Width(label) > width {
		label = truncate(label, width)
	}
	return style.Render(label)
}
