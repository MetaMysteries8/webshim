// Package agent runs the LLM loop that edits WebSim projects.
//
// It owns the Fantasy wiring, the toolbelt, and the permission gate, and it
// knows nothing about the terminal UI. Progress is reported through an Emit
// callback of typed events, which the TUI forwards into Bubble Tea messages and
// a headless run prints directly. That split is what lets the same agent serve a
// local terminal and, later, an SSH session.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"charm.land/fantasy"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

// DefaultMaxSteps bounds one turn. Without it a confused model can loop on a
// failing tool until the context window or the budget runs out.
const DefaultMaxSteps = 40

// Agent runs turns against a model with the WebSim toolbelt.
type Agent struct {
	model   fantasy.LanguageModel
	session *Session
	gate    *Gate
	deps    *Deps
	tools   []toolDef
	log     *slog.Logger

	maxSteps   int
	maxTokens  int64
	riskByTool map[string]permission.Risk
}

// Config builds an Agent.
type Config struct {
	Model   fantasy.LanguageModel
	Session *Session
	Gate    *Gate
	Client  clientIface
	Logger  *slog.Logger

	// MaxSteps bounds one turn. Zero means DefaultMaxSteps.
	MaxSteps int

	// MaxTokens optionally stops a turn once total usage passes a budget.
	MaxTokens int64
}

// clientIface is the subset of *websim.Client the agent needs. Declaring it
// here keeps Config readable; the concrete type is passed through to Deps.
type clientIface = interface {
	Sanitize(string) string
}

// New builds an agent.
func New(cfg Config) (*Agent, error) {
	if cfg.Model == nil {
		return nil, errors.New("agent: a language model is required")
	}
	if cfg.Session == nil {
		return nil, errors.New("agent: a session is required")
	}
	if cfg.Gate == nil {
		cfg.Gate = NewGate(permission.ModeManual, nil)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	deps := &Deps{
		Client:  cfg.Session.client,
		Gate:    cfg.Gate,
		Session: cfg.Session,
	}
	tools := buildTools(deps)

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	return &Agent{
		model:      cfg.Model,
		session:    cfg.Session,
		gate:       cfg.Gate,
		deps:       deps,
		tools:      tools,
		log:        logger,
		maxSteps:   maxSteps,
		maxTokens:  cfg.MaxTokens,
		riskByTool: riskByTool(tools),
	}, nil
}

// Session exposes the conversation state for the UI to render.
func (a *Agent) Session() *Session { return a.session }

// Gate exposes the permission gate so the UI can change modes.
func (a *Agent) Gate() *Gate { return a.gate }

// RiskOf reports a tool's risk class, for labelling in the UI.
func (a *Agent) RiskOf(tool string) permission.Risk { return a.riskByTool[tool] }

// ToolNames lists the registered tools.
func (a *Agent) ToolNames() []string {
	out := make([]string, 0, len(a.tools))
	for _, t := range a.tools {
		out = append(out, t.Name)
	}
	return out
}

// Run executes one turn: the person says something, the agent works, the agent
// replies.
//
// Progress arrives through emit as it happens. Run returns when the turn is
// over. A nil emit is allowed and means "report nothing".
func (a *Agent) Run(ctx context.Context, prompt string, emit Emit) (*fantasy.AgentResult, error) {
	if emit == nil {
		emit = func(Event) {}
	}

	// Refresh live state first so the system prompt describes reality rather
	// than what was true at startup.
	a.session.Refresh(ctx)

	systemPrompt, err := BuildSystemPrompt(a.session.Context(a.gate.Mode()))
	if err != nil {
		return nil, err
	}

	stopWhen := []fantasy.StopCondition{fantasy.StepCountIs(a.maxSteps)}
	if a.maxTokens > 0 {
		stopWhen = append(stopWhen, fantasy.MaxTokensUsed(a.maxTokens))
	}

	fa := fantasy.NewAgent(a.model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(fantasyTools(a.tools)...),
		fantasy.WithStopConditions(stopWhen...),
	)

	call := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: a.session.Messages(),
		StopWhen: stopWhen,
	}
	a.wireCallbacks(&call, emit)

	emit(TurnStartEvent{Prompt: prompt})

	result, err := fa.Stream(ctx, call)
	if err != nil {
		// A cancelled turn is the person pressing escape, not a failure.
		if ctx.Err() != nil {
			emit(TurnEndEvent{Cancelled: true})
			return nil, ctx.Err()
		}
		emit(ErrorEvent{Err: err})
		emit(TurnEndEvent{})
		return nil, err
	}

	if result != nil {
		if len(result.Steps) > 0 {
			last := result.Steps[len(result.Steps)-1]
			a.session.appendMessages(last.Messages)
		}
		emit(TurnEndEvent{
			Text:  result.Response.Content.Text(),
			Usage: result.TotalUsage,
		})
	} else {
		emit(TurnEndEvent{})
	}
	return result, nil
}

// wireCallbacks maps Fantasy's stream callbacks onto webshim events.
//
// Each callback returns an error that aborts the stream when non-nil. Only
// context cancellation does that here: a UI that cannot keep up must not kill
// the turn.
func (a *Agent) wireCallbacks(call *fantasy.AgentStreamCall, emit Emit) {
	checkCtx := func(ctx context.Context) error {
		if ctx == nil {
			return nil
		}
		return ctx.Err()
	}
	_ = checkCtx

	call.OnTextStart = func(id string) error {
		emit(TextStartEvent{ID: id})
		return nil
	}
	call.OnTextDelta = func(id, text string) error {
		emit(TextDeltaEvent{ID: id, Text: text})
		return nil
	}
	call.OnTextEnd = func(id string) error {
		emit(TextEndEvent{ID: id})
		return nil
	}

	call.OnReasoningStart = func(id string, _ fantasy.ReasoningContent) error {
		emit(ReasoningStartEvent{ID: id})
		return nil
	}
	call.OnReasoningDelta = func(id, text string) error {
		emit(ReasoningDeltaEvent{ID: id, Text: text})
		return nil
	}
	call.OnReasoningEnd = func(id string, _ fantasy.ReasoningContent) error {
		emit(ReasoningEndEvent{ID: id})
		return nil
	}

	call.OnToolInputStart = func(id, toolName string) error {
		emit(ToolStartEvent{ID: id, Tool: toolName, Risk: a.RiskOf(toolName)})
		return nil
	}
	call.OnToolInputDelta = func(id, delta string) error {
		emit(ToolInputDeltaEvent{ID: id, Delta: delta})
		return nil
	}
	call.OnToolCall = func(tc fantasy.ToolCallContent) error {
		emit(ToolCallEvent{
			ID:    tc.ToolCallID,
			Tool:  tc.ToolName,
			Input: tc.Input,
			Risk:  a.RiskOf(tc.ToolName),
		})
		return nil
	}
	call.OnToolResult = func(tr fantasy.ToolResultContent) error {
		ev := ToolResultEvent{ID: tr.ToolCallID, Tool: tr.ToolName}
		switch tr.Result.GetType() {
		case fantasy.ToolResultContentTypeError:
			ev.IsError = true
			if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result); ok && e.Error != nil {
				ev.Output = e.Error.Error()
			}
		default:
			if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
				ev.Output = txt.Text
				// A tool that failed reports it in the text, since a Go
				// error would abort the run.
				ev.IsError = looksLikeToolError(txt.Text)
			}
		}
		emit(ev)
		return nil
	}

	call.OnStepFinish = func(step fantasy.StepResult) error {
		a.session.recordStep()
		a.session.recordUsage(step.Usage)
		emit(StepEvent{
			Step:  a.session.Steps(),
			Usage: a.session.Usage(),
			Cost:  a.session.Cost(),
		})
		return nil
	}

	call.OnError = func(err error) {
		a.log.Error("agent error", "error", a.deps.sanitize(err.Error()))
		emit(ErrorEvent{Err: err})
	}
}

// looksLikeToolError reports whether a text tool result is one of our error
// responses. Fantasy delivers NewTextErrorResponse as text, so the distinction
// has to be recovered here for the UI to colour it correctly.
func looksLikeToolError(s string) bool {
	if s == "" {
		return false
	}
	for _, marker := range []string{
		"websim: ", "mirror: ", "the operator declined", "agent: ",
	} {
		if len(s) >= len(marker) && s[:len(marker)] == marker {
			return true
		}
	}
	return false
}

// Describe returns a one-line summary of the agent's configuration, for the
// status bar.
func (a *Agent) Describe() string {
	m := a.session.Model()
	return fmt.Sprintf("%s/%s", m.ProviderID, m.ID)
}
