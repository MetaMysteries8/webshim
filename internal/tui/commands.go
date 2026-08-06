package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/permission"
)

// sprintf is a local alias so command handlers read cleanly.
var sprintf = fmt.Sprintf

// runCommand handles a slash command.
//
// Commands that would change live state are phrased as instructions to the agent
// rather than executed directly, so that they still pass through the permission
// gate and appear in the transcript. /publish is a request, not a bypass.
func (m Model) runCommand(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]

	switch cmd {
	case "help", "?":
		m.conversation.AddNotice(helpText())
		m.refreshViewport()
		return m, nil

	case "quit", "exit", "q":
		return m.quit()

	case "clear":
		m.conversation.Clear()
		m.deps.Agent.Session().Reset()
		m.refreshViewport()
		return m, statusCmd("conversation cleared", false)

	case "mode":
		return m.setMode(args)

	case "model":
		return m.showModels(args)

	case "cost":
		s := m.deps.Agent.Session()
		u := s.Usage()
		m.conversation.AddNotice(sprintf(
			"%d step(s)  ·  %d in / %d out / %d total tokens  ·  about $%.4f  ·  %s",
			s.Steps(), u.InputTokens, u.OutputTokens, u.TotalTokens, s.Cost(),
			m.deps.Agent.Describe()))
		m.refreshViewport()
		return m, nil

	case "log":
		path := m.deps.LogPath
		if path == "" {
			path = "(logging is disabled)"
		}
		m.conversation.AddNotice("log file: " + path)
		m.refreshViewport()
		return m, nil

	case "tools":
		names := m.deps.Agent.ToolNames()
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("available tools\n")
		for _, n := range names {
			fmt.Fprintf(&b, "  %-32s %s\n", n, m.deps.Agent.RiskOf(n))
		}
		m.conversation.AddNotice(b.String())
		m.refreshViewport()
		return m, nil

	case "reasoning":
		m.showReasoning = !m.showReasoning
		m.refreshViewport()
		return m, statusCmd(sprintf("reasoning %s", onOff(m.showReasoning)), false)

	case "sidebar":
		m.showSidebar = !m.showSidebar
		return m.resize(m.width, m.height)

	// These are handed to the agent so they go through the permission gate
	// and show up in the transcript like any other action.
	case "sync":
		return m.instruct("Sync the local mirror with the live revision.")
	case "publish":
		msg := "Publish the local changes."
		if len(args) > 0 {
			msg = "Publish the local changes with the description: " + strings.Join(args, " ")
		}
		return m.instruct(msg)
	case "rollback":
		if len(args) != 1 {
			return m, statusCmd("usage: /rollback <version>", true)
		}
		if _, err := strconv.Atoi(args[0]); err != nil {
			return m, statusCmd("version must be a number", true)
		}
		return m.instruct("Roll the project back to revision v" + args[0] + ".")
	case "diff":
		return m.instruct("Show me what has changed locally since the last sync.")
	case "revisions":
		return m.instruct("List the project's revisions.")
	case "comments":
		return m.instruct("List the project's comments.")

	default:
		return m, statusCmd("unknown command: /"+cmd+"  (try /help)", true)
	}
}

// instruct sends a phrase to the agent as if the person had typed it.
func (m Model) instruct(prompt string) (tea.Model, tea.Cmd) {
	m.conversation.AddUser(prompt)
	m.refreshViewport()
	return m.startTurn(prompt)
}

// setMode changes the permission mode.
func (m Model) setMode(args []string) (tea.Model, tea.Cmd) {
	gate := m.deps.Agent.Gate()
	if len(args) == 0 {
		m.conversation.AddNotice(sprintf("mode: %s (%s)\n  modes: manual, normal, yolo",
			gate.Mode(), gate.Mode().Describe()))
		m.refreshViewport()
		return m, nil
	}

	mode, err := permission.ParseMode(strings.ToLower(args[0]))
	if err != nil {
		return m, statusCmd(err.Error(), true)
	}
	gate.SetMode(mode)

	if mode == permission.ModeYOLO {
		m.conversation.AddNotice(
			"mode: yolo — the agent will publish and change live state without asking.")
		m.refreshViewport()
	}
	return m, statusCmd("mode: "+string(mode), mode == permission.ModeYOLO)
}

// showModels lists the models available for the current provider.
//
// Switching models mid-session is not wired up: the Fantasy model is built at
// startup, and rebuilding it here would need the provider key and a fresh
// client. Listing is still useful, and the command says what to do instead.
func (m Model) showModels(args []string) (tea.Model, tea.Cmd) {
	if m.deps.Catalog == nil {
		return m, statusCmd("no model catalog is loaded", true)
	}
	if len(args) > 0 {
		return m, statusCmd(
			"switching models mid-session is not supported yet; restart with --model "+args[0], true)
	}

	current := m.deps.Agent.Session().Model()
	models := m.deps.Catalog.ToolCallModels(current.ProviderID)
	if len(models) == 0 {
		models = m.deps.Catalog.ToolCallModels(catalog.DefaultProviderID)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "current: %s\n\navailable (restart with --model ID to switch):\n",
		m.deps.Agent.Describe())
	const max = 12
	for i, mm := range models {
		if i == max {
			fmt.Fprintf(&b, "  ... and %d more (webshim models)\n", len(models)-max)
			break
		}
		marker := " "
		if mm.ID == current.ID {
			marker = "*"
		}
		fmt.Fprintf(&b, " %s %-32s %s\n", marker, mm.ID, modelBadges(mm))
	}
	m.conversation.AddNotice(b.String())
	m.refreshViewport()
	return m, nil
}

func modelBadges(m catalog.Model) string {
	var parts []string
	if m.Limit.Context > 0 {
		parts = append(parts, humanCount(int64(m.Limit.Context))+" ctx")
	}
	if m.Cost.Input > 0 {
		parts = append(parts, sprintf("$%.2f/$%.2f", m.Cost.Input, m.Cost.Output))
	}
	if m.Reasoning {
		parts = append(parts, "reasoning")
	}
	return strings.Join(parts, "  ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func helpText() string {
	return `webshim

Type what you want and press enter. The agent edits a local copy of the
project, then publishes when you approve.

  /sync              pull the live revision into the local copy
  /publish [note]    publish local changes
  /diff              show what changed locally
  /rollback <ver>    make an earlier revision live
  /revisions         list revisions
  /comments          list project comments

  /mode [m]          manual | normal | yolo
  /model             list models
  /tools             list tools and their risk
  /cost              tokens and estimated spend
  /reasoning         show or hide model reasoning
  /sidebar           show or hide the file panel
  /clear             clear the conversation
  /log               where the log file lives
  /quit              exit

keys
  enter              send            alt+enter   newline
  esc                stop the turn   ctrl+c      quit
  ctrl+b             sidebar         ctrl+r      reasoning
  ctrl+p             cycle mode      pgup/pgdn   scroll

Permission modes
  manual   approve everything, including reads
  normal   edits happen freely; live changes ask first
  yolo     nothing asks`
}
