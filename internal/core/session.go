package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// TransportFactory creates a Transport for a host id.
type TransportFactory func(hostID string) (ports.Transport, error)

// SessionService manages durable sessions via Transport + Persistence adapters.
type SessionService struct {
	Reg          *Registry
	Profiles     *ProfileService
	NewTransport TransportFactory
	Persist      ports.Persistence
}

func (s *SessionService) applyChrome(ctx context.Context, t ports.Transport, h ports.PersistHandle) {
	if chrome, ok := s.Persist.(ports.SessionChrome); ok {
		_ = chrome.ApplyChrome(ctx, t, h)
	}
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// CreateOpts configures session creation.
type CreateOpts struct {
	HostID             string
	Name               string // optional persist name; default derived
	RepoRef            string // local git root
	RemoteCWD          string // optional override (skips path_map)
	Command            string // optional initial command; default interactive shell
	Labels             map[string]string
	SourceSessionID    string
	SourceHostID       string
	SourcePersistName  string
	CreatedByHandoffID string
}

// OpenNamed returns the existing host/name session or creates it. A remote
// tmux session that predates the local registry is adopted without replacing
// it, preserving the user's work.
func (s *SessionService) OpenNamed(ctx context.Context, opts CreateOpts) (*Session, bool, error) {
	if opts.HostID == "" || opts.Name == "" {
		return nil, false, fmt.Errorf("host and name required")
	}
	safe, err := shellquote.SanitizeSessionName(opts.Name)
	if err != nil {
		return nil, false, err
	}
	opts.Name = safe
	t, err := s.NewTransport(opts.HostID)
	if err != nil {
		return nil, false, err
	}
	handle := ports.PersistHandle{Kind: s.Persist.Kind(), Name: safe}
	exists, err := s.Persist.Exists(ctx, t, handle)
	if err != nil {
		return nil, false, err
	}
	list, _ := s.List()
	for _, sess := range list {
		if sess.HostID != opts.HostID || sess.Persist.Name != safe {
			continue
		}
		if exists {
			s.applyChrome(ctx, t, sess.Persist)
			RememberResume(sess)
			return sess, false, nil
		}
		_ = s.Reg.DeleteSession(sess.ID)
	}
	if exists {
		sess, err := s.Adopt(ctx, opts)
		return sess, false, err
	}
	sess, err := s.Create(ctx, opts)
	return sess, true, err
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
	sessionID := newID("sess")
	bridgeToken := newID("br")
	if err := rememberBridgeToken(sessionID, bridgeToken); err != nil {
		return nil, err
	}
	t, err := s.NewTransport(opts.HostID)
	if err != nil {
		forgetBridgeToken(sessionID)
		return nil, err
	}
	cmd := opts.Command
	if cmd == "" {
		cmd = "bash -l"
	}
	cmd = relaySessionCommand(cmd, sessionID, opts.HostID, name, bridgeToken)
	h, err := s.Persist.Create(ctx, t, name, cwd, cmd)
	if err != nil {
		forgetBridgeToken(sessionID)
		return nil, err
	}
	s.applyChrome(ctx, t, h)
	now := time.Now().UTC()
	sess := &Session{
		ID:                 sessionID,
		HostID:             opts.HostID,
		RemoteCWD:          cwd,
		Persist:            h,
		RepoRef:            opts.RepoRef,
		Labels:             opts.Labels,
		CreatedAt:          now,
		UpdatedAt:          now,
		SourceSessionID:    opts.SourceSessionID,
		SourceHostID:       opts.SourceHostID,
		SourcePersistName:  opts.SourcePersistName,
		CreatedByHandoffID: opts.CreatedByHandoffID,
	}
	if err := provisionBridgeIdentity(ctx, t, sess, bridgeToken); err != nil {
		_ = s.Persist.Destroy(ctx, t, h)
		forgetBridgeToken(sessionID)
		return nil, fmt.Errorf("provision bridge identity: %w", err)
	}
	if err := s.Reg.PutSession(sess); err != nil {
		_ = s.Persist.Destroy(ctx, t, h)
		clearBridgeIdentity(ctx, t, sess.ID)
		forgetBridgeToken(sessionID)
		return nil, err
	}
	RememberResume(sess)
	_ = AppendSessionStart(sess)
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
	s.applyChrome(ctx, t, h)
	now := time.Now().UTC()
	labels := opts.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if _, has := labels["adopted"]; !has {
		labels["adopted"] = "existing"
	}
	sessionID := newID("sess")
	bridgeToken := newID("br")
	if err := rememberBridgeToken(sessionID, bridgeToken); err != nil {
		return nil, err
	}
	sess := &Session{
		ID:                 sessionID,
		HostID:             opts.HostID,
		RemoteCWD:          cwd,
		Persist:            h,
		RepoRef:            opts.RepoRef,
		Labels:             labels,
		CreatedAt:          now,
		UpdatedAt:          now,
		SourceSessionID:    opts.SourceSessionID,
		SourceHostID:       opts.SourceHostID,
		SourcePersistName:  opts.SourcePersistName,
		CreatedByHandoffID: opts.CreatedByHandoffID,
	}
	if err := provisionBridgeIdentity(ctx, t, sess, bridgeToken); err != nil {
		forgetBridgeToken(sessionID)
		return nil, fmt.Errorf("provision adopted bridge identity: %w", err)
	}
	if err := s.Reg.PutSession(sess); err != nil {
		clearBridgeIdentity(ctx, t, sess.ID)
		forgetBridgeToken(sessionID)
		return nil, err
	}
	RememberResume(sess)
	_ = AppendSessionStart(sess)
	return sess, nil
}

// ProvisionBridge repairs the authenticated fallback identity for an existing
// registry session without restarting or injecting secrets into its agent.
func (s *SessionService) ProvisionBridge(ctx context.Context, sessionID string) (*Session, error) {
	sess, err := s.Reg.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.HostID == LocalHostID {
		return nil, fmt.Errorf("local sessions do not use a remote bridge identity")
	}
	t, err := s.NewTransport(sess.HostID)
	if err != nil {
		return nil, err
	}
	exists, err := s.Persist.Exists(ctx, t, sess.Persist)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("remote session %s/%s is not live", sess.HostID, sess.Persist.Name)
	}
	token := ""
	if raw, readErr := os.ReadFile(bridgeTokenPath(sess.ID)); readErr == nil {
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		token = newID("br")
		if err := rememberBridgeToken(sess.ID, token); err != nil {
			return nil, err
		}
	}
	if err := provisionBridgeIdentity(ctx, t, sess, token); err != nil {
		return nil, err
	}
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

// Rename changes a session's durable persistence identity in place. The
// session id remains stable so handoffs, bridge authentication, and lineage
// edges continue to point at the same work.
func (s *SessionService) Rename(ctx context.Context, id, name string) (*Session, error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return nil, err
	}
	if isLocalParent(sess) {
		return nil, fmt.Errorf("local parent identity is pane-owned; re-register it with relay parent register")
	}
	safe, err := shellquote.SanitizeSessionName(name)
	if err != nil {
		return nil, err
	}
	old := sess.Persist
	if old.Name == safe {
		return sess, nil
	}
	list, err := s.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	for _, other := range list {
		if other.ID != sess.ID && other.HostID == sess.HostID && other.Persist.Name == safe {
			return nil, fmt.Errorf("session name %q is already registered on %s", safe, sess.HostID)
		}
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return nil, err
	}
	next := ports.PersistHandle{Kind: old.Kind, Name: safe}
	if err := s.Persist.Rename(ctx, t, old, next); err != nil {
		return nil, err
	}
	sess.Persist = next
	if err := s.Reg.PutSession(sess); err != nil {
		_ = s.Persist.Rename(ctx, t, next, old)
		return nil, fmt.Errorf("save renamed session: %w", err)
	}
	if err := RenameResumePersist(old.Name, sess); err != nil {
		return sess, fmt.Errorf("session renamed, but resume registry update failed: %w", err)
	}
	if _, err := RenamePaneBindingsForPersist(old.Name, sess); err != nil {
		return sess, fmt.Errorf("session renamed, but pane history update failed: %w", err)
	}
	if err := AppendSessionRename(sess.ID, old.Name, safe); err != nil {
		return sess, fmt.Errorf("session renamed, but lineage update failed: %w", err)
	}
	if _, err := s.ProvisionBridge(ctx, sess.ID); err != nil {
		return sess, fmt.Errorf("session renamed, but bridge identity update failed: %w", err)
	}
	return sess, nil
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
	cwd, cmd, err := s.execPlan(sess, command)
	if err != nil {
		return "", "", err
	}
	return t.Run(ctx, cwd, cmd)
}

// execPlan resolves the (cwd, command) for an ad-hoc exec. Container sessions
// are wrapped with `docker exec` (no host cwd — the cwd is inside the wrap);
// host sessions pass through with their RemoteCWD.
func (s *SessionService) execPlan(sess *Session, command string) (cwd, cmd string, err error) {
	if sess.Container != nil {
		wrapped, werr := ContainerExec(sess.Container.Runtime, *sess.Container, command, false)
		if werr != nil {
			return "", "", werr
		}
		return "", wrapped, nil
	}
	return sess.RemoteCWD, command, nil
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
	if isLocalParent(sess) {
		return fmt.Errorf("refuse unguarded local parent destruction; use relay parent retire %s", id)
	}
	if !keepRemote {
		t, err := s.transportFor(sess)
		if err != nil {
			return err
		}
		_ = s.Persist.Destroy(ctx, t, sess.Persist)
		// Intentional teardown — cmux must not treat this as a reconnectable drop.
		MarkResumeCleaned(sess.Persist.Name, "destroyed")
		clearBridgeIdentity(ctx, t, sess.ID)
		forgetBridgeToken(sess.ID)
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
			forgetBridgeToken(sess.ID)
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
		if n.HostID == "" || n.HostID == LocalHostID || n.PersistName == "" {
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
