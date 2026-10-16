package core

import (
	"strings"
	"testing"
)

func TestComposeAgentRecipesKnownAgents(t *testing.T) {
	got, err := ComposeAgentRecipes([]string{"claude", "codex", "ccs", "cursor-agent"}, "/relay/toolkit")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"RELAY_TOOLKIT='/relay/toolkit'",
		"@anthropic-ai/claude-code",
		"@openai/codex",
		"cursor.com/install",
		"ln -sfn",
		"cursor-agent",
		"# recipe:claude",
		"# recipe:ccs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestComposeAgentRecipesRejectsUnknown(t *testing.T) {
	if _, err := ComposeAgentRecipes([]string{"claude", "nope"}, "/t"); err == nil {
		t.Fatal("expected unknown recipe to be refused")
	}
}

func TestToolkitResolvedProvisionAgentsVsRaw(t *testing.T) {
	raw := &ToolkitSpec{Target: "/t", Provision: "echo raw", Agents: []string{"claude"}}
	got, err := raw.ResolvedProvision()
	if err != nil || got != "echo raw" {
		t.Fatalf("raw provision must win: %q %v", got, err)
	}
	recipe := &ToolkitSpec{Target: "/t", Agents: []string{"codex"}}
	got, err = recipe.ResolvedProvision()
	if err != nil || !strings.Contains(got, "@openai/codex") {
		t.Fatalf("agents recipe: %q %v", got, err)
	}
}

func TestApplyAgentVolumesInheritsAndOptsOut(t *testing.T) {
	off := false
	p := &HostProfile{
		AgentVolumes: &AgentVolumesSpec{
			Toolkit: &ToolkitSpec{Volume: "relay-toolkit", Target: "/relay/toolkit", Agents: []string{"claude"}},
			Home:    &HomeSpec{Volume: "relay-home", Target: "/relay/home"},
		},
		Containers: []ContainerSpec{
			{Name: "img", Image: "alpine"},
			{Name: "own", Image: "alpine", Home: &HomeSpec{Volume: "mine", Target: "/home/x"}},
			{Name: "off", Image: "alpine", AgentVolumes: &off},
		},
	}
	img, err := p.ResolveContainer("img")
	if err != nil {
		t.Fatal(err)
	}
	if img.Toolkit == nil || img.Toolkit.Volume != "relay-toolkit" || img.Home == nil || img.Home.Volume != "relay-home" {
		t.Fatalf("inherit failed: %+v toolkit=%+v home=%+v", img, img.Toolkit, img.Home)
	}
	if img.Toolkit.Provision == "" || !strings.Contains(img.Toolkit.Provision, "claude-code") {
		t.Fatalf("recipes not composed on inherit: %q", img.Toolkit.Provision)
	}
	own, err := p.ResolveContainer("own")
	if err != nil {
		t.Fatal(err)
	}
	if own.Home.Volume != "mine" || own.Toolkit == nil || own.Toolkit.Volume != "relay-toolkit" {
		t.Fatalf("partial override: home=%+v toolkit=%+v", own.Home, own.Toolkit)
	}
	opt, err := p.ResolveContainer("off")
	if err != nil {
		t.Fatal(err)
	}
	if opt.Toolkit != nil || opt.Home != nil {
		t.Fatalf("opt-out must leave volumes unset: %+v", opt)
	}
}

func TestAuthSyncAllowlistExcludesHooksAndPlugins(t *testing.T) {
	joined := ""
	for _, f := range AuthSyncAllowlist() {
		joined += f.HostRel + "\n"
	}
	for _, forbidden := range []string{
		".claude/hooks", ".claude/plugins", ".claude/settings.json",
		"mcp.json", ".claude/mcp", "CLAUDE.md",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("allowlist must not include %q:\n%s", forbidden, joined)
		}
	}
	for _, want := range []string{
		".claude/.credentials.json", ".claude.json",
		".codex/auth.json", ".codex/config.toml",
		".ccs", ".cursor/auth.json", ".config/cursor/auth.json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("allowlist missing %q", want)
		}
	}
}

func TestAuthSyncFilesForFilter(t *testing.T) {
	files, err := AuthSyncFilesFor("claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Agent != "claude" {
			t.Fatalf("filter leaked %q", f.Agent)
		}
	}
	if _, err := AuthSyncFilesFor("nope"); err == nil {
		t.Fatal("expected unknown agent refusal")
	}
	all, err := AuthSyncFilesFor("cursor-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[0].Agent != "cursor" {
		t.Fatalf("cursor-agent should map to cursor: %+v", all)
	}
}

func TestAuthSyncCommandNeverBindMountsDotClaude(t *testing.T) {
	files, _ := AuthSyncFilesFor("claude")
	cmd, err := AuthSyncCommand("docker", "relay-home", "/relay/home", "1005", files, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "$HOME/.claude:") || strings.Contains(cmd, "~/.claude:") {
		t.Fatalf("must not bind-mount the whole ~/.claude tree:\n%s", cmd)
	}
	if !strings.Contains(cmd, ".claude/.credentials.json") {
		t.Fatalf("must copy the credentials file:\n%s", cmd)
	}
	if !strings.Contains(cmd, "bash -ilc") {
		t.Fatalf("must run under login shell:\n%s", cmd)
	}
	dry, err := AuthSyncCommand("docker", "relay-home", "/relay/home", "", files, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry, "WOULD_COPY") || strings.Contains(dry, "docker run") {
		t.Fatalf("dry-run must not run containers:\n%s", dry)
	}
}

func TestToolkitProvisionUsesRecipes(t *testing.T) {
	spec := &ContainerSpec{
		Name: "x",
		Toolkit: &ToolkitSpec{
			Volume: "relay-toolkit",
			Target: "/relay/toolkit",
			Agents: []string{"claude", "cursor-agent"},
		},
	}
	ref := spec.RefFor("cid", "")
	cmd, err := ToolkitProvisionCommand(spec, ref, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "claude-code") || !strings.Contains(cmd, "cursor.com/install") {
		t.Fatalf("recipes missing from provision:\n%s", cmd)
	}
}

func TestParseAgentStatusLines(t *testing.T) {
	rows := ParseAgentStatusLines("AGENT claude present yes 2.1.197\nAGENT codex absent no\nnoise\n")
	if len(rows) != 2 {
		t.Fatalf("got %+v", rows)
	}
	if !rows[0].Binary || !rows[0].Authed || rows[0].Version != "2.1.197" {
		t.Errorf("claude: %+v", rows[0])
	}
	if rows[1].Binary || rows[1].Authed {
		t.Errorf("codex: %+v", rows[1])
	}
}

func TestAgentVolumesYAMLRoundTrip(t *testing.T) {
	raw := []byte(`version: 1
host_id: h
agent_volumes:
  toolkit:
    volume: relay-toolkit
    target: /relay/toolkit
    agents: [claude, codex, ccs, cursor-agent]
  home:
    volume: relay-home
    target: /relay/home
containers:
  - name: hammer
    image: alpine
  - name: bare
    image: alpine
    agent_volumes: false
`)
	p, err := ParseHostProfileYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.AgentVolumes == nil || p.AgentVolumes.Toolkit == nil || len(p.AgentVolumes.Toolkit.Agents) != 4 {
		t.Fatalf("agent_volumes not parsed: %+v", p.AgentVolumes)
	}
	hammer, err := p.ResolveContainer("hammer")
	if err != nil || hammer.Toolkit == nil || hammer.Home == nil {
		t.Fatalf("hammer inherit: %+v %v", hammer, err)
	}
	bare, err := p.ResolveContainer("bare")
	if err != nil || bare.Toolkit != nil || bare.Home != nil {
		t.Fatalf("bare opt-out: %+v %v", bare, err)
	}
}
