package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/MetaMysteries8/webshim/internal/agent"
)

// agentEventMsg carries an agent event into the Bubble Tea loop.
//
// The agent runs on its own goroutine and emits events as they happen. Wrapping
// each one and calling Program.Send is the whole bridge: the model stays
// single-threaded and the agent never touches UI state directly.
type agentEventMsg struct{ Event agent.Event }

// turnFinishedMsg fires when a turn's goroutine exits, successfully or not.
type turnFinishedMsg struct{ Err error }

// permissionRequestMsg asks the UI to present an approval prompt.
//
// Reply carries the operator's answer back to the blocked tool. The channel is
// buffered by the sender so a cancelled turn cannot deadlock the UI.
type permissionRequestMsg struct {
	Request agent.Request
	Reply   chan<- agent.Decision
}

// statusMsg shows a transient line in the status bar.
type statusMsg struct {
	Text  string
	Error bool
}

// clearStatusMsg removes a transient status line.
type clearStatusMsg struct{ Seq int }

// projectRefreshedMsg carries refreshed live project state.
type projectRefreshedMsg struct{}

// tickMsg drives animations.
type tickMsg struct{}

// emitToProgram builds an agent.Emit that forwards events into a program.
func emitToProgram(send func(tea.Msg)) agent.Emit {
	return func(ev agent.Event) {
		send(agentEventMsg{Event: ev})
	}
}
