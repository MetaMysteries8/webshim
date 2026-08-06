package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

// ErrDenied is returned to a tool whose request the operator refused.
var ErrDenied = errors.New("the operator declined this action")

// Request is one approval request presented to the operator.
type Request struct {
	// ID is unique per request, so a UI can match a reply to its prompt.
	ID int

	// Tool is the tool name, e.g. "websim_publish".
	Tool string

	// Risk is what the action can affect.
	Risk permission.Risk

	// Summary is a one-line description, e.g. "publish 3 files to demo".
	Summary string

	// Detail is optional multi-line context: a diff, a file list, the exact
	// paths that would be deleted.
	Detail string
}

// Decision is the operator's answer.
type Decision struct {
	Approved bool

	// Reason is optional text from the operator explaining a refusal, which
	// is passed back to the model so it can adapt.
	Reason string
}

// Asker presents a request and returns the operator's decision. The TUI
// implements this by rendering a form; headless runs implement it by refusing or
// by auto-approving.
//
// An implementation must honor ctx: if the operator cancels, return ctx.Err().
type Asker interface {
	Ask(ctx context.Context, req Request) (Decision, error)
}

// AskerFunc adapts a function to Asker.
type AskerFunc func(ctx context.Context, req Request) (Decision, error)

func (f AskerFunc) Ask(ctx context.Context, req Request) (Decision, error) { return f(ctx, req) }

// Gate decides whether an action needs approval and obtains it.
type Gate struct {
	mu     sync.RWMutex
	mode   permission.Mode
	asker  Asker
	nextID int

	// alwaysAllow records tool names the operator approved for the rest of
	// the session.
	alwaysAllow map[string]bool
}

// NewGate builds a gate. A nil asker means every request that would need
// approval is denied, which is the correct default for a non-interactive run:
// there is nobody to ask.
func NewGate(mode permission.Mode, asker Asker) *Gate {
	return &Gate{
		mode:        mode,
		asker:       asker,
		alwaysAllow: map[string]bool{},
	}
}

// Mode returns the current mode.
func (g *Gate) Mode() permission.Mode {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode
}

// SetMode changes the mode mid-session, for the /mode command.
func (g *Gate) SetMode(m permission.Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = m
}

// AllowAlways approves a tool for the remainder of the session.
func (g *Gate) AllowAlways(tool string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.alwaysAllow[tool] = true
}

// Require obtains approval for an action, or returns an error explaining why it
// may not proceed.
//
// The returned error is meant to be handed back to the model as a tool result
// rather than crashing the run: a refusal is information, and the model can pick
// a different approach.
func (g *Gate) Require(ctx context.Context, req Request) error {
	g.mu.RLock()
	mode := g.mode
	allowed := g.alwaysAllow[req.Tool]
	g.mu.RUnlock()

	if allowed {
		return nil
	}
	if !permission.NeedsApproval(mode, req.Risk) {
		return nil
	}
	if g.asker == nil {
		return fmt.Errorf("%w: %s needs approval in %s mode, but this run is not interactive",
			ErrDenied, req.Tool, mode)
	}

	g.mu.Lock()
	g.nextID++
	req.ID = g.nextID
	g.mu.Unlock()

	decision, err := g.asker.Ask(ctx, req)
	if err != nil {
		return err
	}
	if !decision.Approved {
		if decision.Reason != "" {
			return fmt.Errorf("%w: %s", ErrDenied, decision.Reason)
		}
		return ErrDenied
	}
	return nil
}

// AutoApprove is an Asker that accepts everything. It exists for tests and for
// `--yes`-style non-interactive runs, where the operator has already consented
// out of band.
func AutoApprove() Asker {
	return AskerFunc(func(context.Context, Request) (Decision, error) {
		return Decision{Approved: true}, nil
	})
}

// DenyAll is an Asker that refuses everything, for dry runs and tests.
func DenyAll() Asker {
	return AskerFunc(func(_ context.Context, req Request) (Decision, error) {
		return Decision{Approved: false, Reason: "no operator is available to approve " + req.Tool}, nil
	})
}
