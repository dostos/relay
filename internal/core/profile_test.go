package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalHostIDFromProfile(t *testing.T) {
	t.Setenv("RELAY_CONFIG_DIR", t.TempDir())
	if got := LocalHostIDFromProfile(); got != "" {
		t.Fatalf("missing profile host id = %q", got)
	}
	if err := os.WriteFile(filepath.Join(ConfigRoot(), "host.yaml"), []byte("version: 1\nhost_id: home-relay\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LocalHostIDFromProfile(); got != "home-relay" {
		t.Fatalf("profile host id = %q", got)
	}
}

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

func TestAgentLaunchHooksAreGeneralAndProviderAware(t *testing.T) {
	codex := (&AgentSpec{Name: "codex"}).LaunchCommand("goal")
	for _, want := range []string{"PermissionRequest", "relay hook --kind result", "relay\" signal exit", "--dangerously-bypass-approvals-and-sandbox"} {
		if !strings.Contains(codex, want) {
			t.Fatalf("codex command missing %q: %s", want, codex)
		}
	}
	claude := (&AgentSpec{Name: "claude"}).LaunchCommand("goal")
	if !strings.Contains(claude, "--settings") || !strings.Contains(claude, "permission_required") || !strings.Contains(claude, "--dangerously-skip-permissions") {
		t.Fatalf("claude hooks missing: %s", claude)
	}
	cursor := (&AgentSpec{Name: "cursor-agent"}).LaunchCommand("goal")
	if !strings.Contains(cursor, "signal exit") || !strings.Contains(cursor, "--force") {
		t.Fatalf("generic exit wrapper missing: %s", cursor)
	}
	off := (&AgentSpec{Name: "codex", RelayHooks: "off"}).LaunchCommand("goal")
	if strings.Contains(off, "PermissionRequest") || strings.Contains(off, "signal exit") {
		t.Fatalf("hooks off ignored: %s", off)
	}
	if !strings.Contains(off, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("hooks off also disabled autonomous permissions: %s", off)
	}
}

func TestAgentLaunchDoesNotDuplicateExplicitPermissionMode(t *testing.T) {
	cursor := (&AgentSpec{Name: "cursor-agent", Command: "cursor-agent --yolo"}).LaunchCommand("goal")
	if strings.Count(cursor, "--yolo") != 1 || strings.Contains(cursor, "--force") {
		t.Fatalf("cursor permission mode duplicated: %s", cursor)
	}
	claude := (&AgentSpec{Name: "claude", Args: []string{"--permission-mode", "bypassPermissions"}}).LaunchCommand("goal")
	if strings.Contains(claude, "--dangerously-skip-permissions") {
		t.Fatalf("claude permission mode overridden: %s", claude)
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
