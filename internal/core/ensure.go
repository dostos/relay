package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"gopkg.in/yaml.v3"
)

// EnsureOptions controls host ensure.
type EnsureOptions struct {
	Apply bool
}

// DepStatus is one dependency probe row (presence / rotation). Never persisted as runtime truth.
type DepStatus struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// EnsureResult is the JSON / text contract for `relay host ensure`.
type EnsureResult struct {
	OK             bool            `json:"ok"`
	HostID         string          `json:"host_id"`
	DryRun         bool            `json:"dry_run"`
	Applied        bool            `json:"applied,omitempty"`
	WroteProfile   bool            `json:"wrote_profile,omitempty"`
	Deps           []DepStatus     `json:"deps"`
	ProposedAgents []AgentSpec     `json:"proposed_agents"`
	SkippedAgents  []AgentSpec     `json:"skipped_agents,omitempty"`
	Auth           []AuthStatusRow `json:"auth,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	Next           string          `json:"next,omitempty"`
	Argv           []string        `json:"argv,omitempty"`
}

// EnsureService makes a host ready for account-agent launch (deps + yaml merge + auth help).
type EnsureService struct {
	NewTransport TransportFactory
	Profiles     *ProfileService
}

// Ensure probes deps, proposes additive account agents, optionally merges host.yaml, and
// returns live auth-help rows. It never writes remaining%/pin/current into config.
func (s *EnsureService) Ensure(ctx context.Context, hostID string, opts EnsureOptions) (*EnsureResult, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	res := &EnsureResult{
		OK:     true,
		HostID: hostID,
		DryRun: !opts.Apply,
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		res.OK = false
		res.Detail = err.Error()
		return res, nil
	}

	res.Deps = probeEnsureDeps(ctx, t)

	var existing []AgentSpec
	var profile *HostProfile
	raw, readErr := t.ReadFile(ctx, RemoteHostProfilePath())
	if readErr == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if p, err := ParseHostProfileYAML(raw); err == nil {
			profile = p
			existing = p.Agents
		}
	}

	proposed, skipped := proposedAccountAgents(ctx, t, existing)
	res.ProposedAgents = proposed
	res.SkippedAgents = skipped

	if depFail, detail := ensureDepsBlocking(res.Deps, proposed, existing); depFail {
		res.OK = false
		res.Detail = detail
		res.Next = "install missing tools on the host login PATH, then re-run ensure"
		res.Argv = []string{"relay", "host", "ensure", "-H", hostID}
		// Still surface auth help for whatever is present.
		res.Auth = ensureAuthRows(ctx, t, existing, proposed)
		return res, nil
	}

	if opts.Apply {
		if profile == nil {
			res.OK = false
			res.Detail = fmt.Sprintf("host.yaml missing at %s; run host init first", RemoteHostProfilePath())
			res.Next = fmt.Sprintf("relay host init -H %s --apply", hostID)
			res.Argv = []string{"relay", "host", "init", "-H", hostID, "--apply"}
			return res, nil
		}
		if len(proposed) == 0 {
			res.Detail = "nothing to apply; account agents already present or none discovered"
			res.Applied = true
		} else {
			merged, added := mergeAccountAgents(profile, proposed)
			body, err := yaml.Marshal(merged)
			if err != nil {
				res.OK = false
				res.Detail = err.Error()
				return res, nil
			}
			header := "# ~/.config/relay/host.yaml — updated by relay host ensure\n"
			if err := t.WriteFile(ctx, RemoteHostProfilePath(), []byte(header+string(body)), "644"); err != nil {
				res.OK = false
				res.Detail = err.Error()
				return res, nil
			}
			res.WroteProfile = true
			res.Applied = true
			res.Detail = fmt.Sprintf("merged %d account agent(s) at %s", added, time.Now().UTC().Format(time.RFC3339))
			if s.Profiles != nil {
				_, _ = s.Profiles.Fetch(ctx, hostID)
			}
			existing = merged.Agents
			proposed = nil
			res.ProposedAgents = nil
		}
	} else if len(proposed) > 0 {
		res.Next = fmt.Sprintf("relay host ensure -H %s --apply", hostID)
		res.Argv = []string{"relay", "host", "ensure", "-H", hostID, "--apply"}
		if res.Detail == "" {
			res.Detail = fmt.Sprintf("dry-run: would add %d account agent(s)", len(proposed))
		}
	}

	res.Auth = ensureAuthRows(ctx, t, existing, proposed)
	if unauthed := firstUnauthedAccount(res.Auth); unauthed != "" && res.Next == "" {
		res.Next = fmt.Sprintf("relay auth login -H %s --agent %s", hostID, unauthed)
		res.Argv = []string{"relay", "auth", "login", "-H", hostID, "--agent", unauthed}
	}
	return res, nil
}

func proposedAccountAgents(ctx context.Context, t ports.Transport, existing []AgentSpec) (proposed, skipped []AgentSpec) {
	for _, prof := range listCCSProfiles(ctx, t) {
		as := AgentSpec{Name: "ccs:" + prof, Command: "ccs " + prof}
		if hasAgent(existing, as.Name) {
			skipped = append(skipped, as)
			continue
		}
		proposed = append(proposed, as)
	}
	for _, sel := range listCodexMultiAuthAccounts(ctx, t) {
		as := AgentSpec{
			Name:     "codex:" + sel,
			Command:  "codex-multi-auth-codex",
			Args:     []string{"--account", sel},
			UsageKey: "codex",
		}
		if hasAgent(existing, as.Name) {
			skipped = append(skipped, as)
			continue
		}
		proposed = append(proposed, as)
	}
	return proposed, skipped
}

func mergeAccountAgents(p *HostProfile, proposed []AgentSpec) (*HostProfile, int) {
	if p == nil {
		return nil, 0
	}
	// Shallow copy so callers can keep the original profile pointer untouched.
	out := *p
	out.Agents = append([]AgentSpec{}, p.Agents...)
	added := 0
	for _, as := range proposed {
		if hasAgent(out.Agents, as.Name) {
			continue
		}
		out.Agents = append(out.Agents, as)
		added++
	}
	return &out, added
}

func probeEnsureDeps(ctx context.Context, t ports.Transport) []DepStatus {
	bins := []struct {
		name string
		hint string
	}{
		{"ccs", "install ccs and ensure it is on the login-shell PATH"},
		{"codex-multi-auth", "install codex-multi-auth and ensure it is on the login-shell PATH"},
		{"codex-multi-auth-codex", "install codex-multi-auth (wrapper) and ensure it is on the login-shell PATH"},
	}
	var out []DepStatus
	for _, b := range bins {
		stdout, _, _ := t.Run(ctx, "", loginShellRun(
			fmt.Sprintf(`command -v %s >/dev/null 2>&1 && echo PRESENT || echo MISSING`, shellQuote(b.name)),
		))
		present := strings.Contains(stdout, "PRESENT")
		d := DepStatus{Name: b.name, Present: present, Detail: strings.TrimSpace(stdout)}
		if !present {
			d.Hint = b.hint
		}
		out = append(out, d)
	}
	// Rotation is runtime policy on the host — report only, never store in yaml.
	rotOut, _, _ := t.Run(ctx, "", loginShellRun(`
if command -v codex-multi-auth >/dev/null 2>&1; then
  codex-multi-auth rotation status 2>&1 | head -c 800
fi
`))
	rotDetail := strings.TrimSpace(rotOut)
	rotOK := strings.Contains(strings.ToLower(rotDetail), "runtime rotation proxy: enabled") ||
		(strings.Contains(strings.ToLower(rotDetail), "stored setting: enabled") &&
			!strings.Contains(strings.ToLower(rotDetail), "runtime rotation proxy: disabled"))
	rot := DepStatus{
		Name:    "codex-multi-auth-rotation",
		Present: rotOK,
		Detail:  truncate(rotDetail, 240),
	}
	if !rotOK {
		rot.Hint = "codex-multi-auth rotation enable"
	}
	out = append(out, rot)
	return out
}

func ensureDepsBlocking(deps []DepStatus, proposed, existing []AgentSpec) (bool, string) {
	needCCS := accountStackPresent(proposed, existing, "ccs:")
	needCodex := accountStackPresent(proposed, existing, "codex:")
	byName := map[string]DepStatus{}
	for _, d := range deps {
		byName[d.Name] = d
	}
	var missing []string
	if needCCS && !byName["ccs"].Present {
		missing = append(missing, "ccs")
	}
	if needCodex {
		for _, n := range []string{"codex-multi-auth", "codex-multi-auth-codex", "codex-multi-auth-rotation"} {
			if !byName[n].Present {
				missing = append(missing, n)
			}
		}
	}
	// If no account stack yet but tools are half-installed, still require wrapper when multi-auth is present.
	if !needCodex && byName["codex-multi-auth"].Present && !byName["codex-multi-auth-codex"].Present {
		missing = append(missing, "codex-multi-auth-codex")
	}
	if len(missing) == 0 {
		return false, ""
	}
	return true, "missing required deps: " + strings.Join(missing, ", ")
}

func accountStackPresent(proposed, existing []AgentSpec, prefix string) bool {
	for _, a := range proposed {
		if strings.HasPrefix(a.Name, prefix) {
			return true
		}
	}
	for _, a := range existing {
		if strings.HasPrefix(a.Name, prefix) {
			return true
		}
	}
	return false
}

func ensureAuthRows(ctx context.Context, t ports.Transport, existing, proposed []AgentSpec) []AuthStatusRow {
	seen := map[string]bool{}
	var specs []AgentSpec
	add := func(list []AgentSpec) {
		for _, a := range list {
			if !strings.HasPrefix(a.Name, "ccs:") && !strings.HasPrefix(a.Name, "codex:") {
				continue
			}
			if seen[a.Name] {
				continue
			}
			seen[a.Name] = true
			specs = append(specs, a)
		}
	}
	add(existing)
	add(proposed)
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
	return rows
}

func firstUnauthedAccount(rows []AuthStatusRow) string {
	for _, r := range rows {
		if r.Present && !r.Authed {
			return r.Agent
		}
	}
	return ""
}
