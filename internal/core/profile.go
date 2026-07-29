package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"gopkg.in/yaml.v3"
)

// ErrMissingProfile means a host has no ~/.config/relay/host.yaml yet — it has
// never been onboarded. Callers can offer `relay host init` as the fix.
var ErrMissingProfile = errors.New("relay host profile missing")

// HostProfile is authoritative on each remote (~/.config/relay/host.yaml).
type HostProfile struct {
	Version    int                    `yaml:"version" json:"version"`
	HostID     string                 `yaml:"host_id,omitempty" json:"host_id,omitempty"`
	Agents     []AgentSpec            `yaml:"agents" json:"agents"`
	PathMap    []PathMapEntry         `yaml:"path_map" json:"path_map"`
	Containers []ContainerSpec        `yaml:"containers,omitempty" json:"containers,omitempty"`
	Defaults   HostDefaults           `yaml:"defaults" json:"defaults"`
	Meta       map[string]any         `yaml:"meta,omitempty" json:"meta,omitempty"`
	ProbedAt   *time.Time             `yaml:"probed_at,omitempty" json:"probed_at,omitempty"`
	Probe      map[string]ProbeResult `yaml:"probe,omitempty" json:"probe,omitempty"`
}

// AgentSpec describes an agent CLI available on the host.
type AgentSpec struct {
	Name    string            `yaml:"name" json:"name"` // claude | cursor-agent | codex | ccs:personal | …
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Notes   string            `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// PathMapEntry maps a local identity to a remote working directory.
type PathMapEntry struct {
	// Match is matched against local git root basename, full path, or repo_ref.
	Match     string `yaml:"match" json:"match"`
	RemoteCWD string `yaml:"remote_cwd" json:"remote_cwd"` // absolute or ~/…
}

// HostDefaults are per-host defaults.
type HostDefaults struct {
	PreferredAgent string `yaml:"preferred_agent,omitempty" json:"preferred_agent,omitempty"`
	SilenceSec     int    `yaml:"silence_sec,omitempty" json:"silence_sec,omitempty"`
	WorkspaceHint  string `yaml:"workspace_hint,omitempty" json:"workspace_hint,omitempty"`
}

// ProbeResult records a capability probe for one agent.
type ProbeResult struct {
	Present bool   `yaml:"present" json:"present"`
	Authed  bool   `yaml:"authed" json:"authed"`
	Detail  string `yaml:"detail,omitempty" json:"detail,omitempty"`
}

// CachedProfile is the local read-through cache of a remote HostProfile.
type CachedProfile struct {
	HostID    string      `json:"host_id"`
	FetchedAt time.Time   `json:"fetched_at"`
	Profile   HostProfile `json:"profile"`
}

// ParseHostProfileYAML parses remote host.yaml contents.
func ParseHostProfileYAML(data []byte) (*HostProfile, error) {
	var p HostProfile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse host.yaml: %w", err)
	}
	if p.Version == 0 {
		p.Version = 1
	}
	return &p, nil
}

// matchPathMap returns the remote cwd for a local repo/basename, or ("", false).
func matchPathMap(entries []PathMapEntry, localRepo string) (string, bool) {
	base := filepath.Base(strings.TrimRight(localRepo, string(filepath.Separator)))
	for _, e := range entries {
		m := e.Match
		if m == localRepo || m == base || filepath.Base(m) == base {
			return e.RemoteCWD, true
		}
		if strings.HasSuffix(localRepo, string(filepath.Separator)+m) || strings.HasSuffix(localRepo, "/"+m) {
			return e.RemoteCWD, true
		}
	}
	return "", false
}

// ResolveRemoteCWD finds a path_map entry for the given local repo root / basename.
func (p *HostProfile) ResolveRemoteCWD(localRepo string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("nil host profile")
	}
	if cwd, ok := matchPathMap(p.PathMap, localRepo); ok {
		return cwd, nil
	}
	return "", fmt.Errorf("no path_map entry for %q on host (configure ~/.config/relay/host.yaml)", localRepo)
}

// FindAgent returns the agent spec by name, or the preferred default.
//
// Matching is exact first. On a miss it falls back to a single unambiguous
// alias: the requested name is a prefix of, or the binary base name of,
// exactly one listed agent — so "cursor" resolves to "cursor-agent" and "ccs"
// resolves to a lone "ccs:personal". If the name matches nothing, or is
// ambiguous across several agents, the error enumerates the available agents
// so the caller can correct it instead of guessing again.
func (p *HostProfile) FindAgent(name string) (*AgentSpec, error) {
	if p == nil {
		return nil, fmt.Errorf("nil host profile")
	}
	if name == "" {
		name = p.Defaults.PreferredAgent
	}
	if name == "" && len(p.Agents) > 0 {
		return &p.Agents[0], nil
	}
	// Exact match wins.
	for i := range p.Agents {
		if p.Agents[i].Name == name {
			return &p.Agents[i], nil
		}
	}
	// Unambiguous alias: prefix of, or binary base name of, exactly one agent.
	var match *AgentSpec
	matches := 0
	for i := range p.Agents {
		if agentNameMatchesAlias(p.Agents[i], name) {
			match = &p.Agents[i]
			matches++
		}
	}
	if matches == 1 {
		return match, nil
	}
	avail := make([]string, 0, len(p.Agents))
	for i := range p.Agents {
		avail = append(avail, p.Agents[i].Name)
	}
	if matches > 1 {
		return nil, fmt.Errorf("agent %q is ambiguous; available: %s", name, strings.Join(avail, ", "))
	}
	return nil, fmt.Errorf("agent %q not listed in host profile; available: %s", name, strings.Join(avail, ", "))
}

// agentNameMatchesAlias reports whether a short/alias name unambiguously refers
// to this agent: a prefix of its listed name (cursor → cursor-agent) or the
// base name of its launch binary (matches regardless of a path/args prefix).
func agentNameMatchesAlias(a AgentSpec, name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(a.Name, name) {
		return true
	}
	cmd := a.Command
	if cmd == "" {
		cmd = a.Name
	}
	if fields := strings.Fields(cmd); len(fields) > 0 {
		if path.Base(fields[0]) == name {
			return true
		}
	}
	return false
}

// InnerCommand is the bare agent invocation (no login-shell wrap).
func (a *AgentSpec) InnerCommand() string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.Command) != "" {
		return strings.TrimSpace(a.Command)
	}
	if strings.HasPrefix(a.Name, "ccs:") {
		return "ccs " + strings.TrimPrefix(a.Name, "ccs:")
	}
	return a.Name
}

// LaunchCommand builds the remote shell command to start an agent with a goal.
// Always runs under a login interactive shell so nvm/cargo/~/.local/bin are on PATH.
func (a *AgentSpec) LaunchCommand(goal string) string {
	inner := a.InnerCommand()
	if len(a.Args) > 0 {
		inner = shellJoin(append([]string{inner}, a.Args...))
	}
	_ = goal
	return wrapLoginShell(inner)
}

// wrapLoginShell ensures remotes see a full user PATH (nvm, ~/.local/bin, …).
func wrapLoginShell(inner string) string {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return "bash -ilc 'exec bash'"
	}
	if strings.Contains(inner, "bash -ilc") || strings.Contains(inner, "bash -lc") {
		return inner
	}
	return "bash -ilc " + shellQuote("exec "+inner)
}

// loginShellRun wraps a remote one-shot command for probes.
func loginShellRun(script string) string {
	return "bash -ilc " + shellQuote(script)
}

func shellJoin(parts []string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if strings.ContainsAny(p, " \t\"'`$&|;<>()\\") {
			out[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
		} else {
			out[i] = p
		}
	}
	return strings.Join(out, " ")
}

// ProfileService fetches and caches remote host profiles.
type ProfileService struct {
	NewTransport func(hostID string) (ports.Transport, error)
}

// Fetch loads the authoritative profile from the remote host and updates the local cache.
func (s *ProfileService) Fetch(ctx context.Context, hostID string) (*HostProfile, error) {
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return nil, err
	}
	data, err := t.ReadFile(ctx, RemoteHostProfilePath())
	if err != nil {
		return nil, fmt.Errorf("%w: host %s has no %s (%v); run: relay host init -H %s --apply", ErrMissingProfile, hostID, RemoteHostProfilePath(), err, hostID)
	}
	p, err := ParseHostProfileYAML(data)
	if err != nil {
		return nil, err
	}
	if p.HostID == "" {
		p.HostID = hostID
	}
	if err := s.saveCache(hostID, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Cache returns a cached profile if present (may be stale).
func (s *ProfileService) Cache(hostID string) (*CachedProfile, error) {
	b, err := os.ReadFile(ProfileCachePath(hostID))
	if err != nil {
		return nil, err
	}
	var c CachedProfile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Get prefers fresh fetch; falls back to cache only if fetch fails and allowStale.
func (s *ProfileService) Get(ctx context.Context, hostID string, allowStale bool) (*HostProfile, error) {
	p, err := s.Fetch(ctx, hostID)
	if err == nil {
		return p, nil
	}
	if !allowStale {
		return nil, err
	}
	c, cerr := s.Cache(hostID)
	if cerr != nil {
		return nil, err
	}
	return &c.Profile, nil
}

func (s *ProfileService) saveCache(hostID string, p *HostProfile) error {
	c := CachedProfile{HostID: hostID, FetchedAt: time.Now().UTC(), Profile: *p}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ProfileCachePath(hostID), b, 0o644)
}

// Probe runs remote checks for listed agents and writes results into the profile cache.
func (s *ProfileService) Probe(ctx context.Context, hostID string) (*HostProfile, error) {
	p, err := s.Fetch(ctx, hostID)
	if err != nil {
		return nil, err
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return nil, err
	}
	if p.Probe == nil {
		p.Probe = map[string]ProbeResult{}
	}
	for _, a := range p.Agents {
		pr := probeOneAgent(ctx, t, a)
		p.Probe[a.Name] = pr
	}
	now := time.Now().UTC()
	p.ProbedAt = &now
	if err := s.saveCache(hostID, p); err != nil {
		return nil, err
	}
	return p, nil
}

// agentCatalog is the fixed list of CLIs discover scans for (not PATH-wide).
var agentCatalog = []AgentSpec{
	{Name: "claude", Command: "claude", Notes: "interactive Claude Code CLI"},
	{Name: "cursor-agent", Command: "cursor-agent"},
	{Name: "codex", Command: "codex"},
	{Name: "ccs", Command: "ccs", Notes: "ccs multi-profile launcher"},
}

func probeAgentCatalog(ctx context.Context, t ports.Transport) []AgentDetect {
	var out []AgentDetect
	for _, spec := range agentCatalog {
		pr := probeOneAgent(ctx, t, spec)
		d := AgentDetect{
			Name:    spec.Name,
			Present: pr.Present,
			Authed:  pr.Authed,
			Detail:  pr.Detail,
		}
		if pr.Present {
			s := spec
			if s.Name == "ccs" {
				profiles := listCCSProfiles(ctx, t)
				if len(profiles) == 0 {
					profiles = []string{"personal"}
				}
				for _, prof := range profiles {
					as := AgentSpec{Name: "ccs:" + prof, Command: "ccs " + prof}
					out = append(out, AgentDetect{
						Name:          as.Name,
						Present:       true,
						Authed:        probeOneAgent(ctx, t, as).Authed,
						SuggestedSpec: &as,
					})
				}
				continue
			}
			d.SuggestedSpec = &s
		}
		out = append(out, d)
	}
	return out
}

func listCCSProfiles(ctx context.Context, t ports.Transport) []string {
	stdout, _, _ := t.Run(ctx, "", loginShellRun(`
if command -v ccs >/dev/null 2>&1; then
  ccs auth list 2>/dev/null || true
fi
ls -1 "$HOME"/.ccs/instances 2>/dev/null || true
`))
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || name == "Profile" || name == "ccs" || strings.HasSuffix(name, ".lock") {
			return
		}
		if strings.ContainsAny(name, "/ \\") || strings.Contains(name, "─") {
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "│") {
			fields := strings.Split(line, "│")
			if len(fields) >= 2 {
				add(fields[1])
			}
			continue
		}
		if !strings.Contains(line, " ") {
			add(line)
		}
	}
	sort.Strings(out)
	return out
}

func probeOneAgent(ctx context.Context, t ports.Transport, a AgentSpec) ProbeResult {
	bin := agentBinName(a)
	stdout, _, _ := t.Run(ctx, "", loginShellRun(
		fmt.Sprintf(`command -v %s >/dev/null 2>&1 && echo PRESENT || echo MISSING`, shellQuote(bin)),
	))
	present := strings.Contains(stdout, "PRESENT")
	authed := false
	detail := strings.TrimSpace(stdout)
	if !present {
		return ProbeResult{Present: false, Authed: false, Detail: detail}
	}
	switch {
	case a.Name == "claude" || bin == "claude":
		o, _, _ := t.Run(ctx, "", loginShellRun(`claude auth status 2>&1 | head -c 300; echo; claude -p PONG --model haiku 2>&1 | head -c 200`))
		detail = strings.TrimSpace(o)
		low := strings.ToLower(detail)
		authed = detail != "" &&
			!strings.Contains(low, "not logged in") &&
			!strings.Contains(low, `"loggedin": false`) &&
			!strings.Contains(low, "unauthorized") &&
			!strings.Contains(low, "oauth session expired") &&
			!strings.Contains(low, "failed to authenticate")
	case a.Name == "cursor-agent" || bin == "cursor-agent" || strings.Contains(bin, "cursor"):
		o, _, _ := t.Run(ctx, "", loginShellRun(`cursor-agent status 2>&1 | head -c 300`))
		detail = strings.TrimSpace(o)
		low := strings.ToLower(detail)
		authed = detail != "" && !strings.Contains(low, "not logged") && !strings.Contains(low, "logged out")
	case a.Name == "ccs" || strings.HasPrefix(a.Name, "ccs:") || bin == "ccs":
		prof := strings.TrimPrefix(a.Name, "ccs:")
		if prof == "" || prof == a.Name {
			prof = "personal"
		}
		o, _, _ := t.Run(ctx, "", loginShellRun(fmt.Sprintf(`ccs %s -p PONG 2>&1 | head -c 400`, shellQuote(prof))))
		detail = strings.TrimSpace(o)
		low := strings.ToLower(detail)
		// Weekly limit means auth works; only hard auth failures count as unauthed.
		authed = detail != "" &&
			!strings.Contains(low, "failed to authenticate") &&
			!strings.Contains(low, "oauth session expired") &&
			!strings.Contains(low, "not logged") &&
			!strings.Contains(low, "e301")
	case a.Name == "codex" || bin == "codex":
		o, _, _ := t.Run(ctx, "", loginShellRun(`codex login status 2>&1 | head -c 300`))
		detail = strings.TrimSpace(o)
		low := strings.ToLower(detail)
		authed = detail != "" && !strings.Contains(low, "not logged") && !strings.Contains(low, "unauthenticated")
		if detail == "" {
			authed = present
			detail = "PRESENT"
		}
	default:
		authed = present
	}
	return ProbeResult{Present: present, Authed: authed, Detail: detail}
}

func agentBinName(a AgentSpec) string {
	inner := a.InnerCommand()
	if inner == "" {
		return "true"
	}
	fields := strings.Fields(inner)
	bin := fields[0]
	if bin == "bash" || bin == "env" {
		for _, f := range fields {
			base := path.Base(f)
			if base == "ccs" || base == "claude" || base == "codex" || base == "cursor-agent" {
				return base
			}
		}
	}
	if strings.HasPrefix(a.Name, "ccs:") {
		return "ccs"
	}
	return path.Base(bin)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExampleHostProfileYAML is a starter template for remotes.
func ExampleHostProfileYAML(hostID string) string {
	return fmt.Sprintf(`# ~/.config/relay/host.yaml — authoritative on this host
version: 1
host_id: %s

agents:
  - name: claude
    command: claude
    notes: interactive Claude Code CLI
  - name: cursor-agent
    command: cursor-agent
  - name: codex
    command: codex
  - name: ccs:personal
    command: ccs personal
  - name: ccs:hcs
    command: ccs hcs

path_map:
  # match: local git basename or path fragment → remote cwd
  - match: relay
    remote_cwd: ~/gh/relay
  - match: dostos-workspace
    remote_cwd: ~/gh/dostos-workspace

defaults:
  preferred_agent: claude
  silence_sec: 45
`, hostID)
}
