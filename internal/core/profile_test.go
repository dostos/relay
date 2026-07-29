package core

import (
	"strings"
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
	if !strings.Contains(cmd, "bash -ilc") || !strings.Contains(cmd, "ccs personal") {
		t.Fatalf("got %q", cmd)
	}
}

func TestLoginCommandPerAgent(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"claude", "claude auth login"},
		{"cursor-agent", "cursor-agent login"},
		{"codex", "codex login"},
		{"ccs:personal", "ccs auth create personal --force"},
		{"ccs:hcs", "ccs auth create hcs --force"},
	}
	for _, c := range cases {
		got := LoginCommand(AgentSpec{Name: c.name})
		if !strings.Contains(got, "bash -ilc") || !strings.Contains(got, c.want) {
			t.Fatalf("%s: got %q want substring %q", c.name, got, c.want)
		}
	}
}

func TestCredentialPathsCCS(t *testing.T) {
	paths := CredentialPaths(AgentSpec{Name: "ccs:hcs"})
	if len(paths) != 1 || !strings.Contains(paths[0], "instances/hcs/.credentials.json") {
		t.Fatalf("got %#v", paths)
	}
	if len(CredentialPaths(AgentSpec{Name: "claude"})) != 0 {
		t.Fatal("claude copy should be unsupported")
	}
}
