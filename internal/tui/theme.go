// Package tui is webshim's terminal interface.
//
// Every model here is built from an explicit Deps value rather than package
// globals, and nothing writes to stdout or calls os.Exit. That discipline is
// what will let the same models be served over SSH later, where each connection
// needs its own state, its own colour profile, and its own credentials.
package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

// Theme holds every style the UI uses.
//
// Colours are resolved once, from the terminal's reported background, rather
// than being sampled at render time. Over SSH the background is per-session, so
// a global would be wrong.
type Theme struct {
	dark bool

	Base    lipgloss.Style
	Faint   lipgloss.Style
	Bold    lipgloss.Style
	Title   lipgloss.Style
	Border  lipgloss.Style
	Focused lipgloss.Style

	UserLabel      lipgloss.Style
	AssistantLabel lipgloss.Style
	Reasoning      lipgloss.Style

	ToolRunning lipgloss.Style
	ToolOK      lipgloss.Style
	ToolError   lipgloss.Style

	StatusBar  lipgloss.Style
	ModeManual lipgloss.Style
	ModeNormal lipgloss.Style
	ModeYOLO   lipgloss.Style

	Added    lipgloss.Style
	Modified lipgloss.Style
	Deleted  lipgloss.Style

	Error   lipgloss.Style
	Warning lipgloss.Style
	Success lipgloss.Style
}

// NewTheme builds a theme for a light or dark terminal.
func NewTheme(hasDarkBackground bool) Theme {
	ld := lipgloss.LightDark(hasDarkBackground)

	var (
		fg      = ld(lipgloss.Color("#1a1a1a"), lipgloss.Color("#e6e6e6"))
		faint   = ld(lipgloss.Color("#6b6b6b"), lipgloss.Color("#8a8a8a"))
		accent  = ld(lipgloss.Color("#7d3ac1"), lipgloss.Color("#b78cf0"))
		green   = ld(lipgloss.Color("#0a7a35"), lipgloss.Color("#5fd68a"))
		red     = ld(lipgloss.Color("#b3261e"), lipgloss.Color("#ff8a80"))
		yellow  = ld(lipgloss.Color("#8a6100"), lipgloss.Color("#f0c674"))
		blue    = ld(lipgloss.Color("#0b5cad"), lipgloss.Color("#7fb8f0"))
		barBG   = ld(lipgloss.Color("#e8e6ef"), lipgloss.Color("#2a2735"))
		borderC = ld(lipgloss.Color("#c9c6d4"), lipgloss.Color("#4a4658"))
	)

	base := lipgloss.NewStyle().Foreground(fg)

	return Theme{
		dark:    hasDarkBackground,
		Base:    base,
		Faint:   lipgloss.NewStyle().Foreground(faint),
		Bold:    base.Bold(true),
		Title:   lipgloss.NewStyle().Foreground(accent).Bold(true),
		Border:  lipgloss.NewStyle().Foreground(borderC),
		Focused: lipgloss.NewStyle().Foreground(accent),

		UserLabel:      lipgloss.NewStyle().Foreground(blue).Bold(true),
		AssistantLabel: lipgloss.NewStyle().Foreground(accent).Bold(true),
		Reasoning:      lipgloss.NewStyle().Foreground(faint).Italic(true),

		ToolRunning: lipgloss.NewStyle().Foreground(yellow),
		ToolOK:      lipgloss.NewStyle().Foreground(green),
		ToolError:   lipgloss.NewStyle().Foreground(red),

		StatusBar:  lipgloss.NewStyle().Background(barBG).Foreground(fg),
		ModeManual: lipgloss.NewStyle().Background(blue).Foreground(lipgloss.Color("#ffffff")).Bold(true).Padding(0, 1),
		ModeNormal: lipgloss.NewStyle().Background(green).Foreground(lipgloss.Color("#000000")).Bold(true).Padding(0, 1),
		ModeYOLO:   lipgloss.NewStyle().Background(red).Foreground(lipgloss.Color("#ffffff")).Bold(true).Padding(0, 1),

		Added:    lipgloss.NewStyle().Foreground(green),
		Modified: lipgloss.NewStyle().Foreground(yellow),
		Deleted:  lipgloss.NewStyle().Foreground(red),

		Error:   lipgloss.NewStyle().Foreground(red).Bold(true),
		Warning: lipgloss.NewStyle().Foreground(yellow),
		Success: lipgloss.NewStyle().Foreground(green),
	}
}

// IsDark reports whether the theme targets a dark terminal.
func (t Theme) IsDark() bool { return t.dark }

// ModeBadge renders the permission mode chip for the status bar.
//
// The colours are not decorative: YOLO is red because the agent will change a
// live site without asking, and that should be impossible to miss.
func (t Theme) ModeBadge(m permission.Mode) string {
	switch m {
	case permission.ModeYOLO:
		return t.ModeYOLO.Render("YOLO")
	case permission.ModeNormal:
		return t.ModeNormal.Render("NORMAL")
	default:
		return t.ModeManual.Render("MANUAL")
	}
}
