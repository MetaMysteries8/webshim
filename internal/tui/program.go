package tui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/MetaMysteries8/webshim/internal/agent"
)

// sender delivers messages into a running program from another goroutine.
//
// It exists because the program handle is only available after the model is
// constructed, while the model is copied by value on every update. A pointer to
// this survives those copies, and the mutex makes it safe for the agent
// goroutine to use while the UI thread is running.
//
// Sends before a program is attached, or after it exits, are dropped rather than
// blocking. A UI that has gone away must not wedge the agent.
type sender struct {
	mu      sync.RWMutex
	program *tea.Program
}

func newSender() *sender { return &sender{} }

func (s *sender) attach(p *tea.Program) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = p
}

func (s *sender) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = nil
}

// Send delivers a message, or drops it if no program is attached.
func (s *sender) Send(msg tea.Msg) {
	s.mu.RLock()
	p := s.program
	s.mu.RUnlock()
	if p != nil {
		p.Send(msg)
	}
}

// Asker returns an agent.Asker that presents approval prompts in the UI.
//
// The tool's goroutine blocks on the reply channel while the person decides. The
// channel is buffered so that a cancelled turn cannot leave the UI blocked
// trying to hand back an answer nobody is waiting for.
func (s *sender) Asker() agent.Asker {
	return agent.AskerFunc(func(ctx context.Context, req agent.Request) (agent.Decision, error) {
		reply := make(chan agent.Decision, 1)
		s.Send(permissionRequestMsg{Request: req, Reply: reply})

		select {
		case decision := <-reply:
			return decision, nil
		case <-ctx.Done():
			return agent.Decision{}, ctx.Err()
		}
	})
}

// NewAsker builds an Asker paired with the sender a Model will use.
//
// The agent needs an Asker at construction time, but the program does not exist
// until Run. Creating the sender first and attaching the program later resolves
// that ordering.
func NewAsker() (*sender, agent.Asker) {
	s := newSender()
	return s, s.Asker()
}

// Run starts the interface and blocks until the person quits.
func Run(ctx context.Context, s *sender, deps Deps) error {
	model := New(deps)
	model.sender = s

	program := tea.NewProgram(model, tea.WithContext(ctx))
	s.attach(program)
	defer s.detach()

	_, err := program.Run()
	return err
}
