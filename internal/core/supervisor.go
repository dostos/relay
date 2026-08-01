package core

import (
	"context"
	"sync"
	"syscall"
	"time"
)

// Watcher lifecycle used to be nobody's job. A watcher was spawned once when a
// handoff was created and never restarted: if it died — crash, SIGTERM, laptop
// sleep, an upgrade killing it — that handoff silently stopped routing
// escalations, and the only thing that ever restarted watchers was install.sh.
// A handoff with no watcher looks exactly like a quiet one, so the failure was
// invisible until someone noticed work had stalled for hours.
//
// The supervisor makes it relay's job: one long-lived process that reconciles
// live handoffs against running watchers and adopts anything unwatched.

// SupervisorReconcileInterval bounds how long a handoff can sit unwatched. It
// is deliberately short — the cost of a tick is one registry read.
const SupervisorReconcileInterval = 30 * time.Second

// SupervisorService keeps exactly one watcher running per live handoff.
type SupervisorService struct {
	Reg     *Registry
	Parents *ParentService

	// Interval overrides SupervisorReconcileInterval in tests.
	Interval time.Duration

	// OnEvent reports lifecycle transitions; nil is fine.
	OnEvent func(event, handoffID string, err error)

	mu      sync.Mutex
	running map[string]struct{}
}

func (s *SupervisorService) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return SupervisorReconcileInterval
}

func (s *SupervisorService) emit(event, handoffID string, err error) {
	if s.OnEvent != nil {
		s.OnEvent(event, handoffID, err)
	}
}

// Supervised reports the handoffs this supervisor currently has a watcher for.
func (s *SupervisorService) Supervised() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for id := range s.running {
		out = append(out, id)
	}
	return out
}

// WatcherRunning reports whether ANY process is currently watching this
// handoff. The per-handoff flock is the authority: a watcher holds it for its
// whole life, so a non-blocking probe answers the question without caring
// which process holds it — this supervisor, another one, or a standalone
// `relay parent watch`. A stale PID in the lock file proves nothing, which is
// exactly how a dead watcher used to masquerade as a live one.
func WatcherRunning(handoffID string) bool {
	lock, err := acquireParentWatchLock(handoffID)
	if err != nil {
		return true // someone holds it
	}
	// We took it, so nobody was watching. Release immediately.
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
	return false
}

// Unwatched lists live handoffs that currently have no watcher at all. This is
// the diagnostic the old architecture could not answer: an unwatched handoff
// is indistinguishable from a quiet one until work visibly stalls.
func (s *SupervisorService) Unwatched() ([]*Handoff, error) {
	pending, err := s.NeedsWatch()
	if err != nil {
		return nil, err
	}
	var out []*Handoff
	for _, ho := range pending {
		if !WatcherRunning(ho.ID) {
			out = append(out, ho)
		}
	}
	return out, nil
}

// NeedsWatch lists non-terminal handoffs that have a parent to escalate to.
// A handoff with no SourceSessionID has nowhere to route, and a terminal one
// is done — neither needs watching.
func (s *SupervisorService) NeedsWatch() ([]*Handoff, error) {
	all, err := s.Reg.ListHandoffs()
	if err != nil {
		return nil, err
	}
	var out []*Handoff
	for _, ho := range all {
		if ho == nil || ho.SourceSessionID == "" || handoffTerminal(ho) {
			continue
		}
		out = append(out, ho)
	}
	return out, nil
}

// Reconcile starts a watcher for every live handoff that lacks one. It returns
// how many it started. Watchers already running are left alone — the per-
// handoff flock means a duplicate would fail anyway, and a standalone
// `relay parent watch` keeps working alongside the supervisor.
func (s *SupervisorService) Reconcile(ctx context.Context) (int, error) {
	pending, err := s.NeedsWatch()
	if err != nil {
		return 0, err
	}
	started := 0
	for _, ho := range pending {
		s.mu.Lock()
		if s.running == nil {
			s.running = map[string]struct{}{}
		}
		if _, busy := s.running[ho.ID]; busy {
			s.mu.Unlock()
			continue
		}
		// Another process may already be watching this handoff; starting a
		// second one would just lose the flock race and burn a goroutine.
		if WatcherRunning(ho.ID) {
			s.mu.Unlock()
			continue
		}
		s.running[ho.ID] = struct{}{}
		s.mu.Unlock()

		started++
		id := ho.ID
		s.emit("watch_start", id, nil)
		go func() {
			// Watch returns nil when the handoff ends, and an error when the
			// lock is held elsewhere or the stream gives up. Either way the
			// slot frees, so the next tick re-adopts it if it is still live.
			err := s.Parents.Watch(ctx, id)
			s.mu.Lock()
			delete(s.running, id)
			s.mu.Unlock()
			s.emit("watch_end", id, err)
		}()
	}
	return started, nil
}

// Run reconciles until the context is cancelled.
func (s *SupervisorService) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	if _, err := s.Reconcile(ctx); err != nil {
		s.emit("reconcile_error", "", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := s.Reconcile(ctx); err != nil {
				s.emit("reconcile_error", "", err)
			}
		}
	}
}
