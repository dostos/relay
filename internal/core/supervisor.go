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
	// RepairSensors refreshes tmux hooks once per live session whenever the
	// supervisor process starts, so binary upgrades cannot leave old hook
	// semantics installed indefinitely.
	RepairSensors func(context.Context, string) error

	mu      sync.Mutex
	running map[string]struct{}
	backoff map[string]time.Time
	started map[string]time.Time
	sensors map[string]struct{}
}

// watcherFlapWindow is how quickly a watcher must exit to count as flapping.
// A real watcher blocks on an event stream for minutes; one that returns almost
// immediately did not get to work — usually because another process holds the
// lock — and restarting it every tick is a spin, not supervision.
const watcherFlapWindow = 5 * time.Second

// watcherBackoff is how long to leave a flapping handoff alone. Long enough to
// stop the spin, short enough that a genuinely free handoff is picked up fast.
const watcherBackoff = 5 * time.Minute

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

// NeedsWatch lists non-terminal handoffs that have a boundary destination.
// Ordinary children need a parent; an intentional apex root needs its watcher
// too because its own asks/results route to the bound human authority surface.
func (s *SupervisorService) NeedsWatch() ([]*Handoff, error) {
	all, err := s.Reg.ListHandoffs()
	if err != nil {
		return nil, err
	}
	var out []*Handoff
	for _, ho := range all {
		if ho == nil || handoffTerminal(ho) {
			continue
		}
		effective, err := effectiveLiveHandoff(s.Reg, ho)
		if err != nil {
			continue
		}
		if effective.SourceSessionID == "" {
			sess, getErr := s.Reg.GetSession(effective.SessionID)
			if getErr != nil || sess.Labels[ApexLabel] != "true" {
				continue
			}
		}
		out = append(out, effective)
	}
	return out, nil
}

// Reconcile starts a watcher for every live handoff that lacks one. It returns
// how many it started. Watchers already running are left alone — the per-
// handoff flock means a duplicate would fail anyway, and a standalone
// `relay parent watch` keeps working alongside the supervisor.
func (s *SupervisorService) Reconcile(ctx context.Context) (int, error) {
	if s.RepairSensors != nil {
		sessions, err := s.Reg.ListSessions()
		if err != nil {
			return 0, err
		}
		for _, sess := range sessions {
			if sess == nil || sess.Persist.Kind == LocalPersistKind || (sess.Labels["agent"] == "" && sess.Labels["apex"] != "true") {
				continue
			}
			s.mu.Lock()
			_, repaired := s.sensors[sess.ID]
			s.mu.Unlock()
			if repaired {
				continue
			}
			if err := s.RepairSensors(ctx, sess.ID); err != nil {
				s.emit("sensor_repair_error", sess.ID, err)
				continue
			}
			s.mu.Lock()
			if s.sensors == nil {
				s.sensors = map[string]struct{}{}
			}
			s.sensors[sess.ID] = struct{}{}
			s.mu.Unlock()
			s.emit("sensors_repaired", sess.ID, nil)
		}
	}
	pending, err := s.NeedsWatch()
	if err != nil {
		return 0, err
	}
	// Durable parent envelopes have one periodic retry owner. Child event
	// replay only deduplicates; it never injects the same pending decision
	// again. A parent rebind may still trigger an immediate repair, while this
	// tick guarantees recovery even when no new child event arrives.
	parents := map[string]struct{}{}
	for _, ho := range pending {
		parentID := ho.SourceSessionID
		if parentID == "" {
			parentID = ho.SessionID
		}
		parents[parentID] = struct{}{}
	}
	for parentID := range parents {
		if _, deliveryErr := s.Parents.DeliverPending(ctx, parentID); deliveryErr != nil {
			s.emit("pending_delivery_error", parentID, deliveryErr)
		}
	}
	// A question nobody answers must reach someone who can. This is the same
	// tick because both are "the tree is not making progress" problems.
	if n, ageErr := s.Parents.ReportStaleEscalations(ctx, EscalationMaxHold()); ageErr != nil {
		s.emit("stale_report_error", "", ageErr)
	} else if n > 0 {
		s.emit("stalled_escalations_reported", "", nil)
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
		// Do NOT probe the flock here: acquiring it to test it races with real
		// watchers and can momentarily steal the lock being checked. Let Watch
		// arbitrate, and back off from anything that flaps.
		if s.backoff == nil {
			s.backoff = map[string]time.Time{}
		}
		if s.started == nil {
			s.started = map[string]time.Time{}
		}
		if until, ok := s.backoff[ho.ID]; ok && time.Now().Before(until) {
			s.mu.Unlock()
			continue
		}
		s.running[ho.ID] = struct{}{}
		s.started[ho.ID] = time.Now()
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
			startedAt := s.started[id]
			delete(s.running, id)
			if !startedAt.IsZero() && time.Since(startedAt) < watcherFlapWindow {
				// It never got to work. Something else owns this handoff, or it
				// cannot start; retrying every tick just burns cycles.
				s.backoff[id] = time.Now().Add(watcherBackoff)
			}
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
