package core

import (
	"testing"
)

func TestParseAndResolvePathMap(t *testing.T) {
	yaml := `
version: 1
host_id: c1
agents:
  - name: claude
    command: claude
path_map:
  - match: relay
    remote_cwd: ~/gh/relay
  - match: opaquebench
    remote_cwd: ~/gh/opaquebench
defaults:
  preferred_agent: claude
  silence_sec: 45
`
	p, err := ParseHostProfileYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := p.ResolveRemoteCWD("/Users/jingyu/dev/relay")
	if err != nil {
		t.Fatal(err)
	}
	if cwd != "~/gh/relay" {
		t.Fatalf("got %q", cwd)
	}
	ag, err := p.FindAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if ag.Name != "claude" {
		t.Fatalf("preferred agent %q", ag.Name)
	}
}

func TestFindAgentMissing(t *testing.T) {
	p := &HostProfile{Agents: []AgentSpec{{Name: "codex"}}}
	_, err := p.FindAgent("claude")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAgentLaunchCCS(t *testing.T) {
	a := &AgentSpec{Name: "ccs:personal"}
	cmd := a.LaunchCommand("do thing")
	if cmd != "ccs personal" {
		t.Fatalf("got %q", cmd)
	}
}
