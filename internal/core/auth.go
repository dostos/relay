package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// AuthStatusRow is one agent’s auth/presence on a host.
type AuthStatusRow struct {
	Agent   string `json:"agent"`
	Present bool   `json:"present"`
	Authed  bool   `json:"authed"`
	Detail  string `json:"detail,omitempty"`
	Login   string `json:"login_cmd,omitempty"`
	CopyOK  bool   `json:"copy_supported"`
	// WeeklyRemaining is the account's remaining weekly usage (0–100, % LEFT)
	// when a usage hook is configured; nil when unknown or no hook.
	WeeklyRemaining *int `json:"weekly_remaining,omitempty"`
}

// AuthLoginResult is returned after opening an interactive login pane.
type AuthLoginResult struct {
	OK        bool   `json:"ok"`
	HostID    string `json:"host_id"`
	Agent     string `json:"agent"`
	SessionID string `json:"session_id,omitempty"`
	Surface   string `json:"surface,omitempty"`
	LoginCmd  string `json:"login_cmd"`
	AuthURL   string `json:"auth_url,omitempty"` // reassembled; pane width often wraps/crops this
	Opened    bool   `json:"opened,omitempty"`   // local browser open attempted
	Hint      string `json:"hint"`
	Next      string `json:"next"`
}

// AuthCopyResult summarizes a credential copy.
type AuthCopyResult struct {
	OK     bool     `json:"ok"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Agent  string   `json:"agent"`
	Files  []string `json:"files"`
	Authed bool     `json:"authed"`
	Detail string   `json:"detail,omitempty"`
}

// AuthService probes and repairs agent auth across hosts.
type AuthService struct {
	Profiles     *ProfileService
	Sessions     *SessionService
	Viz          ports.Viz
	NewTransport TransportFactory
}

// SpecForAgent builds a spec from a name, preferring host.yaml when available.
func SpecForAgent(profile *HostProfile, name string) (AgentSpec, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return AgentSpec{}, fmt.Errorf("--agent required")
	}
	if profile != nil {
		if ag, err := profile.FindAgent(name); err == nil {
			return *ag, nil
		}
	}
	if strings.HasPrefix(name, "ccs:") {
		return AgentSpec{Name: name, Command: "ccs " + strings.TrimPrefix(name, "ccs:")}, nil
	}
	if strings.HasPrefix(name, "codex:") {
		sel := strings.TrimPrefix(name, "codex:")
		return AgentSpec{
			Name:     name,
			Command:  "codex-multi-auth-codex",
			Args:     []string{"--account", sel},
			UsageKey: "codex",
		}, nil
	}
	switch name {
	case "claude", "cursor-agent", "codex", "ccs":
		return AgentSpec{Name: name, Command: name}, nil
	default:
		return AgentSpec{Name: name, Command: name}, nil
	}
}

// LoginCommand is the interactive vendor login for an agent (login-shell wrapped).
func LoginCommand(spec AgentSpec) string {
	name := spec.Name
	switch {
	case strings.HasPrefix(name, "ccs:"):
		prof := strings.TrimPrefix(name, "ccs:")
		return wrapLoginShell(fmt.Sprintf("ccs auth create %s --force", prof))
	case name == "ccs":
		return wrapLoginShell("ccs auth create personal --force")
	case name == "claude" || agentBinName(spec) == "claude":
		return wrapLoginShell("claude auth login")
	case name == "cursor-agent" || strings.Contains(agentBinName(spec), "cursor"):
		return wrapLoginShell("cursor-agent login")
	case strings.HasPrefix(name, "codex:") || agentBinName(spec) == "codex-multi-auth-codex" || agentBinName(spec) == "mcodex":
		return wrapLoginShell("codex-multi-auth login")
	case name == "codex" || agentBinName(spec) == "codex":
		return wrapLoginShell("codex login")
	default:
		// Fall back to launching the agent; user completes vendor auth UX.
		return wrapLoginShell(spec.InnerCommand())
	}
}

// CredentialPaths returns Linux-side files that can be copied between hosts.
// Empty means copy is unsupported (use auth login).
func CredentialPaths(spec AgentSpec) []string {
	name := spec.Name
	switch {
	case strings.HasPrefix(name, "ccs:"):
		prof := strings.TrimPrefix(name, "ccs:")
		return []string{path.Join("~/.ccs/instances", prof, ".credentials.json")}
	case name == "codex":
		return []string{"~/.codex/auth.json", "~/.codex/config.toml"}
	default:
		return nil
	}
}

// Status probes agents on a host (host.yaml list, or a single --agent).
func (s *AuthService) Status(ctx context.Context, hostID, agentFilter string) ([]AuthStatusRow, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return nil, err
	}
	profile, _ := s.Profiles.Get(ctx, hostID, true)

	var specs []AgentSpec
	if agentFilter != "" {
		spec, err := SpecForAgent(profile, agentFilter)
		if err != nil {
			return nil, err
		}
		specs = []AgentSpec{spec}
	} else if profile != nil && len(profile.Agents) > 0 {
		specs = append(specs, profile.Agents...)
		// Also surface discovered CCS / multi-auth accounts not yet in host.yaml.
		for _, prof := range listCCSProfiles(ctx, t) {
			name := "ccs:" + prof
			if hasAgent(specs, name) {
				continue
			}
			specs = append(specs, AgentSpec{Name: name, Command: "ccs " + prof})
		}
		for _, sel := range listCodexMultiAuthAccounts(ctx, t) {
			name := "codex:" + sel
			if hasAgent(specs, name) {
				continue
			}
			specs = append(specs, AgentSpec{
				Name:     name,
				Command:  "codex-multi-auth-codex",
				Args:     []string{"--account", sel},
				UsageKey: "codex",
			})
		}
	} else {
		for _, d := range probeAgentCatalog(ctx, t) {
			if d.SuggestedSpec != nil {
				specs = append(specs, *d.SuggestedSpec)
			} else {
				specs = append(specs, AgentSpec{Name: d.Name, Command: d.Name})
			}
		}
	}

	var rows []AuthStatusRow
	for _, spec := range specs {
		pr := probeOneAgent(ctx, t, spec)
		rows = append(rows, AuthStatusRow{
			Agent:   spec.Name,
			Present: pr.Present,
			Authed:  pr.Authed,
			Detail:  truncate(pr.Detail, 240),
			Login:   LoginCommand(spec),
			CopyOK:  len(CredentialPaths(spec)) > 0,
		})
	}
	// Annotate with remaining weekly usage when a hook is configured.
	if hint, ok := LoadUsageHint(ctx, usageHookFor(profile)); ok {
		for i := range rows {
			key := rows[i].Agent
			if profile != nil {
				if spec, findErr := profile.FindAgent(rows[i].Agent); findErr == nil && strings.TrimSpace(spec.UsageKey) != "" {
					key = strings.TrimSpace(spec.UsageKey)
				}
			}
			if v, known := hint.Remaining(key); known {
				vv := v
				rows[i].WeeklyRemaining = &vv
			}
		}
	}
	return rows, nil
}

func hasAgent(specs []AgentSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Login opens a cmux pane running the agent’s interactive login command.
// Narrow panes wrap OAuth URLs; we reassemble from capture and open locally.
func (s *AuthService) Login(ctx context.Context, hostID, agent string) (*AuthLoginResult, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	profile, err := s.Profiles.Get(ctx, hostID, true)
	if err != nil {
		// still allow catalog agents when profile missing
		profile = nil
	}
	spec, err := SpecForAgent(profile, agent)
	if err != nil {
		return nil, err
	}
	loginCmd := LoginCommand(spec)
	if s.Sessions == nil {
		return nil, fmt.Errorf("session service not configured")
	}
	safe := sanitizeID(strings.ReplaceAll(spec.Name, ":", "-"))
	authName := "auth-" + safe
	// Drop stale auth tmux (previous login left it behind) + local viz rows.
	if list, err := s.Sessions.List(); err == nil {
		for _, old := range list {
			if old.HostID == hostID && old.Persist.Name == authName {
				if s.Viz != nil {
					_ = s.Viz.Close(ctx, old.ID)
				}
			}
		}
	}
	sess, err := s.Sessions.ReplaceCreate(ctx, CreateOpts{
		HostID:    hostID,
		Name:      authName,
		RemoteCWD: "~",
		Command:   loginCmd,
		Labels:    map[string]string{"role": "auth", "agent": spec.Name},
	})
	if err != nil {
		return nil, err
	}
	res := &AuthLoginResult{
		OK:        true,
		HostID:    hostID,
		Agent:     spec.Name,
		SessionID: sess.ID,
		LoginCmd:  loginCmd,
		Hint:      "complete OAuth/API login in the pane (or the opened browser), then: relay auth status -H " + hostID + " --agent " + spec.Name,
		Next:      "relay auth status -H " + hostID + " --agent " + spec.Name,
	}
	if s.Viz != nil && s.Viz.Available(ctx) {
		launch := ResumeLaunchCmd(sess.Persist.Name)
		ref, err := PresentSession(ctx, s.Viz, sess, launch, ports.Layout{Mode: "remote"})
		if err == nil {
			res.Surface = ref
			sess.VizSurfaceRef = ref
			_ = s.Sessions.Reg.PutSession(sess)
			RememberPane(ref, sess, true)
		} else {
			res.Hint += " (viz present failed: " + err.Error() + "; attach with relay resume --session " + sess.Persist.Name + ")"
		}
	} else {
		res.Hint += "; viz unavailable — relay resume --session " + sess.Persist.Name
	}

	if url, err := s.WaitAuthURL(ctx, sess.ID, 12*time.Second); err == nil && url != "" {
		res.AuthURL = url
		if openLocalURL(url) {
			res.Opened = true
			res.Hint = "opened auth URL in local browser; paste the code back into the pane if prompted. Then: " + res.Next
		} else {
			res.Hint = "auth URL (pane was wrapped/cropped — use this full URL):\n" + url + "\nThen: " + res.Next
		}
	} else {
		res.Hint += "; if the URL looks cropped, run: relay auth url --session " + sess.ID
	}
	return res, nil
}

// WaitAuthURL polls session capture until an https auth/login URL appears.
func (s *AuthService) WaitAuthURL(ctx context.Context, sessionID string, timeout time.Duration) (string, error) {
	if s.Sessions == nil {
		return "", fmt.Errorf("session service not configured")
	}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		text, err := s.Sessions.Capture(ctx, sessionID, 120)
		if err == nil {
			if u := ExtractWrappedURL(text); u != "" {
				return u, nil
			}
			last = text
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if u := ExtractWrappedURL(last); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no auth URL in pane yet")
}

// ExtractAuthURL captures a session and returns a reassembled auth URL.
func (s *AuthService) ExtractAuthURL(ctx context.Context, sessionID string) (string, error) {
	text, err := s.Sessions.Capture(ctx, sessionID, 160)
	if err != nil {
		return "", err
	}
	u := ExtractWrappedURL(text)
	if u == "" {
		return "", fmt.Errorf("no https URL found in session capture")
	}
	return u, nil
}

var urlChar = regexp.MustCompile(`^https://[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+`)

// ExtractWrappedURL rebuilds an https URL that a narrow terminal wrapped across lines.
func ExtractWrappedURL(capture string) string {
	idx := strings.Index(capture, "https://")
	if idx < 0 {
		return ""
	}
	rest := capture[idx:]
	lower := strings.ToLower(rest)
	for _, stop := range []string{"\npaste code", "\npaste the code", "\nenter code", "\nopening browser"} {
		if j := strings.Index(lower, stop); j > 0 {
			rest = rest[:j]
			break
		}
	}
	// Soft-wrap: newlines (and spaces introduced by wrap) are not part of the URL.
	joined := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, rest)
	m := urlChar.FindString(joined)
	if m == "" {
		return ""
	}
	return m
}

func openLocalURL(u string) bool {
	if u == "" || os.Getenv("RELAY_NO_OPEN") == "1" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	default:
		return false
	}
	return cmd.Start() == nil
}

// Copy transfers known credential files from one host to another, then re-probes.
func (s *AuthService) Copy(ctx context.Context, fromHost, toHost, agent string) (*AuthCopyResult, error) {
	if fromHost == "" || toHost == "" {
		return nil, fmt.Errorf("--from and --to hosts required")
	}
	if fromHost == toHost {
		return nil, fmt.Errorf("from and to hosts must differ")
	}
	fromT, err := s.NewTransport(fromHost)
	if err != nil {
		return nil, err
	}
	toT, err := s.NewTransport(toHost)
	if err != nil {
		return nil, err
	}
	fromProfile, _ := s.Profiles.Get(ctx, fromHost, true)
	spec, err := SpecForAgent(fromProfile, agent)
	if err != nil {
		return nil, err
	}
	paths := CredentialPaths(spec)
	if len(paths) == 0 {
		return nil, fmt.Errorf("copy unsupported for %q; use: relay auth login -H %s --agent %s", agent, toHost, agent)
	}
	srcProbe := probeOneAgent(ctx, fromT, spec)
	if !srcProbe.Present || !srcProbe.Authed {
		return nil, fmt.Errorf("source %s agent %s not present+authed (%s)", fromHost, spec.Name, truncate(srcProbe.Detail, 120))
	}
	var copied []string
	for _, p := range paths {
		data, err := fromT.ReadFile(ctx, p)
		if err != nil {
			// skip missing optional files (e.g. codex config)
			if strings.Contains(p, "config.toml") {
				continue
			}
			return nil, fmt.Errorf("read %s:%s: %w", fromHost, p, err)
		}
		if len(bytesTrimSpace(data)) == 0 {
			continue
		}
		if err := toT.WriteFile(ctx, p, data, "600"); err != nil {
			return nil, fmt.Errorf("write %s:%s: %w", toHost, p, err)
		}
		copied = append(copied, p)
	}
	if len(copied) == 0 {
		return nil, fmt.Errorf("no credential files copied from %s", fromHost)
	}
	dstProbe := probeOneAgent(ctx, toT, spec)
	return &AuthCopyResult{
		OK:     dstProbe.Authed,
		From:   fromHost,
		To:     toHost,
		Agent:  spec.Name,
		Files:  copied,
		Authed: dstProbe.Authed,
		Detail: truncate(dstProbe.Detail, 240),
	}, nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
