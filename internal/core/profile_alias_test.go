package core

import (
	"strings"
	"testing"
)

func profileWithAgents(names ...string) *HostProfile {
	p := &HostProfile{}
	for _, n := range names {
		p.Agents = append(p.Agents, AgentSpec{Name: n, Command: n})
	}
	return p
}

func TestFindAgentExactStillWins(t *testing.T) {
	p := profileWithAgents("claude", "cursor-agent", "codex")
	ag, err := p.FindAgent("cursor-agent")
	if err != nil || ag.Name != "cursor-agent" {
		t.Fatalf("exact match failed: ag=%v err=%v", ag, err)
	}
}

func TestFindAgentPrefixAlias(t *testing.T) {
	p := profileWithAgents("claude", "cursor-agent", "codex")
	ag, err := p.FindAgent("cursor")
	if err != nil {
		t.Fatalf("alias \"cursor\" should resolve to cursor-agent: %v", err)
	}
	if ag.Name != "cursor-agent" {
		t.Fatalf("want cursor-agent, got %q", ag.Name)
	}
}

func TestFindAgentSoleCCSProfileAlias(t *testing.T) {
	p := profileWithAgents("claude", "ccs:personal")
	ag, err := p.FindAgent("ccs")
	if err != nil || ag.Name != "ccs:personal" {
		t.Fatalf("ccs should resolve to lone ccs:personal: ag=%v err=%v", ag, err)
	}
}

func TestFindAgentBinBaseNameAlias(t *testing.T) {
	// Agent listed under a friendly name but launched via a pathful binary.
	p := &HostProfile{Agents: []AgentSpec{{Name: "grok", Command: "/home/u/.local/bin/cursor-agent --model grok"}}}
	ag, err := p.FindAgent("cursor-agent")
	if err != nil || ag.Name != "grok" {
		t.Fatalf("bin base name should match: ag=%v err=%v", ag, err)
	}
}

func TestFindAgentAmbiguousErrors(t *testing.T) {
	p := profileWithAgents("claude", "codex")
	_, err := p.FindAgent("c")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous alias should error with 'ambiguous', got %v", err)
	}
	if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("error should list available agents, got %v", err)
	}
}

func TestFindAgentMissListsAvailable(t *testing.T) {
	p := profileWithAgents("claude", "cursor-agent")
	_, err := p.FindAgent("nope")
	if err == nil || !strings.Contains(err.Error(), "available:") {
		t.Fatalf("miss should list available agents, got %v", err)
	}
}
