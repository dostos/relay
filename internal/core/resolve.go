package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// Session addressing (#72). Every session command accepts:
//
//   - a registry id (`sess-…`);
//   - `HOST/NAME`: the tmux session NAME on HOST. A registered session with
//     that host and name is used; otherwise, if tmux on HOST really has NAME,
//     an ad-hoc target is built for this process (not written to the
//     registry), so messaging works for panes Relay never created;
//   - a bare `NAME` when exactly one registered session has that tmux name.
//     With none, a tmux session of that name on this machine is used.
//
// An ambiguous NAME fails with the candidates; nothing guesses.

// AdhocLabel marks a session record that came from HOST/NAME resolution and
// is not in the registry.
const AdhocLabel = "adhoc"

// SessionCandidate is one match of an ambiguous or unknown session reference.
type SessionCandidate struct {
	ID     string `json:"id"`
	HostID string `json:"host_id"`
	Name   string `json:"name"`
	Ref    string `json:"ref"`
}

// AmbiguousSessionError lists every session a bare NAME matched.
type AmbiguousSessionError struct {
	Ref        string
	Candidates []SessionCandidate
}

func (e *AmbiguousSessionError) Error() string {
	refs := make([]string, 0, len(e.Candidates))
	for _, c := range e.Candidates {
		refs = append(refs, c.Ref)
	}
	return fmt.Sprintf("session %q is ambiguous (%d matches); use HOST/NAME: %s", e.Ref, len(e.Candidates), strings.Join(refs, ", "))
}

// SessionNotFoundError is an unresolvable reference, with near matches.
type SessionNotFoundError struct {
	Ref        string
	Detail     string
	Candidates []SessionCandidate
}

func (e *SessionNotFoundError) Error() string {
	msg := fmt.Sprintf("no session matches %q", e.Ref)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if len(e.Candidates) > 0 {
		refs := make([]string, 0, len(e.Candidates))
		for _, c := range e.Candidates {
			refs = append(refs, c.Ref)
		}
		msg += "; known: " + strings.Join(refs, ", ")
	}
	return msg + ". List sessions with `relay session list`"
}

// ErrNotRegistered is returned by registry-only commands for ad-hoc targets.
var ErrNotRegistered = errors.New("target is not a registered relay session")

func candidate(s *Session) SessionCandidate {
	return SessionCandidate{ID: s.ID, HostID: s.HostID, Name: s.Persist.Name, Ref: s.HostID + "/" + s.Persist.Name}
}

// lookup returns a registry session or an ad-hoc target resolved earlier in
// this process.
func (s *SessionService) lookup(id string) (*Session, error) {
	if v, ok := s.adhoc.Load(id); ok {
		return v.(*Session), nil
	}
	return s.Reg.GetSession(id)
}

// IsAdhoc reports whether sess came from HOST/NAME resolution.
func IsAdhoc(sess *Session) bool { return sess != nil && sess.Labels[AdhocLabel] == "true" }

// adhocID is a synthetic, path- and shell-safe id for HOST/NAME.
func adhocID(host, name string) string {
	safe := func(v string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
				return r
			}
			return '-'
		}, v)
	}
	id := "tmux:" + safe(host) + ":" + safe(name)
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}

// Resolve turns a session reference into a session record.
func (s *SessionService) Resolve(ctx context.Context, ref string) (*Session, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("session reference required (sess-ID, HOST/NAME or NAME)")
	}
	if sess, err := s.lookup(ref); err == nil {
		return sess, nil
	}
	list, _ := s.List()
	all := make([]SessionCandidate, 0, len(list))
	for _, sess := range list {
		all = append(all, candidate(sess))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Ref < all[j].Ref })
	if rest, ok := strings.CutPrefix(ref, "tmux:"); ok {
		// An ad-hoc id printed by an earlier invocation: tmux:HOST:NAME.
		if host, name, ok := strings.Cut(rest, ":"); ok && host != "" && name != "" {
			ref = host + "/" + name
		}
	}
	if strings.HasPrefix(ref, "sess-") {
		return nil, &SessionNotFoundError{Ref: ref, Detail: "id not in this registry", Candidates: limitCandidates(all)}
	}
	if host, name, ok := strings.Cut(ref, "/"); ok {
		if host == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid session reference %q (want HOST/NAME)", ref)
		}
		for _, sess := range list {
			if sess.Persist.Name == name && (sess.HostID == host || (isLocalAlias(host) && isLocalAlias(sess.HostID))) {
				return sess, nil
			}
		}
		return s.adhocTarget(ctx, host, name, all)
	}
	var hits []*Session
	for _, sess := range list {
		if sess.Persist.Name == ref {
			hits = append(hits, sess)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return s.adhocTarget(ctx, LocalHostID, ref, all)
	default:
		c := make([]SessionCandidate, 0, len(hits))
		for _, h := range hits {
			c = append(c, candidate(h))
		}
		sort.Slice(c, func(i, j int) bool { return c[i].Ref < c[j].Ref })
		return nil, &AmbiguousSessionError{Ref: ref, Candidates: c}
	}
}

func limitCandidates(c []SessionCandidate) []SessionCandidate {
	if len(c) > 20 {
		return c[:20]
	}
	return c
}

func isLocalAlias(host string) bool {
	if host == LocalHostID || host == "self" {
		return true
	}
	if local := LocalHostIDFromProfile(); local != "" && host == local {
		return true
	}
	return false
}

func (s *SessionService) adhocTarget(ctx context.Context, host, name string, known []SessionCandidate) (*Session, error) {
	ref := host + "/" + name
	if err := shellquote.ValidateSessionName(name); err != nil {
		return nil, &SessionNotFoundError{Ref: ref, Detail: err.Error(), Candidates: limitCandidates(known)}
	}
	if s.NewTransport == nil || s.Persist == nil {
		return nil, &SessionNotFoundError{Ref: ref, Candidates: limitCandidates(known)}
	}
	t, err := s.NewTransport(host)
	if err != nil {
		return nil, err
	}
	h := ports.PersistHandle{Kind: s.Persist.Kind(), Name: name}
	exists, err := s.Persist.Exists(ctx, t, h)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	if !exists {
		return nil, &SessionNotFoundError{Ref: ref, Detail: "no tmux session " + name + " on " + host, Candidates: limitCandidates(known)}
	}
	sess := &Session{ID: adhocID(host, name), HostID: host, Persist: h, Labels: map[string]string{AdhocLabel: "true"}}
	s.adhoc.Store(sess.ID, sess)
	return sess, nil
}

// DirectMode is set when the desktop bridge could not be reached and a
// messaging command runs on this host instead (#73). In that mode the
// session's own host id (RELAY_SESSION_HOST) is this machine.
var DirectMode bool

// IsThisHost reports whether hostID names the machine this process runs on.
func IsThisHost(hostID string) bool {
	if isLocalAlias(hostID) {
		return true
	}
	if DirectMode {
		if own := strings.TrimSpace(os.Getenv("RELAY_SESSION_HOST")); own != "" && own == hostID {
			return true
		}
	}
	return false
}
