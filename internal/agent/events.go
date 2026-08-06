package agent

import (
	"charm.land/fantasy"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

// Event is something that happened during a turn.
//
// Events are plain values with no behavior so that a consumer can switch on
// them: the TUI forwards each one as a Bubble Tea message, a headless run prints
// it, and a test collects it. Keeping them free of UI types is what allows all
// three.
type Event interface{ isAgentEvent() }

// Emit receives events as they happen. Implementations must not block for long:
// they run on the streaming goroutine.
type Emit func(Event)

// TurnStartEvent marks the beginning of a turn.
type TurnStartEvent struct{ Prompt string }

// TurnEndEvent marks the end of a turn.
type TurnEndEvent struct {
	// Text is the assistant's final message.
	Text string

	// Usage is the turn's cumulative token usage.
	Usage fantasy.Usage

	// Cancelled is true when the person interrupted the turn.
	Cancelled bool
}

// TextStartEvent begins a block of assistant text.
type TextStartEvent struct{ ID string }

// TextDeltaEvent is a chunk of streaming assistant text.
type TextDeltaEvent struct {
	ID   string
	Text string
}

// TextEndEvent ends a block of assistant text.
type TextEndEvent struct{ ID string }

// ReasoningStartEvent begins a block of model reasoning.
type ReasoningStartEvent struct{ ID string }

// ReasoningDeltaEvent is a chunk of streaming reasoning.
type ReasoningDeltaEvent struct {
	ID   string
	Text string
}

// ReasoningEndEvent ends a block of model reasoning.
type ReasoningEndEvent struct{ ID string }

// ToolStartEvent fires when the model begins composing a tool call, before its
// arguments are complete.
type ToolStartEvent struct {
	ID   string
	Tool string
	Risk permission.Risk
}

// ToolInputDeltaEvent is a chunk of a tool call's arguments as they stream in.
type ToolInputDeltaEvent struct {
	ID    string
	Delta string
}

// ToolCallEvent fires when a tool call is complete and about to run.
type ToolCallEvent struct {
	ID    string
	Tool  string
	Input string
	Risk  permission.Risk
}

// ToolResultEvent fires when a tool finishes.
type ToolResultEvent struct {
	ID      string
	Tool    string
	Output  string
	IsError bool
}

// StepEvent fires at the end of each agent step.
type StepEvent struct {
	Step  int
	Usage fantasy.Usage

	// Cost is the running estimate in US dollars.
	Cost float64
}

// ErrorEvent reports a non-fatal error during a turn.
type ErrorEvent struct{ Err error }

func (TurnStartEvent) isAgentEvent()      {}
func (TurnEndEvent) isAgentEvent()        {}
func (TextStartEvent) isAgentEvent()      {}
func (TextDeltaEvent) isAgentEvent()      {}
func (TextEndEvent) isAgentEvent()        {}
func (ReasoningStartEvent) isAgentEvent() {}
func (ReasoningDeltaEvent) isAgentEvent() {}
func (ReasoningEndEvent) isAgentEvent()   {}
func (ToolStartEvent) isAgentEvent()      {}
func (ToolInputDeltaEvent) isAgentEvent() {}
func (ToolCallEvent) isAgentEvent()       {}
func (ToolResultEvent) isAgentEvent()     {}
func (StepEvent) isAgentEvent()           {}
func (ErrorEvent) isAgentEvent()          {}
