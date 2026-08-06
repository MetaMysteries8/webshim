package websim

import (
	"context"
	"sync"
)

// lockSet holds one mutex per project ID.
//
// The playbook requires the critical section to span the entire mutation flow
// -- from reading current_version through post-promotion verification -- not
// just the upload step. Client.WithProjectLock is the only sanctioned way to
// enter that section.
//
// This serializes flows within a single process. It does not coordinate across
// processes or hosts; the playbook's warning about external locks still applies
// when more than one agent can touch the same project.
type lockSet struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func newLockSet() *lockSet {
	return &lockSet{m: make(map[string]chan struct{})}
}

// acquire blocks until the lock for key is held or ctx is done. The returned
// release function must be called exactly once.
func (l *lockSet) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	ch, ok := l.m[key]
	if !ok {
		ch = make(chan struct{}, 1)
		l.m[key] = ch
	}
	l.mu.Unlock()

	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WithProjectLock runs fn while holding the per-project lock.
func (c *Client) WithProjectLock(ctx context.Context, projectID string, fn func(context.Context) error) error {
	release, err := c.locks.acquire(ctx, projectID)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}
