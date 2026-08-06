package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MetaMysteries8/webshim/internal/agent"
	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/permission"
)

// Deps is everything a session needs, passed in explicitly.
//
// Nothing here is read from a package global or the environment at render time.
// That is what makes a second concurrent session -- an SSH connection -- possible
// without the two interfering.
type Deps struct {
	Agent   *agent.Agent
	Catalog *catalog.Catalog
	Config  *config.Config
	Logger  *slog.Logger

	// Alias and ProjectID describe the project this session edits.
	Alias     string
	ProjectID string

	// LogPath is shown by /log.
	LogPath string
}

// Model is the root Bubble Tea model.
type Model struct {
	deps  Deps
	theme Theme

	width, height int
	ready         bool

	conversation *Conversation
	viewport     viewport.Model
	input        textarea.Model
	spinner      spinner.Model
	help         help.Model
	keys         keyMap

	// sidebar shows project files and revisions.
	sidebar       sidebar
	showSidebar   bool
	showReasoning bool

	// Turn state.
	running    bool
	cancelTurn context.CancelFunc

	// A pending approval takes over the input area until answered.
	approval *approvalPrompt

	status      string
	statusError bool
	statusSeq   int

	quitting bool

	// sender delivers agent events into this program from the turn goroutine.
	sender *sender
}

// New builds the root model.
func New(deps Deps) Model {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}

	ta := textarea.New()
	ta.Placeholder = "Ask webshim to build something, or type /help"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return Model{
		deps:         deps,
		theme:        NewTheme(true), // replaced once the terminal reports back
		conversation: NewConversation(),
		input:        ta,
		spinner:      sp,
		help:         help.New(),
		keys:         defaultKeyMap(),
		showSidebar:  true,
		sidebar:      newSidebar(),
	}
}

// Init starts the program.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		// Ask the terminal what it looks like, rather than assuming. Over
		// SSH this is the only way to know.
		tea.RequestBackgroundColor,
		tea.RequestWindowSize,
		m.spinner.Tick,
		m.refreshProjectCmd(),
	)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.BackgroundColorMsg:
		// The terminal has told us what it looks like. Rebuild the theme and
		// the markdown renderer, since both were built from a guess.
		m.theme = NewTheme(msg.IsDark())
		m.conversation.SetRenderer(m.viewport.Width(), m.theme.IsDark())
		m.refreshViewport()
		return m, nil

	case tea.WindowSizeMsg:
		return m.resize(msg.Width, msg.Height)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case agentEventMsg:
		return m.handleAgentEvent(msg.Event)

	case permissionRequestMsg:
		m.approval = newApprovalPrompt(msg.Request, msg.Reply, m.theme)
		m.conversation.MarkAwaitingApproval(msg.Request.Tool)
		m.input.Blur()
		return m, nil

	case turnFinishedMsg:
		m.running = false
		m.cancelTurn = nil
		m.input.Focus()
		if msg.Err != nil && !errors.Is(msg.Err, context.Canceled) {
			m.conversation.AddError(msg.Err.Error())
		}
		m.refreshViewport()
		return m, m.refreshProjectCmd()

	case projectRefreshedMsg:
		m.sidebar.refresh(m.deps)
		return m, nil

	case statusMsg:
		m.status, m.statusError = msg.Text, msg.Error
		m.statusSeq++
		return m, clearStatusAfter(m.statusSeq)

	case clearStatusMsg:
		if msg.Seq == m.statusSeq {
			m.status, m.statusError = "", false
		}
		return m, nil
	}

	return m.forwardToFocused(msg)
}

// resize recomputes the layout.
func (m Model) resize(w, h int) (tea.Model, tea.Cmd) {
	m.width, m.height = w, h
	m.ready = true

	sidebarWidth := m.sidebarWidth()
	chatWidth := w - sidebarWidth
	if chatWidth < 20 {
		chatWidth = w
	}

	// Rows: chat area, input, status bar.
	inputHeight := m.input.Height() + 1
	statusHeight := 1
	chatHeight := h - inputHeight - statusHeight
	if chatHeight < 3 {
		chatHeight = 3
	}

	m.viewport.SetWidth(chatWidth)
	m.viewport.SetHeight(chatHeight)
	m.input.SetWidth(w - 2)
	m.help.SetWidth(w)

	m.conversation.SetRenderer(chatWidth-2, m.theme.IsDark())
	m.sidebar.setSize(sidebarWidth, chatHeight)

	m.refreshViewport()
	return m, nil
}

// sidebarWidth returns the sidebar's width, or zero when it is hidden or the
// terminal is too narrow to justify it.
func (m Model) sidebarWidth() int {
	if !m.showSidebar || m.width < 90 {
		return 0
	}
	w := m.width / 3
	if w > 40 {
		w = 40
	}
	if w < 24 {
		return 0
	}
	return w
}

// forwardToFocused routes a message to whichever component has focus.
func (m Model) forwardToFocused(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.approval != nil {
		cmd := m.approval.Update(msg)
		return m, cmd
	}

	var cmds []tea.Cmd
	var cmd tea.Cmd

	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// handleAgentEvent folds an event into the transcript.
func (m Model) handleAgentEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	m.conversation.Apply(ev)

	if _, ok := ev.(agent.StepEvent); ok {
		// Step events only change the status bar totals.
		return m, nil
	}
	m.refreshViewport()
	return m, nil
}

// refreshViewport re-renders the transcript and keeps the view pinned to the
// bottom while a turn is streaming.
func (m *Model) refreshViewport() {
	atBottom := m.viewport.AtBottom()
	m.viewport.SetContent(m.conversation.Render(m.theme, m.showReasoning))
	if atBottom || m.running {
		m.viewport.GotoBottom()
	}
}

// View renders the UI.
func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = m.windowTitle()

	if m.quitting {
		v.SetContent("")
		return v
	}
	if !m.ready {
		v.SetContent("starting webshim...")
		return v
	}

	chat := m.viewport.View()
	body := chat
	if w := m.sidebarWidth(); w > 0 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, chat, m.sidebar.view(m.theme))
	}

	bottom := m.input.View()
	if m.approval != nil {
		bottom = m.approval.View(m.width)
	}

	v.SetContent(lipgloss.JoinVertical(lipgloss.Left,
		body,
		bottom,
		m.statusBar(),
	))
	return v
}

func (m Model) windowTitle() string {
	if m.deps.Alias == "" {
		return "webshim"
	}
	return "webshim - " + m.deps.Alias
}

// statusBar renders the bottom line.
func (m Model) statusBar() string {
	gate := m.deps.Agent.Gate()
	session := m.deps.Agent.Session()

	left := m.theme.ModeBadge(gate.Mode())

	parts := []string{m.deps.Alias}
	if v := session.LiveVersion(); v > 0 {
		parts = append(parts, fmt.Sprintf("v%d", v))
	}
	parts = append(parts, m.deps.Agent.Describe())

	usage := session.Usage()
	if usage.TotalTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s tok", humanCount(usage.TotalTokens)))
		if cost := session.Cost(); cost > 0 {
			parts = append(parts, fmt.Sprintf("$%.4f", cost))
		}
	}

	middle := " " + strings.Join(parts, " · ")
	if m.running {
		middle = " " + m.spinner.View() + middle
	}

	right := "? help"
	if m.status != "" {
		right = m.status
		if m.statusError {
			right = m.theme.Error.Render(right)
		}
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(middle) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
	}
	return m.theme.StatusBar.Width(m.width).Render(
		left + middle + strings.Repeat(" ", gap) + right)
}

// humanCount abbreviates a token count.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// keyMap holds the global keybindings.
type keyMap struct {
	Send       key.Binding
	NewLine    key.Binding
	Cancel     key.Binding
	Quit       key.Binding
	Help       key.Binding
	Sidebar    key.Binding
	Reasoning  key.Binding
	ScrollUp   key.Binding
	ScrollDown key.Binding
	CycleMode  key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Send:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
		NewLine:    key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"), key.WithHelp("alt+enter", "newline")),
		Cancel:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "stop")),
		Quit:       key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
		Help:       key.NewBinding(key.WithKeys("ctrl+h"), key.WithHelp("ctrl+h", "help")),
		Sidebar:    key.NewBinding(key.WithKeys("ctrl+b"), key.WithHelp("ctrl+b", "sidebar")),
		Reasoning:  key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "reasoning")),
		ScrollUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "scroll up")),
		ScrollDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "scroll down")),
		CycleMode:  key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("ctrl+p", "mode")),
	}
}

// ShortHelp implements help.KeyMap.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Send, k.Cancel, k.Sidebar, k.CycleMode, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Send, k.NewLine, k.Cancel},
		{k.Sidebar, k.Reasoning, k.ScrollUp, k.ScrollDown},
		{k.CycleMode, k.Help, k.Quit},
	}
}

// cycleMode advances to the next permission mode.
func cycleMode(m permission.Mode) permission.Mode {
	for i, candidate := range permission.Modes {
		if candidate == m {
			return permission.Modes[(i+1)%len(permission.Modes)]
		}
	}
	return permission.ModeNormal
}
