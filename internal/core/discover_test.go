package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuggestPathsBoostAndDedupe(t *testing.T) {
	got := suggestPaths(
		[]string{"relay", "other"},
		[]string{"opaquebench", "relay"},
		"relay",
	)
	if len(got) < 3 {
		t.Fatalf("expected paths, got %#v", got)
	}
	if got[0].Match != "relay" || got[0].RemoteCWD != "~/dev/relay" {
		t.Fatalf("boost should prefer ~/dev/relay first, got %#v", got[0])
	}
	seen := map[string]int{}
	for _, e := range got {
		seen[e.Match]++
	}
	if seen["relay"] != 1 {
		t.Fatalf("relay should appear once: %#v", got)
	}
}

func TestExactBuildWorktreeIgnoresDirtyCallerTree(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Clean(filepath.Join(cwd, "..", ".."))
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	build := strings.TrimSpace(string(out))
	worktree, cleanup, err := exactBuildWorktree(context.Background(), repo, build)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := exec.Command("git", "-C", worktree, "rev-parse", "--short", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(got)) != build {
		t.Fatalf("worktree build=%q want=%q err=%v", got, build, err)
	}
}

func TestEnsurePingRequiresExpectedBuild(t *testing.T) {
	transport := &fakeTransport{id: "c3", outputs: map[string]string{"c3": `{"ok":true,"build":"old"}`}}
	if _, err := ensurePing(context.Background(), transport, "new"); err == nil || !strings.Contains(err.Error(), "did not land") {
		t.Fatalf("stale build accepted: %v", err)
	}
	transport.outputs["c3"] = `{"ok":true,"build":"new"}`
	if build, err := ensurePing(context.Background(), transport, "new"); err != nil || build != "new" {
		t.Fatalf("verified build=%q err=%v", build, err)
	}
}

func TestMergePathSuggestionsAddsOnly(t *testing.T) {
	existing := []PathMapEntry{{Match: "relay", RemoteCWD: "~/gh/relay"}}
	suggested := []PathMapEntry{
		{Match: "relay", RemoteCWD: "~/dev/relay"},
		{Match: "opaquebench", RemoteCWD: "~/gh/opaquebench"},
	}
	got := mergePathSuggestions(existing, suggested)
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].RemoteCWD != "~/gh/relay" {
		t.Fatalf("keep existing cwd, got %q", got[0].RemoteCWD)
	}
	if got[1].Match != "opaquebench" {
		t.Fatalf("expected add opaquebench, got %#v", got)
	}
}

func TestBuildProposalFromDetected(t *testing.T) {
	card := &DiscoverCard{
		HostID: "c9",
		AgentsDetected: []AgentDetect{
			{Name: "claude", Present: true, Authed: true, SuggestedSpec: &AgentSpec{Name: "claude", Command: "claude"}},
			{Name: "codex", Present: false},
		},
		PathSuggestions: []PathMapEntry{{Match: "phyzfuzz", RemoteCWD: "~/dev/phyzfuzz"}},
	}
	p := buildProposal("c9", card)
	if p.HostID != "c9" || len(p.Agents) != 1 || p.Agents[0].Name != "claude" {
		t.Fatalf("proposal agents wrong: %#v", p.Agents)
	}
	if p.Defaults.PreferredAgent != "claude" {
		t.Fatalf("preferred %q", p.Defaults.PreferredAgent)
	}
	if len(p.PathMap) != 1 || p.PathMap[0].Match != "phyzfuzz" {
		t.Fatalf("path_map %#v", p.PathMap)
	}
}

func TestFormatDiscoverText(t *testing.T) {
	s := FormatDiscoverText(&DiscoverCard{
		HostID:    "c1",
		Reachable: true,
		HostYAML:  "missing",
		Relayd:    "ok",
		Tmux:      TmuxInfo{Present: true},
		AgentsDetected: []AgentDetect{
			{Name: "claude", Present: true, Authed: true},
		},
		Next: "relay host init -H c1 --apply",
	})
	if !strings.Contains(s, "host c1") || !strings.Contains(s, "next") {
		t.Fatalf("bad summary: %s", s)
	}
}
