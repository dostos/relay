package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// TransportFactory creates a Transport for a host id.
type TransportFactory func(hostID string) (ports.Transport, error)

// SessionService manages durable sessions via Transport + Persistence adapters.
type SessionService struct {
	Reg       *Registry
	Profiles  *ProfileService
	NewTransport TransportFactory
	Persist   ports.Persistence
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// CreateOpts configures session creation.
type CreateOpts struct {
	HostID    string
	Name      string // optional persist name; default derived
	RepoRef   string // local git root
	RemoteCWD string // optional override (skips path_map)
	Command   string // optional initial command; default interactive shell
	Labels    map[string]string
}

// Create creates a remote persistent session and registers it locally.
func (s *SessionService) Create(ctx context.Context, opts CreateOpts) (*Session, error) {
	if opts.HostID == "" {
		return nil, fmt.Errorf("host_id required")
	}
	profile, err := s.Profiles.Get(ctx, opts.HostID, false)
	if err != nil {
		return nil, err
	}
	cwd := opts.RemoteCWD
	if cwd == "" {
		if opts.RepoRef == "" {
			return nil, fmt.Errorf("repo_ref or remote_cwd required")
		}
		cwd, err = profile.ResolveRemoteCWD(opts.RepoRef)
		if err != nil {
			return nil, err
		}
	}
	name := opts.Name
	if name == "" {
		base := "sess"
		if opts.RepoRef != "" {
			base = filepath.Base(opts.RepoRef)
		}
		name = base + "-" + newID("s")[2:8]
	}
	safe, err := shellquote.SanitizeSessionName(name)
	if err != nil {
		return nil, err
	}
	name = safe
	t, err := s.NewTransport(opts.HostID)
	if err != nil {
		return nil, err
	}
	cmd := opts.Command
	if cmd == "" {
		cmd = "bash -l"
	}
	h, err := s.Persist.Create(ctx, t, name, cwd, cmd)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sess := &Session{
		ID:        newID("sess"),
		HostID:    opts.HostID,
		RemoteCWD: cwd,
		Persist:   h,
		RepoRef:   opts.RepoRef,
		Labels:    opts.Labels,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	RememberResume(sess)
	return sess, nil
}

// Adopt registers an already-running remote tmux session.
// Does not create a new tmux session; fails if the remote name is missing.
func (s *SessionService) Adopt(ctx context.Context, opts CreateOpts) (*Session, error) {
	if opts.HostID == "" {
		return nil, fmt.Errorf("host_id required")
	}
	if opts.Name == "" {
		return nil, fmt.Errorf("--name required (existing tmux session)")
	}
	safe, err := shellquote.SanitizeSessionName(opts.Name)
	if err != nil {
		return nil, err
	}
	profile, err := s.Profiles.Get(ctx, opts.HostID, false)
	if err != nil {
		return nil, err
	}
	cwd := opts.RemoteCWD
	if cwd == "" && opts.RepoRef != "" {
		cwd, err = profile.ResolveRemoteCWD(opts.RepoRef)
		if err != nil {
			return nil, err
		}
	}
	t, err := s.NewTransport(opts.HostID)
	if err != nil {
		return nil, err
	}
	h := ports.PersistHandle{Kind: s.Persist.Kind(), Name: safe}
	ok, err := s.Persist.Exists(ctx, t, h)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("tmux session %q not found on %s", safe, opts.HostID)
	}
	now := time.Now().UTC()
	labels := opts.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if _, has := labels["adopted"]; !has {
		labels["adopted"] = "existing"
	}
	sess := &Session{
		ID:        newID("sess"),
		HostID:    opts.HostID,
		RemoteCWD: cwd,
		Persist:   h,
		RepoRef:   opts.RepoRef,
		Labels:    labels,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	RememberResume(sess)
	return sess, nil
}

func (s *SessionService) transportFor(sess *Session) (ports.Transport, error) {
	return s.NewTransport(sess.HostID)
}

func (s *SessionService) Get(id string) (*Session, error) {
	return s.Reg.GetSession(id)
}

func (s *SessionService) List() ([]*Session, error) {
	return s.Reg.ListSessions()
}

func (s *SessionService) Capture(ctx context.Context, id string, lines int) (string, error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return "", err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 50
	}
	return s.Persist.Capture(ctx, t, sess.Persist, lines)
}

func (s *SessionService) Send(ctx context.Context, id, text string, enter bool) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return err
	}
	return s.Persist.Send(ctx, t, sess.Persist, text, enter)
}

func (s *SessionService) Exec(ctx context.Context, id, command string) (stdout, stderr string, err error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return "", "", err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return "", "", err
	}
	return t.Run(ctx, sess.RemoteCWD, command)
}

func (s *SessionService) Resize(ctx context.Context, id string) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return err
	}
	return s.Persist.Resize(ctx, t, sess.Persist)
}

func (s *SessionService) Attach(ctx context.Context, id string) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return err
	}
	cmd := s.Persist.AttachCommand(sess.Persist, sess.RemoteCWD)
	return t.Interactive(ctx, cmd)
}

func (s *SessionService) AttachCommand(id string) (string, error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return "", err
	}
	return s.Persist.AttachCommand(sess.Persist, sess.RemoteCWD), nil
}

func (s *SessionService) Destroy(ctx context.Context, id string, keepRemote bool) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	if !keepRemote {
		t, err := s.transportFor(sess)
		if err != nil {
			return err
		}
		_ = s.Persist.Destroy(ctx, t, sess.Persist)
		// Intentional teardown — cmux must not treat this as a reconnectable drop.
		MarkResumeCleaned(sess.Persist.Name, "destroyed")
	} else {
		// Local unbound; remote kept → disconnected/resumable.
		RememberResume(sess)
	}
	return s.Reg.DeleteSession(id)
}

// ReplaceCreate kills any existing remote session with opts.Name (and local
// registry rows for it), then Create. Used by ephemeral flows like auth login.
func (s *SessionService) ReplaceCreate(ctx context.Context, opts CreateOpts) (*Session, error) {
	if opts.Name != "" && opts.HostID != "" {
		_ = s.KillPersist(ctx, opts.HostID, opts.Name)
	}
	return s.Create(ctx, opts)
}

// KillPersist destroys a remote persist session by name and drops local rows.
func (s *SessionService) KillPersist(ctx context.Context, hostID, persistName string) error {
	if hostID == "" || persistName == "" {
		return fmt.Errorf("host and persist name required")
	}
	safe, err := shellquote.SanitizeSessionName(persistName)
	if err != nil {
		return err
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return err
	}
	h := ports.PersistHandle{Kind: s.Persist.Kind(), Name: safe}
	_ = s.Persist.Destroy(ctx, t, h)
	MarkResumeCleaned(safe, "replaced")
	list, err := s.Reg.ListSessions()
	if err != nil {
		return nil
	}
	for _, sess := range list {
		if sess.HostID == hostID && sess.Persist.Name == safe {
			_ = s.Reg.DeleteSession(sess.ID)
		}
	}
	return nil
}

// RemoteLiveness is the result of probing whether persist names actually have a
// live remote tmux. Alive[name]=true means the remote session exists;
// HostReached[host]=false means the host could not be reached (probe skipped,
// do not treat its names as dead).
type RemoteLiveness struct {
	Alive       map[string]bool
	HostReached map[string]bool
}

// ProbeRemoteTmux checks which persist names are actually alive on their hosts.
// It is storm-safe: ONE `tmux list-sessions` per host (reusing the ssh
// ControlMaster), never one probe per session, with bounded concurrency and a
// short timeout. Unreachable hosts are reported (HostReached=false), not
// guessed. This is what turns the optimistic registry `live` into ground truth.
func (s *SessionService) ProbeRemoteTmux(ctx context.Context, names []ResumeInfo) RemoteLiveness {
	byHost := map[string][]string{}
	for _, n := range names {
		if n.HostID == "" || n.PersistName == "" {
			continue
		}
		byHost[n.HostID] = append(byHost[n.HostID], n.PersistName)
	}
	res := RemoteLiveness{Alive: map[string]bool{}, HostReached: map[string]bool{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // cap concurrent hosts — IPS-safe
	for host, hostNames := range byHost {
		host, hostNames := host, hostNames
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			set, reached := s.tmuxSet(ctx, host)
			mu.Lock()
			defer mu.Unlock()
			res.HostReached[host] = reached
			if !reached {
				return
			}
			for _, n := range hostNames {
				if set[n] {
					res.Alive[n] = true
				}
			}
		}()
	}
	wg.Wait()
	return res
}

// tmuxSet returns live tmux session names on one host via the shared host probe
// (channels ignored here). reached=false means the host was unreachable.
func (s *SessionService) tmuxSet(ctx context.Context, hostID string) (map[string]bool, bool) {
	t, err := s.NewTransport(hostID)
	if err != nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	tmux, _, ok := probeHostState(cctx, t)
	return tmux, ok
}
