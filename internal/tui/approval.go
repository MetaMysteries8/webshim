package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MetaMysteries8/webshim/internal/agent"
	"github.com/MetaMysteries8/webshim/internal/permission"
)

// approvalPrompt is the blocking confirmation shown when a tool needs approval.
//
// It is a plain keyed prompt rather than a huh form: the agent's goroutine is
// blocked on the answer, and a single unambiguous keystroke is both faster to
// answer and harder to get wrong than a focus-managed widget. The one decision
// that matters here is that the default is "no" -- every path that is not an
// explicit yes declines.
type approvalPrompt struct {
	request agent.Request
	reply   chan<- agent.Decision
	theme   Theme
	keys    approvalKeys
}

type approvalKeys struct {
	Yes    key.Binding
	No     key.Binding
	Always key.Binding
}

func newApprovalPrompt(req agent.Request, reply chan<- agent.Decision, t Theme) *approvalPrompt {
	return &approvalPrompt{
		request: req,
		reply:   reply,
		theme:   t,
		keys: approvalKeys{
			Yes:    key.NewBinding(key.WithKeys("y", "enter"), key.WithHelp("y", "approve")),
			No:     key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n/esc", "decline")),
			Always: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "always allow this tool")),
		},
	}
}

// handleKey interprets a keypress. done is false while the prompt is still open.
func (p *approvalPrompt) handleKey(msg tea.KeyPressMsg) (decision agent.Decision, done bool) {
	switch {
	case key.Matches(msg, p.keys.Yes):
		return agent.Decision{Approved: true}, true
	case key.Matches(msg, p.keys.No):
		return agent.Decision{Approved: false, Reason: "the person declined"}, true
	case key.Matches(msg, p.keys.Always):
		return agent.Decision{Approved: true, Reason: alwaysMarker}, true
	}
	return agent.Decision{}, false
}

// alwaysMarker signals that the tool should be allowed for the rest of the
// session. It travels in Reason because Decision has no field for it, and the
// caller strips it before the reason reaches the model.
const alwaysMarker = "__always__"

// decide hands the answer back to the blocked tool.
func (p *approvalPrompt) decide(d agent.Decision) {
	if d.Reason == alwaysMarker {
		d.Reason = ""
	}
	// The channel is buffered, so this cannot block even if the turn was
	// cancelled and nobody is reading.
	select {
	case p.reply <- d:
	default:
	}
}

// Update lets the prompt consume non-key messages. It currently needs none.
func (p *approvalPrompt) Update(tea.Msg) tea.Cmd { return nil }

// View renders the prompt.
func (p *approvalPrompt) View(width int) string {
	t := p.theme

	risk := t.ToolRunning.Render("[" + p.request.Risk.String() + "]")
	if p.request.Risk == permission.RiskCommand {
		risk = t.Error.Render("[" + p.request.Risk.String() + "]")
	}

	var b strings.Builder
	b.WriteString(risk + " " + t.Bold.Render(p.request.Summary))

	if detail := strings.TrimRight(p.request.Detail, "\n"); detail != "" {
		lines := strings.Split(detail, "\n")
		const maxLines = 12
		truncatedBy := 0
		if len(lines) > maxLines {
			truncatedBy = len(lines) - maxLines
			lines = lines[:maxLines]
		}
		b.WriteString("\n")
		for _, line := range lines {
			b.WriteString("\n" + t.Faint.Render(truncate(line, width-4)))
		}
		if truncatedBy > 0 {
			b.WriteString("\n" + t.Faint.Render(sprintf("  ... and %d more line(s)", truncatedBy)))
		}
	}

	b.WriteString("\n\n")
	b.WriteString(t.Bold.Render("y") + t.Faint.Render(" approve   "))
	b.WriteString(t.Bold.Render("n") + t.Faint.Render(" decline   "))
	b.WriteString(t.Bold.Render("a") + t.Faint.Render(" always allow "+p.request.Tool))

	return lipgloss.NewStyle().
		Width(width-2).
		Padding(0, 1).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(t.Focused.GetForeground()).
		Render(b.String())
}
