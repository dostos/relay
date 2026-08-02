package core

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"gopkg.in/yaml.v3"
)

// DiscoverCard is the new-machine setup inventory for one host.
type DiscoverCard struct {
	OK                bool           `json:"ok"`
	HostID            string         `json:"host_id"`
	Reachable         bool           `json:"reachable"`
	ReachDetail       string         `json:"reach_detail,omitempty"`
	Tmux              TmuxInfo       `json:"tmux"`
	Relayd            string         `json:"relayd"`    // ok | missing | error detail
	HostYAML          string         `json:"host_yaml"` // missing | present
	Existing          *HostProfile   `json:"existing,omitempty"`
	AgentsDetected    []AgentDetect  `json:"agents_detected"`
	AgentsConfigured  []AgentSpec    `json:"agents_configured,omitempty"`
	PathSuggestions   []PathMapEntry `json:"path_suggestions"`
	Proposal          *HostProfile   `json:"proposal,omitempty"`
	ProposalYAML      string         `json:"proposal_yaml,omitempty"`
	Next              string         `json:"next,omitempty"`
	Argv              []string       `json:"argv,omitempty"`
	RemoteProfilePath string         `json:"remote_profile_path"`
}

// TmuxInfo is remote tmux availability.
type TmuxInfo struct {
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
}

// AgentDetect is one catalog agent probe result.
type AgentDetect struct {
	Name          string     `json:"name"`
	Present       bool       `json:"present"`
	Authed        bool       `json:"authed"`
	Detail        string     `json:"detail,omitempty"`
	SuggestedSpec *AgentSpec `json:"suggested_spec,omitempty"`
}

// DiscoverService builds new-machine discover cards.
type DiscoverService struct {
	NewTransport TransportFactory
	Coord        ports.Coord
	Profiles     *ProfileService
}

// Discover inventories a host and proposes host.yaml contents.
func (d *DiscoverService) Discover(ctx context.Context, hostID string) (*DiscoverCard, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	card := &DiscoverCard{
		OK:                true,
		HostID:            hostID,
		RemoteProfilePath: RemoteHostProfilePath(),
		HostYAML:          "missing",
	}
	t, err := d.NewTransport(hostID)
	if err != nil {
		card.OK = false
		card.ReachDetail = err.Error()
		return card, nil
	}

	// Reachability + tmux + path listing in one remote script.
	inv, err := remoteInventory(ctx, t)
	if err != nil {
		card.OK = false
		card.Reachable = false
		card.ReachDetail = err.Error()
		card.Next = fmt.Sprintf("fix ssh -H %s and retry: relay host discover -H %s", hostID, hostID)
		card.Argv = []string{"relay", "host", "discover", "-H", hostID}
		return card, nil
	}
	card.Reachable = true
	card.Tmux = inv.Tmux
	card.PathSuggestions = suggestPaths(inv.DevDirs, inv.GhDirs, localGitBasename())

	// Existing host.yaml
	if raw, err := t.ReadFile(ctx, RemoteHostProfilePath()); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		card.HostYAML = "present"
		if p, err := ParseHostProfileYAML(raw); err == nil {
			card.Existing = p
			card.AgentsConfigured = p.Agents
			card.PathSuggestions = mergePathSuggestions(p.PathMap, card.PathSuggestions)
		}
	}

	// relayd
	card.Relayd = "missing"
	if d.Coord != nil {
		if err := d.Coord.Ensure(ctx, t); err != nil {
			card.Relayd = err.Error()
		} else {
			card.Relayd = "ok"
		}
	}

	// Agent catalog (independent of host.yaml)
	card.AgentsDetected = probeAgentCatalog(ctx, t)

	card.Proposal = buildProposal(hostID, card)
	if y, err := yaml.Marshal(card.Proposal); err == nil {
		card.ProposalYAML = "# ~/.config/relay/host.yaml — proposed by relay host discover\n" + string(y)
	}

	if card.HostYAML == "missing" || card.Relayd != "ok" {
		card.Next = fmt.Sprintf("relay host init -H %s --apply", hostID)
		card.Argv = []string{"relay", "host", "init", "-H", hostID, "--apply"}
	} else {
		card.Next = fmt.Sprintf("relay host probe -H %s", hostID)
		card.Argv = []string{"relay", "host", "probe", "-H", hostID}
	}
	return card, nil
}

type inventory struct {
	Tmux    TmuxInfo
	DevDirs []string
	GhDirs  []string
}

func remoteInventory(ctx context.Context, t ports.Transport) (*inventory, error) {
	script := `
set -e
echo REACHABLE
if command -v tmux >/dev/null 2>&1; then
  echo TMUX_OK $(tmux -V 2>/dev/null | head -1)
else
  echo TMUX_MISSING
fi
echo DEV_BEGIN
for d in "$HOME"/dev/*/; do
  [ -d "$d" ] || continue
  basename "$d"
done
echo DEV_END
echo GH_BEGIN
for d in "$HOME"/gh/*/; do
  [ -d "$d" ] || continue
  basename "$d"
done
echo GH_END
`
	stdout, stderr, err := t.Run(ctx, "", "bash -lc "+shellQuote(script))
	if err != nil {
		return nil, fmt.Errorf("inventory: %w (%s)", err, strings.TrimSpace(stderr))
	}
	inv := &inventory{}
	section := ""
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "REACHABLE":
			// ok
		case strings.HasPrefix(line, "TMUX_OK"):
			inv.Tmux = TmuxInfo{Present: true, Version: strings.TrimSpace(strings.TrimPrefix(line, "TMUX_OK"))}
		case line == "TMUX_MISSING":
			inv.Tmux = TmuxInfo{Present: false}
		case line == "DEV_BEGIN":
			section = "dev"
		case line == "DEV_END", line == "GH_END":
			section = ""
		case line == "GH_BEGIN":
			section = "gh"
		case section == "dev" && line != "":
			inv.DevDirs = append(inv.DevDirs, line)
		case section == "gh" && line != "":
			inv.GhDirs = append(inv.GhDirs, line)
		}
	}
	if !strings.Contains(stdout, "REACHABLE") {
		return nil, fmt.Errorf("unexpected inventory output")
	}
	return inv, nil
}

func suggestPaths(dev, gh []string, boost string) []PathMapEntry {
	seen := map[string]bool{}
	var out []PathMapEntry
	add := func(match, cwd string) {
		if match == "" || seen[match] {
			return
		}
		seen[match] = true
		out = append(out, PathMapEntry{Match: match, RemoteCWD: cwd})
	}
	// Boost local git basename first if present remotely.
	if boost != "" {
		for _, n := range dev {
			if n == boost {
				add(n, "~/dev/"+n)
			}
		}
		for _, n := range gh {
			if n == boost {
				add(n, "~/gh/"+n)
			}
		}
	}
	sort.Strings(dev)
	sort.Strings(gh)
	for _, n := range dev {
		add(n, "~/dev/"+n)
	}
	for _, n := range gh {
		add(n, "~/gh/"+n)
	}
	return out
}

func mergePathSuggestions(existing, suggested []PathMapEntry) []PathMapEntry {
	have := map[string]bool{}
	for _, e := range existing {
		have[e.Match] = true
	}
	var adds []PathMapEntry
	for _, s := range suggested {
		if !have[s.Match] {
			adds = append(adds, s)
		}
	}
	// Return existing first (as configured), then proposed adds.
	out := append([]PathMapEntry{}, existing...)
	out = append(out, adds...)
	return out
}

func localGitBasename() string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(b)))
}

func buildProposal(hostID string, card *DiscoverCard) *HostProfile {
	p := &HostProfile{
		Version: 1,
		HostID:  hostID,
		Defaults: HostDefaults{
			PreferredAgent: "claude",
			SilenceSec:     DefaultSilenceSec,
		},
	}
	if card.Existing != nil {
		p.Defaults = card.Existing.Defaults
		if p.Defaults.SilenceSec == 0 {
			p.Defaults.SilenceSec = DefaultSilenceSec
		}
	}
	// Agents: prefer detected present ones; fall back to existing configured.
	seen := map[string]bool{}
	for _, d := range card.AgentsDetected {
		if !d.Present || d.SuggestedSpec == nil {
			continue
		}
		if seen[d.SuggestedSpec.Name] {
			continue
		}
		seen[d.SuggestedSpec.Name] = true
		p.Agents = append(p.Agents, *d.SuggestedSpec)
	}
	if len(p.Agents) == 0 && card.Existing != nil {
		p.Agents = append(p.Agents, card.Existing.Agents...)
	}
	if len(p.Agents) == 0 {
		// Still emit a sensible starter list so init isn't empty.
		p.Agents = []AgentSpec{
			{Name: "claude", Command: "claude"},
			{Name: "cursor-agent", Command: "cursor-agent"},
			{Name: "codex", Command: "codex"},
		}
	}
	// Preferred: first present+authed, else first present, else keep default.
	preferred := ""
	for _, d := range card.AgentsDetected {
		if d.Present && d.Authed {
			preferred = d.Name
			break
		}
	}
	if preferred == "" {
		for _, d := range card.AgentsDetected {
			if d.Present {
				preferred = d.Name
				break
			}
		}
	}
	if preferred != "" {
		p.Defaults.PreferredAgent = preferred
	}
	p.PathMap = append([]PathMapEntry{}, card.PathSuggestions...)
	if len(p.PathMap) == 0 && card.Existing != nil {
		p.PathMap = append(p.PathMap, card.Existing.PathMap...)
	}
	return p
}

// InitResult is the mutating new-machine follow-through.
type InitResult struct {
	OK           bool             `json:"ok"`
	HostID       string           `json:"host_id"`
	DryRun       bool             `json:"dry_run"`
	Applied      bool             `json:"applied"`
	Bootstrap    *BootstrapResult `json:"bootstrap,omitempty"`
	WroteProfile bool             `json:"wrote_profile"`
	Forced       bool             `json:"forced,omitempty"`
	Discover     *DiscoverCard    `json:"discover"`
	Probe        *HostProfile     `json:"probe,omitempty"`
	Detail       string           `json:"detail,omitempty"`
	Next         string           `json:"next,omitempty"`
	Argv         []string         `json:"argv,omitempty"`
}

// InitOptions controls host init.
type InitOptions struct {
	Apply bool
	Force bool
}

// Init bootstraps relayd and optionally writes the proposed host.yaml.
func (d *DiscoverService) Init(ctx context.Context, hostID string, opts InitOptions, boot *BootstrapService) (*InitResult, error) {
	card, err := d.Discover(ctx, hostID)
	if err != nil {
		return nil, err
	}
	res := &InitResult{
		OK:       card.Reachable,
		HostID:   hostID,
		DryRun:   !opts.Apply,
		Discover: card,
	}
	if !card.Reachable {
		res.OK = false
		res.Detail = card.ReachDetail
		res.Next = card.Next
		res.Argv = card.Argv
		return res, nil
	}

	if !opts.Apply {
		res.Detail = fmt.Sprintf("dry-run: would bootstrap relayd and write %s", RemoteHostProfilePath())
		if card.HostYAML == "present" && !opts.Force {
			res.Detail += " (host.yaml exists; --force required to overwrite)"
		}
		res.Next = fmt.Sprintf("relay host init -H %s --apply", hostID)
		res.Argv = []string{"relay", "host", "init", "-H", hostID, "--apply"}
		if card.HostYAML == "present" {
			res.Next += " --force"
			res.Argv = append(res.Argv, "--force")
		}
		return res, nil
	}

	// Apply path
	if boot != nil {
		br, err := boot.Bootstrap(ctx, hostID)
		res.Bootstrap = br
		if err != nil {
			res.OK = false
			res.Detail = err.Error()
			return res, nil
		}
		if br == nil || !br.Started || !br.PingOK || br.Build == "" {
			res.OK = false
			res.Detail = "bootstrap returned without a verified running build"
			return res, nil
		}
		res.Applied = true
	}

	t, err := d.NewTransport(hostID)
	if err != nil {
		res.OK = false
		res.Detail = err.Error()
		return res, nil
	}

	write := false
	switch {
	case card.HostYAML == "missing":
		write = true
	case opts.Force:
		write = true
		res.Forced = true
	default:
		res.Detail = "host.yaml already present; pass --force to overwrite with proposal"
		res.Next = fmt.Sprintf("relay host init -H %s --apply --force", hostID)
		res.Argv = []string{"relay", "host", "init", "-H", hostID, "--apply", "--force"}
	}

	if write {
		body := card.ProposalYAML
		if body == "" && card.Proposal != nil {
			y, _ := yaml.Marshal(card.Proposal)
			body = string(y)
		}
		if err := t.WriteFile(ctx, RemoteHostProfilePath(), []byte(body), "644"); err != nil {
			res.OK = false
			res.Detail = err.Error()
			return res, nil
		}
		res.WroteProfile = true
		// refresh local cache
		if d.Profiles != nil {
			_, _ = d.Profiles.Fetch(ctx, hostID)
		}
	}

	if d.Profiles != nil && (res.WroteProfile || card.HostYAML == "present") {
		if p, err := d.Profiles.Probe(ctx, hostID); err == nil {
			res.Probe = p
		}
	}

	res.OK = true
	res.Next = fmt.Sprintf("relay handoff -H %s --agent %s --goal '…'", hostID, preferredFromCard(card))
	res.Argv = []string{"relay", "handoff", "-H", hostID, "--agent", preferredFromCard(card), "--goal", "…"}
	if res.Detail == "" {
		res.Detail = fmt.Sprintf("init complete at %s", time.Now().UTC().Format(time.RFC3339))
	}
	return res, nil
}

func preferredFromCard(card *DiscoverCard) string {
	if card != nil && card.Proposal != nil && card.Proposal.Defaults.PreferredAgent != "" {
		return card.Proposal.Defaults.PreferredAgent
	}
	return "claude"
}

// FormatDiscoverText is a thin human summary of a discover card.
func FormatDiscoverText(c *DiscoverCard) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	reach := "no"
	if c.Reachable {
		reach = "yes"
	}
	tmux := "no"
	if c.Tmux.Present {
		tmux = "yes"
	}
	fmt.Fprintf(&b, "host %s\n", c.HostID)
	fmt.Fprintf(&b, "  reachable   %s\n", reach)
	fmt.Fprintf(&b, "  host.yaml   %s\n", c.HostYAML)
	fmt.Fprintf(&b, "  relayd      %s\n", c.Relayd)
	fmt.Fprintf(&b, "  tmux        %s\n", tmux)
	if c.ReachDetail != "" && !c.Reachable {
		fmt.Fprintf(&b, "  reach       %s\n", c.ReachDetail)
	}
	if len(c.AgentsDetected) > 0 {
		fmt.Fprintf(&b, "  agents\n")
		for _, a := range c.AgentsDetected {
			mark := "missing"
			if a.Present && a.Authed {
				mark = "ready"
			} else if a.Present {
				mark = "present"
			}
			fmt.Fprintf(&b, "    %-14s %s\n", a.Name, mark)
		}
	}
	if n := len(c.PathSuggestions); n > 0 {
		fmt.Fprintf(&b, "  paths       %d suggestion(s)\n", n)
	}
	if c.Next != "" {
		fmt.Fprintf(&b, "  next        %s\n", c.Next)
	}
	return b.String()
}
