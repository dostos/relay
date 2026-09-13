package core

import (
	"context"
	"sort"
	"sync"
	"time"
)

// A presenter (Forge) wants one answer per session: is the agent in that pane
// working, blocked at a gate only a human may answer, absent, or unknowable.
// The classifier already exists (ClassifyAgentPane); what a presenter needs is
// a way to ask it. Two paths, one owner: for a pane the presenter is attached
// to it already holds the text and pipes it through `relay pane classify`;
// for a put-away session it asks `relay session readiness`, which captures
// the pane over the wire first. Both return a ReadinessReport.

// DefaultReadinessLines is how much pane tail is captured before classifying.
// ClassifyAgentPane looks at the last 25 non-trailing lines; 40 leaves room
// for blank lines and a gate's context above its prompt.
const DefaultReadinessLines = 40

// ReadinessTimeout bounds one capture, so a host that has gone away costs a
// caller seconds, never a hang. It applies per session, not per list.
const ReadinessTimeout = 8 * time.Second

// readinessHostFanout is how many hosts are captured at once by
// ListWithReadiness. Sessions on one host are always sequential. The fleet's
// ssh burst limit is the campus IPS (docs/fleet-roles.md: never more than 4
// concurrent ssh from one source), so this stays well under it.
const readinessHostFanout = 2

// ReadinessReport is the classifier's answer plus where and when it was
// sampled, which is what lets a presenter show "blocked (3s ago)" rather than
// a bare state.
type ReadinessReport struct {
	AgentReadiness
	Lines     int       `json:"lines"`
	SampledAt time.Time `json:"sampled_at"`
}

// SessionReadiness is a session record with its readiness attached, the shape
// `session list --readiness` and `session get --readiness` print. The session
// fields are promoted, so without the flag the record is byte-identical to a
// plain Session.
type SessionReadiness struct {
	*Session
	Readiness *ReadinessReport `json:"readiness,omitempty"`
}

// ClassifyText is `pane classify`: the classifier over text the caller holds.
// Empty text is `unknown`, not `absent`: an attached surface that handed over
// nothing is a presenter that has not read its screen yet, which is a
// different fact from a pane that is genuinely empty.
func ClassifyText(text string, lines int) ReadinessReport {
	if lines <= 0 {
		lines = DefaultReadinessLines
	}
	rep := ReadinessReport{Lines: lines, SampledAt: time.Now().UTC()}
	if len(text) == 0 {
		rep.AgentReadiness = AgentReadiness{State: AgentUnknown, Reason: "no text"}
		return rep
	}
	rep.AgentReadiness = ClassifyAgentPane(text)
	return rep
}

// Readiness captures one session's pane tail and classifies it. The capture
// is bounded by ReadinessTimeout; a transport failure is returned as an error
// alongside a report whose state is unknown, so a caller can print both.
func (s *SessionService) Readiness(ctx context.Context, id string, lines int) (*Session, *ReadinessReport, error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return nil, nil, err
	}
	rep := s.readinessOf(ctx, sess, lines)
	if rep.State == AgentUnknown && rep.Reason != "" && rep.Reason != "no text" {
		return sess, &rep, &readinessError{reason: rep.Reason}
	}
	return sess, &rep, nil
}

type readinessError struct{ reason string }

func (e *readinessError) Error() string { return e.reason }

// readinessOf never fails: a capture that cannot be made yields unknown with
// the error as its reason. That is the contract ListWithReadiness relies on —
// one dead host must not fail the list.
func (s *SessionService) readinessOf(ctx context.Context, sess *Session, lines int) ReadinessReport {
	if lines <= 0 {
		lines = DefaultReadinessLines
	}
	cctx, cancel := context.WithTimeout(ctx, ReadinessTimeout)
	defer cancel()
	t, err := s.transportFor(sess)
	if err != nil {
		return ReadinessReport{AgentReadiness: AgentReadiness{State: AgentUnknown, Reason: err.Error()}, Lines: lines, SampledAt: time.Now().UTC()}
	}
	text, err := s.Persist.Capture(cctx, t, sess.Persist, lines)
	if err != nil {
		if cctx.Err() != nil {
			err = cctx.Err()
		}
		return ReadinessReport{AgentReadiness: AgentReadiness{State: AgentUnknown, Reason: "capture failed: " + err.Error()}, Lines: lines, SampledAt: time.Now().UTC()}
	}
	rep := ReadinessReport{AgentReadiness: ClassifyAgentPane(text), Lines: lines, SampledAt: time.Now().UTC()}
	return rep
}

// ListWithReadiness is `session list --readiness`: every session, each with
// a report. Sessions on the same host are probed one after another; at most
// readinessHostFanout hosts are probed at once. Order of the result matches
// List(). Nothing here fails the list.
func (s *SessionService) ListWithReadiness(ctx context.Context, lines int) ([]SessionReadiness, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]SessionReadiness, len(list))
	byHost := map[string][]int{}
	for i, sess := range list {
		out[i].Session = sess
		byHost[sess.HostID] = append(byHost[sess.HostID], i)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	sem := make(chan struct{}, readinessHostFanout)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, h := range hosts {
		idx := byHost[h]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			for _, i := range idx {
				rep := s.readinessOf(ctx, out[i].Session, lines)
				mu.Lock()
				out[i].Readiness = &rep
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out, nil
}
