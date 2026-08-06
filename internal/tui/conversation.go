package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/MetaMysteries8/webshim/internal/agent"
	"github.com/MetaMysteries8/webshim/internal/permission"
)

// blockKind is what a transcript block represents.
type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockReasoning
	blockTool
	blockError
	blockNotice
)

// toolState is where a tool call has got to.
type toolState int

const (
	toolPending toolState = iota
	toolRunning
	toolOK
	toolFailed
	toolAwaitingApproval
)

// block is one entry in the transcript.
type block struct {
	kind blockKind
	id   string

	text strings.Builder

	// Tool fields, used when kind is blockTool.
	toolName  string
	toolInput strings.Builder
	toolOut   string
	toolState toolState
	risk      permission.Risk
}

// Conversation is the rendered transcript.
//
// It holds blocks rather than a single string so that a streaming delta appends
// to the right place, and so a tool card can change state after it was first
// drawn.
type Conversation struct {
	blocks []*block

	// index maps a stream id to its block, so deltas find their target
	// without scanning.
	index map[string]*block

	renderer *glamour.TermRenderer
	width    int
	dark     bool
	built    bool
}

// NewConversation builds an empty transcript.
func NewConversation() *Conversation {
	return &Conversation{index: map[string]*block{}}
}

// SetRenderer rebuilds the markdown renderer for a width and background.
//
// Glamour fixes its wrap width and style when the renderer is built, so both a
// resize and a background change mean a new one. The style is chosen from the
// background the terminal actually reported rather than guessed, which matters
// over SSH where the server's own environment says nothing useful.
//
// Failing to build a renderer is not fatal: the transcript falls back to plain
// text rather than showing nothing.
func (c *Conversation) SetRenderer(w int, dark bool) {
	if w <= 0 || (c.built && w == c.width && dark == c.dark) {
		return
	}
	c.width, c.dark, c.built = w, dark, true

	style := "light"
	if dark {
		style = "dark"
	}

	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		c.renderer = nil
		return
	}
	c.renderer = r
}

// Clear empties the transcript.
func (c *Conversation) Clear() {
	c.blocks = nil
	c.index = map[string]*block{}
}

// Empty reports whether anything has been said yet.
func (c *Conversation) Empty() bool { return len(c.blocks) == 0 }

// AddUser records what the person typed.
func (c *Conversation) AddUser(text string) {
	b := &block{kind: blockUser}
	b.text.WriteString(text)
	c.blocks = append(c.blocks, b)
}

// AddNotice records a UI-generated line, such as a slash command result.
func (c *Conversation) AddNotice(text string) {
	b := &block{kind: blockNotice}
	b.text.WriteString(text)
	c.blocks = append(c.blocks, b)
}

// AddError records an error.
func (c *Conversation) AddError(text string) {
	b := &block{kind: blockError}
	b.text.WriteString(text)
	c.blocks = append(c.blocks, b)
}

// Apply folds an agent event into the transcript.
func (c *Conversation) Apply(ev agent.Event) {
	switch e := ev.(type) {
	case agent.TextStartEvent:
		c.open(e.ID, blockAssistant)
	case agent.TextDeltaEvent:
		c.open(e.ID, blockAssistant).text.WriteString(e.Text)

	case agent.ReasoningStartEvent:
		c.open(e.ID, blockReasoning)
	case agent.ReasoningDeltaEvent:
		c.open(e.ID, blockReasoning).text.WriteString(e.Text)

	case agent.ToolStartEvent:
		b := c.open(e.ID, blockTool)
		b.toolName = e.Tool
		b.risk = e.Risk
		b.toolState = toolPending

	case agent.ToolInputDeltaEvent:
		c.open(e.ID, blockTool).toolInput.WriteString(e.Delta)

	case agent.ToolCallEvent:
		b := c.open(e.ID, blockTool)
		b.toolName = e.Tool
		b.risk = e.Risk
		if e.Input != "" {
			b.toolInput.Reset()
			b.toolInput.WriteString(e.Input)
		}
		b.toolState = toolRunning

	case agent.ToolResultEvent:
		b := c.open(e.ID, blockTool)
		if b.toolName == "" {
			b.toolName = e.Tool
		}
		b.toolOut = e.Output
		if e.IsError {
			b.toolState = toolFailed
		} else {
			b.toolState = toolOK
		}

	case agent.ErrorEvent:
		c.AddError(e.Err.Error())
	}
}

// MarkAwaitingApproval flags the most recent pending call of a tool, so the
// transcript shows why the agent has paused.
func (c *Conversation) MarkAwaitingApproval(tool string) {
	for i := len(c.blocks) - 1; i >= 0; i-- {
		b := c.blocks[i]
		if b.kind == blockTool && b.toolName == tool &&
			(b.toolState == toolPending || b.toolState == toolRunning) {
			b.toolState = toolAwaitingApproval
			return
		}
	}
}

// ResumeAfterApproval returns a tool card to the running state.
func (c *Conversation) ResumeAfterApproval(tool string) {
	for i := len(c.blocks) - 1; i >= 0; i-- {
		b := c.blocks[i]
		if b.kind == blockTool && b.toolName == tool && b.toolState == toolAwaitingApproval {
			b.toolState = toolRunning
			return
		}
	}
}

// open finds or creates the block for a stream id.
func (c *Conversation) open(id string, kind blockKind) *block {
	if b, ok := c.index[id]; ok && b.kind == kind {
		return b
	}
	b := &block{kind: kind, id: id}
	c.index[id] = b
	c.blocks = append(c.blocks, b)
	return b
}

// Render draws the whole transcript.
func (c *Conversation) Render(t Theme, showReasoning bool) string {
	var out []string
	for _, b := range c.blocks {
		s := c.renderBlock(b, t, showReasoning)
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

func (c *Conversation) renderBlock(b *block, t Theme, showReasoning bool) string {
	switch b.kind {
	case blockUser:
		return t.UserLabel.Render("you") + "\n" + t.Base.Render(b.text.String())

	case blockAssistant:
		body := b.text.String()
		if strings.TrimSpace(body) == "" {
			return ""
		}
		return t.AssistantLabel.Render("webshim") + "\n" + c.markdown(body)

	case blockReasoning:
		if !showReasoning {
			return ""
		}
		body := strings.TrimSpace(b.text.String())
		if body == "" {
			return ""
		}
		return t.Reasoning.Render("thinking\n" + body)

	case blockError:
		return t.Error.Render("error") + "\n" + t.Base.Render(b.text.String())

	case blockNotice:
		return t.Faint.Render(b.text.String())

	case blockTool:
		return c.renderTool(b, t)
	}
	return ""
}

// renderTool draws a tool card.
func (c *Conversation) renderTool(b *block, t Theme) string {
	var (
		marker string
		style  lipgloss.Style
	)
	switch b.toolState {
	case toolOK:
		marker, style = "✓", t.ToolOK
	case toolFailed:
		marker, style = "✗", t.ToolError
	case toolAwaitingApproval:
		marker, style = "?", t.ToolRunning
	default:
		marker, style = "•", t.ToolRunning
	}

	header := fmt.Sprintf("%s %s", marker, b.toolName)
	if args := summarizeToolInput(b.toolInput.String()); args != "" {
		header += " " + args
	}
	line := style.Render(header)

	if b.toolState == toolAwaitingApproval {
		return line + "\n" + t.Faint.Render("  waiting for your approval")
	}
	if b.toolOut == "" {
		return line
	}

	out := summarizeToolOutput(b.toolOut)
	if out == "" {
		return line
	}
	detail := t.Faint
	if b.toolState == toolFailed {
		detail = t.ToolError
	}
	return line + "\n" + detail.Render("  "+out)
}

// markdown renders assistant text, falling back to plain text when Glamour is
// unavailable or fails.
func (c *Conversation) markdown(s string) string {
	if c.renderer == nil {
		return s
	}
	out, err := c.renderer.Render(s)
	if err != nil {
		return s
	}
	// Glamour pads with leading and trailing blank lines; the transcript
	// already spaces blocks apart.
	return strings.Trim(out, "\n")
}

// summarizeToolInput renders a compact argument preview.
//
// Tool inputs are JSON, and a raw dump of an index.html write would fill the
// screen. Showing the interesting scalar fields keeps the card to one line.
func summarizeToolInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}

	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return truncate(strings.Join(strings.Fields(raw), " "), 60)
	}

	// Path and version are the fields worth seeing at a glance.
	for _, key := range []string{"path", "version", "comment_id", "slug", "title"} {
		if v, ok := fields[key]; ok {
			return truncate(fmt.Sprintf("%v", v), 60)
		}
	}
	if v, ok := fields["description"]; ok {
		return truncate(fmt.Sprintf("%v", v), 60)
	}
	return ""
}

// summarizeToolOutput renders a compact result preview.
func summarizeToolOutput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return truncate(firstLine(raw), 100)
	}

	// A publish result is the one worth spelling out.
	if prev, ok := fields["previous_version"]; ok {
		if cur, ok := fields["current_version"]; ok {
			return fmt.Sprintf("published v%v -> v%v", prev, cur)
		}
	}
	if note, ok := fields["note"].(string); ok {
		return truncate(note, 100)
	}
	if files, ok := fields["files"].([]any); ok {
		return fmt.Sprintf("%d file(s)", len(files))
	}
	if summary, ok := fields["summary"].(string); ok {
		return truncate(summary, 100)
	}
	return truncate(firstLine(raw), 100)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if lipgloss.Width(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) > max {
		runes = runes[:max]
	}
	return string(runes) + "..."
}
