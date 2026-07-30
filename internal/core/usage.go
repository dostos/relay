package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultUsageMinRemaining is the weekly % LEFT below which an agent is treated
// as exhausted for auto-selection, when host.yaml does not set one.
const defaultUsageMinRemaining = 5

// UsageHint holds per-agent remaining weekly headroom (0–100, % LEFT), keyed by
// relay agent Name (e.g. "cursor-agent", "codex", "ccs:hcs"). It is produced by
// a user-configured hook command — relay itself has no knowledge of provider
// quota APIs, keeping it dependency-free.
type UsageHint struct {
	Weekly map[string]int `json:"weekly,omitempty"`
	Source string         `json:"source,omitempty"`
}

// Remaining returns (pct, known) for an agent name.
func (u UsageHint) Remaining(agent string) (int, bool) {
	if u.Weekly == nil {
		return 0, false
	}
	v, ok := u.Weekly[agent]
	return v, ok
}

// usageHookOutput is the JSON contract a usage hook prints on stdout:
//
//	{"agents": {"cursor-agent": {"weekly_remaining": 42}, "codex": {"weekly_remaining": 0}}}
type usageHookOutput struct {
	Agents map[string]struct {
		WeeklyRemaining *int `json:"weekly_remaining"`
	} `json:"agents"`
}

// usageHookFor resolves the effective hook command: host.yaml defaults first,
// then the RELAY_USAGE_HOOK env var as a machine-wide fallback. "" = disabled.
func usageHookFor(profile *HostProfile) string {
	if profile != nil {
		if h := strings.TrimSpace(profile.Defaults.UsageHook); h != "" {
			return h
		}
	}
	return strings.TrimSpace(os.Getenv("RELAY_USAGE_HOOK"))
}

// LoadUsageHint runs the hook command locally and parses its JSON. It never
// blocks a handoff: any failure (empty hook, timeout, non-zero exit, bad JSON,
// no usable rows) returns ok=false so callers fall back to non-usage selection.
// The hook runs via `sh -c` inheriting relay's environment, so PATH-installed
// tools like `agent-usage` resolve as they do in the invoking shell.
func LoadUsageHint(ctx context.Context, hookCmd string) (UsageHint, bool) {
	hookCmd = strings.TrimSpace(hookCmd)
	if hookCmd == "" {
		return UsageHint{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "sh", "-c", hookCmd).Output()
	if err != nil {
		return UsageHint{Source: hookCmd}, false
	}
	var parsed usageHookOutput
	if jerr := json.Unmarshal(out, &parsed); jerr != nil {
		return UsageHint{Source: hookCmd}, false
	}
	weekly := make(map[string]int, len(parsed.Agents))
	for name, a := range parsed.Agents {
		if a.WeeklyRemaining != nil {
			weekly[name] = *a.WeeklyRemaining
		}
	}
	if len(weekly) == 0 {
		return UsageHint{Source: hookCmd}, false
	}
	return UsageHint{Weekly: weekly, Source: hookCmd}, true
}

// AgentRank is one candidate's standing, for advisory display (`relay agent pick`).
type AgentRank struct {
	Agent           string `json:"agent"`
	WeeklyRemaining *int   `json:"weekly_remaining,omitempty"`
	Preferred       bool   `json:"preferred,omitempty"`
	Chosen          bool   `json:"chosen,omitempty"`
}

// SelectAgent chooses an agent from candidates using the usage hint:
//
//  1. If preferred is a candidate and its weekly remaining is unknown or
//     >= minRemaining, pick it.
//  2. Else pick the candidate with the highest weekly remaining >= minRemaining.
//  3. Else keep preferred (if a candidate), else the first candidate.
//
// Ties keep candidate order. Returns the pick plus a ranking in candidate order.
func SelectAgent(candidates []string, preferred string, hint UsageHint, minRemaining int) (string, []AgentRank) {
	inList := func(n string) bool {
		for _, c := range candidates {
			if c == n {
				return true
			}
		}
		return false
	}

	ranks := make([]AgentRank, 0, len(candidates))
	for _, c := range candidates {
		r := AgentRank{Agent: c, Preferred: c == preferred}
		if v, ok := hint.Remaining(c); ok {
			vv := v
			r.WeeklyRemaining = &vv
		}
		ranks = append(ranks, r)
	}

	pick := ""
	// Rule 1: preferred with headroom (or unknown usage).
	if preferred != "" && inList(preferred) {
		if v, ok := hint.Remaining(preferred); !ok || v >= minRemaining {
			pick = preferred
		}
	}
	// Rule 2: highest remaining at/above threshold.
	if pick == "" {
		best := -1
		for _, c := range candidates {
			if v, ok := hint.Remaining(c); ok && v >= minRemaining && v > best {
				best = v
				pick = c
			}
		}
	}
	// Rule 3: fallbacks.
	if pick == "" {
		switch {
		case preferred != "" && inList(preferred):
			pick = preferred
		case len(candidates) > 0:
			pick = candidates[0]
		}
	}

	for i := range ranks {
		if ranks[i].Agent == pick {
			ranks[i].Chosen = true
		}
	}
	return pick, ranks
}

// candidateAgents lists selectable agent names for a host: configured agents,
// dropping any the profile's probe marked not-present.
func candidateAgents(profile *HostProfile) []string {
	if profile == nil {
		return nil
	}
	var out []string
	for _, a := range profile.Agents {
		if pr, ok := profile.Probe[a.Name]; ok && !pr.Present {
			continue
		}
		out = append(out, a.Name)
	}
	return out
}

// Suggest ranks a host's agents by usage headroom and returns the recommended
// pick plus the full ranking. Safe with a nil/absent hook (falls back to the
// configured preferred / first candidate).
func Suggest(ctx context.Context, profile *HostProfile) (string, []AgentRank) {
	if profile == nil {
		return "", nil
	}
	min := profile.Defaults.UsageMinRemaining
	if min <= 0 {
		min = defaultUsageMinRemaining
	}
	hint, _ := LoadUsageHint(ctx, usageHookFor(profile))
	return SelectAgent(candidateAgents(profile), profile.Defaults.PreferredAgent, hint, min)
}
