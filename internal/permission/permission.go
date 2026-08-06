// Package permission defines what the agent may do without asking.
//
// It is deliberately tiny and dependency-free so that the config loader, the
// agent, and the TUI can all share one definition of the rules rather than each
// reimplementing them.
package permission

import "fmt"

// Mode is how much autonomy the agent has.
type Mode string

const (
	// ModeManual asks before every tool call, including reads. Use it when
	// you want to watch exactly what the agent does.
	ModeManual Mode = "manual"

	// ModeNormal is the default. Reads and edits to an unpromoted draft
	// happen freely; anything that changes live or public state asks first.
	ModeNormal Mode = "normal"

	// ModeYOLO never asks. The agent runs the whole flow and reports.
	ModeYOLO Mode = "yolo"
)

// Modes lists every mode in increasing order of autonomy.
var Modes = []Mode{ModeManual, ModeNormal, ModeYOLO}

// ParseMode converts a string to a Mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeManual:
		return ModeManual, nil
	case ModeNormal:
		return ModeNormal, nil
	case ModeYOLO:
		return ModeYOLO, nil
	case "":
		return ModeNormal, nil
	}
	return "", fmt.Errorf("unknown permission mode %q (want manual, normal, or yolo)", s)
}

// Describe returns a one-line summary suitable for a status bar or help text.
func (m Mode) Describe() string {
	switch m {
	case ModeManual:
		return "asks before every action"
	case ModeNormal:
		return "edits freely, asks before changing live state"
	case ModeYOLO:
		return "never asks"
	}
	return string(m)
}

// Risk classifies what a tool can affect.
type Risk int

const (
	// RiskRead only reads. It cannot change anything.
	RiskRead Risk = iota

	// RiskEdit writes to an unpromoted draft revision or to the local
	// project mirror. Nothing a visitor can see changes, and the work is
	// discardable by simply not promoting it.
	RiskEdit

	// RiskCommand changes live or public state, or runs a command on the
	// host. Finalizing, promoting, rolling back, deleting, creating a
	// project, editing metadata, posting comments, and shell execution are
	// all RiskCommand.
	RiskCommand
)

func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskEdit:
		return "edit"
	case RiskCommand:
		return "command"
	}
	return "unknown"
}

// NeedsApproval reports whether an action of this risk requires the operator to
// confirm before it runs.
//
//	                 read   edit   command
//	manual           ask    ask    ask
//	normal           auto   auto   ask
//	yolo             auto   auto   auto
//
// An unrecognized mode is treated as ModeManual. Failing closed matters here:
// a typo in a config file must not silently grant more autonomy than intended.
func NeedsApproval(m Mode, r Risk) bool {
	switch m {
	case ModeYOLO:
		return false
	case ModeNormal:
		return r == RiskCommand
	case ModeManual:
		return true
	}
	return true
}
