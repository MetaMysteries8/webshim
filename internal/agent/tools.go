package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// toolDef pairs a Fantasy tool with the risk class the gate uses.
type toolDef struct {
	Name string
	Risk permission.Risk
	Tool fantasy.AgentTool
}

// Deps is everything the toolbelt needs. It is explicit rather than global so
// each session can have its own, which is what makes serving over SSH possible
// later.
type Deps struct {
	Client  *websim.Client
	Gate    *Gate
	Session *Session
}

// registerTool builds a tool whose handler is wrapped with the permission gate
// and with error normalization.
//
// A refused or failed call returns a text error result rather than a Go error,
// because a Go error aborts the whole agent run. Handing the model the reason
// lets it adapt: pick a different path, ask the person a question, or stop.
func registerTool[TIn any](
	d *Deps,
	name, description string,
	risk permission.Risk,
	// summarize turns the tool input into the approval prompt. It runs before
	// the action, so it must not have side effects.
	summarize func(in TIn) (summary, detail string),
	run func(ctx context.Context, in TIn) (any, error),
) toolDef {
	handler := func(ctx context.Context, in TIn, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		summary, detail := "", ""
		if summarize != nil {
			summary, detail = summarize(in)
		}
		if summary == "" {
			summary = name
		}

		if err := d.Gate.Require(ctx, Request{
			Tool:    name,
			Risk:    risk,
			Summary: summary,
			Detail:  detail,
		}); err != nil {
			// A cancelled context is a real abort, not a tool-level refusal.
			if ctx.Err() != nil {
				return fantasy.ToolResponse{}, ctx.Err()
			}
			return fantasy.NewTextErrorResponse(d.sanitize(err.Error())), nil
		}

		result, err := run(ctx, in)
		if err != nil {
			if ctx.Err() != nil {
				return fantasy.ToolResponse{}, ctx.Err()
			}
			return fantasy.NewTextErrorResponse(d.explain(err)), nil
		}
		return jsonResponse(result)
	}

	return toolDef{
		Name: name,
		Risk: risk,
		Tool: fantasy.NewAgentTool(name, description, handler),
	}
}

// jsonResponse encodes a tool result. Structured output keeps the model from
// having to parse prose.
func jsonResponse(v any) (fantasy.ToolResponse, error) {
	if s, ok := v.(string); ok {
		return fantasy.NewTextResponse(s), nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fantasy.NewTextErrorResponse("encoding the tool result failed: " + err.Error()), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func (d *Deps) sanitize(s string) string {
	if d.Client == nil {
		return s
	}
	return d.Client.Sanitize(s)
}

// explain turns an error into something the model can act on.
//
// The client's sentinel errors carry a specific remedy each, and saying it
// plainly is the difference between the model recovering and the model retrying
// the same failing call.
func (d *Deps) explain(err error) string {
	msg := d.sanitize(err.Error())

	switch {
	case errors.Is(err, ErrDenied):
		return msg + "\nDo not retry this. Tell the person it was declined and ask how to proceed."

	case errors.Is(err, websim.ErrConcurrentModification):
		return msg + "\nSomeone else published while you were working. Call websim_sync to pull " +
			"the new live revision, re-apply your edits, then publish again."

	case errors.Is(err, websim.ErrNotPromoted):
		return msg + "\nNothing went live; the previous revision is still current. " +
			"Fix the cause above and try again."

	case errors.Is(err, websim.ErrUnauthorized), errors.Is(err, websim.ErrForbidden):
		return msg + "\nThis is an authentication or permission problem, not something you can " +
			"work around. Tell the person to run `websim-cli login` and stop."

	case errors.Is(err, websim.ErrUnsafePath):
		return msg + "\nChoose a different path. Do not try to reach outside the project."

	case errors.Is(err, websim.ErrNotFound):
		return msg + "\nRe-read the current state before guessing; the id, version, or path " +
			"may be wrong."

	case errors.Is(err, websim.ErrUnexpectedShape):
		return msg + "\nThe API returned something unexpected. Stop and report this to the " +
			"person rather than retrying."

	case errors.Is(err, websim.ErrDryRun):
		return msg + "\nThis is a dry run, so no changes were sent. Describe what you would " +
			"have done."

	case errors.Is(err, websim.ErrRateLimited):
		return msg + "\nYou are being rate limited. Wait before trying again, and do not " +
			"issue parallel calls."
	}
	return msg
}

// truncateForModel caps a string that is about to be handed to the model, so one
// large file cannot consume the whole context window.
func truncateForModel(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return s[:max], true
}

// describePaths renders a path list for an approval prompt.
func describePaths(prefix string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "%s %s\n", prefix, p)
	}
	return b.String()
}
