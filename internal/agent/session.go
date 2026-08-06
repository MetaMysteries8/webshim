package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"charm.land/fantasy"

	"github.com/MetaMysteries8/webshim/internal/catalog"
	"github.com/MetaMysteries8/webshim/internal/config"
	"github.com/MetaMysteries8/webshim/internal/mirror"
	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// Session is one conversation about one project.
//
// It owns the message history, the cached view of live project state, and the
// running token and cost totals. Everything is behind a mutex because the agent
// runs in its own goroutine while the UI reads this to render.
type Session struct {
	mu sync.RWMutex

	alias   string
	project config.Project
	mirror  *mirror.Mirror
	client  *websim.Client
	log     *slog.Logger
	model   catalog.Model

	// Cached live state, refreshed by Refresh.
	liveVersion int
	slug        string
	title       string
	liveAssets  []websim.Asset
	notes       []string

	messages   []fantasy.Message
	totalUsage fantasy.Usage
	steps      int
}

// SessionConfig builds a Session.
type SessionConfig struct {
	Alias   string
	Project config.Project
	Mirror  *mirror.Mirror
	Client  *websim.Client
	Logger  *slog.Logger
	Model   catalog.Model
}

// NewSession creates a session.
func NewSession(cfg SessionConfig) *Session {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Session{
		alias:   cfg.Alias,
		project: cfg.Project,
		mirror:  cfg.Mirror,
		client:  cfg.Client,
		log:     logger,
		model:   cfg.Model,
		slug:    cfg.Project.Slug,
	}
}

// ProjectID returns the project this session edits.
func (s *Session) ProjectID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.project.ID
}

// Alias returns the configured alias.
func (s *Session) Alias() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alias
}

// Slug returns the project slug, which comment endpoints need for their Referer.
func (s *Session) Slug() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.slug
}

// CommentScope builds the scope comment calls require.
func (s *Session) CommentScope() websim.CommentScope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return websim.CommentScope{ProjectID: s.project.ID, Slug: s.slug}
}

// Mirror returns the local working copy.
func (s *Session) Mirror() *mirror.Mirror {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mirror
}

// LiveVersion returns the last known live revision.
func (s *Session) LiveVersion() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.liveVersion
}

// Model returns the model this session is using.
func (s *Session) Model() catalog.Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

// SetModel switches models mid-session, for the /model command.
func (s *Session) SetModel(m catalog.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = m
}

// Usage returns the running token totals.
func (s *Session) Usage() fantasy.Usage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalUsage
}

// Cost estimates spend so far in US dollars, from the catalog's per-million
// token prices.
//
// It is an estimate: cached-token pricing is applied where the provider reports
// cache hits, but providers differ in how they attribute them.
func (s *Session) Cost() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const perMillion = 1_000_000.0
	c := s.model.Cost
	u := s.totalUsage

	cost := float64(u.InputTokens)*c.Input/perMillion +
		float64(u.OutputTokens)*c.Output/perMillion

	if c.CacheRead > 0 && u.CacheReadTokens > 0 {
		// Cached reads are cheaper, so charge them at the cache rate and
		// refund the difference already counted above.
		cost += float64(u.CacheReadTokens) * (c.CacheRead - c.Input) / perMillion
	}
	if c.CacheWrite > 0 && u.CacheCreationTokens > 0 {
		cost += float64(u.CacheCreationTokens) * c.CacheWrite / perMillion
	}
	if cost < 0 {
		return 0
	}
	return cost
}

// Steps returns how many agent steps have run.
func (s *Session) Steps() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.steps
}

// Messages returns a copy of the conversation history.
func (s *Session) Messages() []fantasy.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]fantasy.Message(nil), s.messages...)
}

// appendMessages records new history after a turn.
func (s *Session) appendMessages(msgs []fantasy.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msgs...)
}

// recordUsage accumulates token totals.
func (s *Session) recordUsage(u fantasy.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalUsage.InputTokens += u.InputTokens
	s.totalUsage.OutputTokens += u.OutputTokens
	s.totalUsage.TotalTokens += u.TotalTokens
	s.totalUsage.ReasoningTokens += u.ReasoningTokens
	s.totalUsage.CacheReadTokens += u.CacheReadTokens
	s.totalUsage.CacheCreationTokens += u.CacheCreationTokens
}

func (s *Session) recordStep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps++
}

// Reset clears the conversation while keeping the project and mirror, for the
// /clear command.
func (s *Session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = nil
	s.totalUsage = fantasy.Usage{}
	s.steps = 0
}

// Refresh re-reads live project state so the next turn's context is accurate.
//
// Failures are recorded as notes rather than returned: the agent should still be
// able to talk to the person and work on the mirror when WebSim is unreachable.
func (s *Session) Refresh(ctx context.Context) {
	projectID := s.ProjectID()
	if projectID == "" || s.client == nil {
		return
	}

	var (
		notes   []string
		version int
		assets  []websim.Asset
		slug    string
		title   string
	)

	project, err := s.client.GetProject(ctx, projectID)
	if err != nil {
		notes = append(notes, "Could not read live project state: "+s.client.Sanitize(err.Error()))
	} else {
		slug, title = project.Slug, project.Title
		if v, verr := project.RequireCurrentVersion(); verr != nil {
			notes = append(notes, "The project has no usable current_version: "+verr.Error())
		} else {
			version = v
			if a, aerr := s.client.ListAssets(ctx, projectID, v); aerr != nil {
				notes = append(notes, "Could not list live assets: "+s.client.Sanitize(aerr.Error()))
			} else {
				assets = a
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if version != 0 {
		s.liveVersion = version
		s.liveAssets = assets
	}
	if slug != "" {
		s.slug = slug
	}
	if title != "" {
		s.title = title
	}
	s.notes = notes
}

// Context builds the per-turn project context for the system prompt.
func (s *Session) Context(mode permission.Mode) ProjectContext {
	s.mu.RLock()
	pc := ProjectContext{
		Alias:       s.alias,
		ProjectID:   s.project.ID,
		Slug:        s.slug,
		Title:       s.title,
		LiveVersion: s.liveVersion,
		Mode:        mode,
		LiveAssets:  append([]websim.Asset(nil), s.liveAssets...),
		Notes:       append([]string(nil), s.notes...),
	}
	m := s.mirror
	s.mu.RUnlock()

	if m == nil {
		return pc
	}
	pc.MirrorDir = m.Dir

	if entries, err := m.List(); err == nil {
		pc.MirrorFiles = entries
	}
	if diff, _, err := m.Diff(); err == nil {
		pc.Diff = diff
	} else if errors.Is(err, mirror.ErrNotSynced) {
		pc.Notes = append(pc.Notes,
			"The mirror has not been synced yet, so local changes cannot be diffed. "+
				"Call websim_sync before publishing.")
	}
	return pc
}
