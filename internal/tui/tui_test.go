package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/MetaMysteries8/webshim/internal/agent"
	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/mirror"
	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// stubModel satisfies fantasy.LanguageModel without talking to anything. The
// UI never calls it; it only needs to exist so an Agent can be constructed.
type stubModel struct{}

func (stubModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{}, nil
}
func (stubModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, nil
}
func (stubModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return &fantasy.ObjectResponse{}, nil
}
func (stubModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, nil
}
func (stubModel) Provider() string { return "stub" }
func (stubModel) Model() string    { return "stub-1" }

// newTestModel builds a Model wired to a real Agent over stub infrastructure.
func newTestModel(t *testing.T, mode permission.Mode) Model {
	t.Helper()

	m, err := mirror.Open(filepath.Join(t.TempDir(), "proj"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	client, err := websim.New(websim.Options{Token: websim.Token("test-token-abcdefghijkl")})
	if err != nil {
		t.Fatal(err)
	}

	session := agent.NewSession(agent.SessionConfig{
		Alias:   "demo",
		Project: config.Project{ID: "proj_1", Slug: "demo"},
		Mirror:  m,
		Client:  client,
		Model:   catalog.Model{ID: "test-model", ProviderID: "hyper"},
	})

	ag, err := agent.New(agent.Config{
		Model:   stubModel{},
		Session: session,
		Gate:    agent.NewGate(mode, agent.AutoApprove()),
	})
	if err != nil {
		t.Fatal(err)
	}

	model := New(Deps{Agent: ag, Alias: "demo", ProjectID: "proj_1", LogPath: "/tmp/webshim.log"})
	model.sender = newSender()
	return model
}

// render drives a model through a resize and returns its view with styling
// removed.
//
// Assertions run against the plain text because Glamour emits a colour escape
// between words, so a styled frame contains "Editing" and "stylesheet" but not
// the substring "Editing the stylesheet".
func render(t *testing.T, m Model, msgs ...tea.Msg) (Model, string) {
	t.Helper()

	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(Model)
	for _, msg := range msgs {
		next, _ = m.Update(msg)
		m = next.(Model)
	}
	return m, plain(m.View().Content)
}

// plain strips ANSI styling.
func plain(s string) string { return ansi.Strip(s) }

// transcript renders the whole conversation, ignoring the viewport's crop.
// Use it when an assertion is about content rather than what is on screen.
func transcript(m Model) string {
	return plain(m.conversation.Render(m.theme, m.showReasoning))
}

func TestViewShowsProjectAndMode(t *testing.T) {
	t.Parallel()

	m, out := render(t, newTestModel(t, permission.ModeNormal))
	_ = m

	for _, want := range []string{"NORMAL", "demo", "hyper/test-model"} {
		if !strings.Contains(out, want) {
			t.Errorf("the status bar should show %q:\n%s", want, out)
		}
	}
}

// TestYoloModeIsVisuallyLoud: in yolo mode the agent changes a live site
// without asking, so the badge has to be impossible to miss.
func TestYoloModeIsVisuallyLoud(t *testing.T) {
	t.Parallel()

	_, out := render(t, newTestModel(t, permission.ModeYOLO))
	if !strings.Contains(out, "YOLO") {
		t.Errorf("the yolo badge is missing:\n%s", out)
	}
}

func TestViewIsAltScreenAndTitled(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := next.(Model).View()

	if !v.AltScreen {
		t.Error("the TUI should use the alternate screen")
	}
	if v.WindowTitle != "webshim - demo" {
		t.Errorf("window title = %q", v.WindowTitle)
	}
}

// TestStreamingEventsReachTheTranscript walks a whole turn's worth of events.
func TestStreamingEventsReachTheTranscript(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	m.conversation.AddUser("make the heading blue")

	_, out := render(t, m,
		agentEventMsg{Event: agent.TextStartEvent{ID: "t1"}},
		agentEventMsg{Event: agent.TextDeltaEvent{ID: "t1", Text: "Editing the stylesheet."}},
		agentEventMsg{Event: agent.ToolStartEvent{ID: "c1", Tool: "mirror_write", Risk: permission.RiskEdit}},
		agentEventMsg{Event: agent.ToolCallEvent{ID: "c1", Tool: "mirror_write",
			Input: `{"path":"style.css","content":"h1{color:blue}"}`}},
		agentEventMsg{Event: agent.ToolResultEvent{ID: "c1", Tool: "mirror_write",
			Output: `{"path":"style.css","note":"Written locally."}`}},
	)

	for _, want := range []string{"you", "make the heading blue", "Editing the stylesheet", "mirror_write", "style.css"} {
		if !strings.Contains(out, want) {
			t.Errorf("the transcript should contain %q:\n%s", want, out)
		}
	}
	// The file content itself must not be dumped into the tool card.
	if strings.Contains(out, "h1{color:blue}") {
		t.Errorf("the tool card should summarize, not dump file content:\n%s", out)
	}
}

func TestToolFailureIsMarkedDistinctly(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	_, out := render(t, m,
		agentEventMsg{Event: agent.ToolCallEvent{ID: "c1", Tool: "websim_publish"}},
		agentEventMsg{Event: agent.ToolResultEvent{ID: "c1", Tool: "websim_publish",
			Output: "websim: revision was not promoted", IsError: true}},
	)
	if !strings.Contains(out, "✗") {
		t.Errorf("a failed tool should be marked:\n%s", out)
	}
}

// TestReasoningIsHiddenByDefault keeps the transcript readable while leaving the
// detail one keystroke away.
func TestReasoningIsHiddenByDefault(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	m, out := render(t, m,
		agentEventMsg{Event: agent.ReasoningDeltaEvent{ID: "r1", Text: "considering the options"}},
	)
	if strings.Contains(out, "considering the options") {
		t.Errorf("reasoning should be hidden by default:\n%s", out)
	}

	next, _ := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	out = next.(Model).View().Content
	if !strings.Contains(out, "considering the options") {
		t.Errorf("ctrl+r should reveal reasoning:\n%s", out)
	}
}

// TestApprovalPromptBlocksAndAnswers is the whole permission UX: the prompt
// appears, shows what is at stake, and the answer reaches the waiting tool.
func TestApprovalPromptBlocksAndAnswers(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	reply := make(chan agent.Decision, 1)

	m, out := render(t, m, permissionRequestMsg{
		Request: agent.Request{
			Tool:    "websim_publish",
			Risk:    permission.RiskCommand,
			Summary: "publish to demo: 1 modified (v11 -> new revision)",
			Detail:  "  ~ index.html\n",
		},
		Reply: reply,
	})

	for _, want := range []string{"publish to demo", "index.html", "approve", "decline", "always allow"} {
		if !strings.Contains(out, want) {
			t.Errorf("the approval prompt should show %q:\n%s", want, out)
		}
	}

	next, _ := m.Update(tea.KeyPressMsg{Code: 'y'})
	m = next.(Model)

	select {
	case d := <-reply:
		if !d.Approved {
			t.Error("pressing y should approve")
		}
	default:
		t.Fatal("the decision never reached the waiting tool")
	}
	if m.approval != nil {
		t.Error("the prompt should close after answering")
	}
}

// TestApprovalDefaultsToDeclining: anything that is not an explicit yes must
// decline, because the alternative is changing a live site by accident.
func TestApprovalDefaultsToDeclining(t *testing.T) {
	t.Parallel()

	for _, k := range []tea.KeyPressMsg{
		{Code: 'n'},
		{Code: tea.KeyEscape},
	} {
		m := newTestModel(t, permission.ModeNormal)
		reply := make(chan agent.Decision, 1)
		m, _ = render(t, m, permissionRequestMsg{
			Request: agent.Request{Tool: "websim_publish", Risk: permission.RiskCommand, Summary: "publish"},
			Reply:   reply,
		})

		next, _ := m.Update(k)
		_ = next

		select {
		case d := <-reply:
			if d.Approved {
				t.Errorf("key %v should have declined", k)
			}
		default:
			t.Errorf("key %v produced no decision", k)
		}
	}
}

func TestCycleModeWrapsAround(t *testing.T) {
	t.Parallel()

	seen := map[permission.Mode]bool{}
	m := permission.ModeManual
	for range len(permission.Modes) {
		seen[m] = true
		m = cycleMode(m)
	}
	if len(seen) != len(permission.Modes) {
		t.Errorf("cycling visited %v, want all of %v", seen, permission.Modes)
	}
	if m != permission.ModeManual {
		t.Errorf("cycling should wrap back to the start, got %q", m)
	}
}

func TestSlashHelpIsShownWithoutCallingTheAgent(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	m, _ = render(t, m)
	m.input.SetValue("/help")

	next, _ := m.submit()
	after := next.(Model)

	// The help text is longer than the viewport, so assert on the transcript
	// rather than on whichever part happens to be scrolled into view.
	out := transcript(after)
	if !strings.Contains(out, "/publish") || !strings.Contains(out, "yolo") {
		t.Errorf("/help should list commands and modes:\n%s", out)
	}
	if after.running {
		t.Error("/help must not start an agent turn")
	}
}

func TestUnknownSlashCommandIsReported(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	m, _ = render(t, m)
	m.input.SetValue("/nonsense")

	next, cmd := m.submit()
	if next.(Model).running {
		t.Error("an unknown command must not start a turn")
	}
	if cmd == nil {
		t.Fatal("expected a status message")
	}
	msg, ok := cmd().(statusMsg)
	if !ok || !msg.Error || !strings.Contains(msg.Text, "nonsense") {
		t.Errorf("expected an error status naming the command, got %+v", msg)
	}
}

func TestNarrowTerminalHidesTheSidebar(t *testing.T) {
	t.Parallel()

	m := newTestModel(t, permission.ModeNormal)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	if w := next.(Model).sidebarWidth(); w != 0 {
		t.Errorf("a 70-column terminal should not get a sidebar, got width %d", w)
	}

	next, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if w := next.(Model).sidebarWidth(); w == 0 {
		t.Error("a wide terminal should get a sidebar")
	}
}

func TestSummarizeToolOutputHighlightsPublishResults(t *testing.T) {
	t.Parallel()

	got := summarizeToolOutput(`{"ok":true,"previous_version":11,"current_version":12}`)
	if !strings.Contains(got, "v11") || !strings.Contains(got, "v12") {
		t.Errorf("a publish result should show both versions, got %q", got)
	}

	if got := summarizeToolOutput(`{"files":[{"path":"a"},{"path":"b"}]}`); !strings.Contains(got, "2 file") {
		t.Errorf("a file listing should be counted, got %q", got)
	}
	if got := summarizeToolOutput("not json at all"); got != "not json at all" {
		t.Errorf("plain text should pass through, got %q", got)
	}
}

func TestHumanCount(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{999: "999", 1500: "1.5k", 2_400_000: "2.4M"}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
}
