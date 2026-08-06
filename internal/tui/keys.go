package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/MetaMysteries8/webshim/internal/agent"
)

// handleKey routes a keypress.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// An open approval prompt takes precedence over everything except quit:
	// the agent is blocked waiting for it.
	if m.approval != nil {
		return m.handleApprovalKey(msg)
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m.quit()

	case key.Matches(msg, m.keys.Cancel):
		if m.running && m.cancelTurn != nil {
			m.cancelTurn()
			return m, statusCmd("stopping...", false)
		}
		return m, nil

	case key.Matches(msg, m.keys.Sidebar):
		m.showSidebar = !m.showSidebar
		return m.resize(m.width, m.height)

	case key.Matches(msg, m.keys.Reasoning):
		m.showReasoning = !m.showReasoning
		m.refreshViewport()
		label := "reasoning hidden"
		if m.showReasoning {
			label = "reasoning shown"
		}
		return m, statusCmd(label, false)

	case key.Matches(msg, m.keys.CycleMode):
		gate := m.deps.Agent.Gate()
		next := cycleMode(gate.Mode())
		gate.SetMode(next)
		return m, statusCmd("mode: "+string(next), next == "yolo")

	case key.Matches(msg, m.keys.Send):
		return m.submit()
	}

	return m.forwardToFocused(msg)
}

// quit shuts down, cancelling any turn in flight.
func (m Model) quit() (tea.Model, tea.Cmd) {
	if m.cancelTurn != nil {
		m.cancelTurn()
	}
	m.quitting = true
	return m, tea.Quit
}

// submit sends the input, either as a slash command or as a prompt.
func (m Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	if m.running {
		return m, statusCmd("already working; press esc to stop", true)
	}

	m.input.Reset()

	if strings.HasPrefix(text, "/") {
		return m.runCommand(text)
	}

	m.conversation.AddUser(text)
	m.refreshViewport()
	return m.startTurn(text)
}

// startTurn runs the agent on its own goroutine and bridges its events back
// into the Bubble Tea loop.
//
// A tea.Cmd can only deliver one message, at the end. Streaming needs messages
// while the turn is still running, so events go through the program handle
// instead; the command's return value only reports that the turn is over.
func (m Model) startTurn(prompt string) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	m.running = true

	ag := m.deps.Agent
	send := m.sender.Send

	return m, tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			_, err := ag.Run(ctx, prompt, func(ev agent.Event) {
				send(agentEventMsg{Event: ev})
			})
			cancel()
			return turnFinishedMsg{Err: err}
		},
	)
}

// handleApprovalKey routes keys while an approval prompt is open.
func (m Model) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Quit) {
		m.approval.decide(agent.Decision{Approved: false, Reason: "webshim is shutting down"})
		m.approval = nil
		return m.quit()
	}

	decision, done := m.approval.handleKey(msg)
	if !done {
		return m, nil
	}

	tool := m.approval.request.Tool
	m.approval.decide(decision)
	m.approval = nil
	m.input.Focus()

	if decision.Approved {
		m.conversation.ResumeAfterApproval(tool)
		m.refreshViewport()
		return m, statusCmd("approved", false)
	}
	m.refreshViewport()
	return m, statusCmd("declined", true)
}

// statusCmd shows a transient status message.
func statusCmd(text string, isError bool) tea.Cmd {
	return func() tea.Msg { return statusMsg{Text: text, Error: isError} }
}

// clearStatusAfter clears a status line once it has been up long enough to read.
func clearStatusAfter(seq int) tea.Cmd {
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg {
		return clearStatusMsg{Seq: seq}
	})
}

// refreshProjectCmd re-reads live project state in the background.
func (m Model) refreshProjectCmd() tea.Cmd {
	ag := m.deps.Agent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ag.Session().Refresh(ctx)
		return projectRefreshedMsg{}
	}
}
