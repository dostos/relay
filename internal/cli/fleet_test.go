package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// homeTransport simulates a remote host: every script runs locally under
// its own HOME, the way ssh would run it on the host.
type homeTransport struct {
	home string
	fail bool
}

func (h *homeTransport) ID() string { return "sim" }
func (h *homeTransport) Run(ctx context.Context, _, command string) (string, string, error) {
	if h.fail {
		return "", "ssh: connect to host sim port 22: No route to host", &exec.ExitError{}
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Env = []string{"HOME=" + h.home, "PATH=" + filepath.Join(h.home, ".local/bin") + ":/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/bash", "RELAY_USAGE_LOG=0"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}
func (h *homeTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (h *homeTransport) ReadFile(context.Context, string) ([]byte, error)           { return nil, nil }
func (h *homeTransport) WriteFile(_ context.Context, path string, data []byte, mode string) error {
	p := filepath.Join(h.home, strings.TrimPrefix(path, "~/"))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o755)
}
func (h *homeTransport) Interactive(context.Context, string) error { return nil }

func TestFleetStatusProbe(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	_ = os.MkdirAll(bin, 0o755)
	fake := "#!/bin/sh\ncase \"$*\" in build) echo oldbuild;; '--json version') echo '{\"ok\":true,\"client\":{\"capabilities\":[\"session.submit\"]}}';; *) exit 1;; esac\n"
	_ = os.WriteFile(filepath.Join(bin, "relay"), []byte(fake), 0o755)
	tr := map[string]ports.Transport{"h1": &homeTransport{home: home}, "down": &homeTransport{fail: true}}
	app := &App{tf: func(h string) (ports.Transport, error) { return tr[h], nil }, noUsage: true}
	res := app.fleetRun(context.Background(), []string{"h1", "down"}, fleetOpts{parallel: 2}, false)
	h1 := res[0]
	if !h1.Reachable || h1.InstalledBuild != "oldbuild" || h1.UpToDate || h1.Platform == "" || !strings.HasSuffix(h1.OnSSHPath, "/.local/bin/relay") {
		t.Fatalf("h1 = %+v", h1)
	}
	if len(h1.MissingCaps) == 0 {
		t.Fatalf("missing capabilities not computed: %+v", h1)
	}
	if res[1].Reachable || res[1].Error == "" {
		t.Fatalf("down = %+v", res[1])
	}
	out := captureStdout(t, func() { app.JSON = true; app.CompactJSON = true; _ = app.printFleet("status", fleetOpts{}, res) })
	var parsed struct {
		Summary map[string]int `json:"summary"`
		Argv    []string       `json:"argv"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || parsed.Summary["unreachable"] != 1 || parsed.Summary["behind"] != 1 || len(parsed.Argv) == 0 {
		t.Fatalf("status json: %s", out)
	}
}

// TestFleetUpgradeE2E cross-compiles the current commit, "ships" it into
// simulated host homes, and checks install, verify, idempotency and agent
// guidance. It builds Go, so it is opt-in: RELAY_FLEET_E2E=1.
func TestFleetUpgradeE2E(t *testing.T) {
	if os.Getenv("RELAY_FLEET_E2E") != "1" {
		t.Skip("set RELAY_FLEET_E2E=1")
	}
	repo, _ := filepath.Abs("../..")
	head, err := exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Skip("no git")
	}
	old := coord.Build
	coord.Build = strings.TrimSpace(string(head))
	t.Cleanup(func() { coord.Build = old })
	t.Setenv("RELAY_REPO", repo)
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	homes := map[string]*homeTransport{"a": {home: t.TempDir()}, "b": {home: t.TempDir()}, "down": {fail: true}}
	// host b has Claude Code and Codex configured and a user AGENTS.md
	_ = os.MkdirAll(filepath.Join(homes["b"].home, ".claude"), 0o755)
	_ = os.MkdirAll(filepath.Join(homes["b"].home, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(homes["b"].home, ".codex", "AGENTS.md"), []byte("# mine\n"), 0o644)
	app := &App{tf: func(h string) (ports.Transport, error) { return homes[h], nil }, noUsage: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dry := app.fleetRun(ctx, []string{"a", "b", "down"}, fleetOpts{parallel: 3, dryRun: true}, true)
	if !strings.HasPrefix(dry[0].Action, "would_upgrade") {
		t.Fatalf("dry run: %+v", dry[0])
	}
	if _, err := os.Stat(filepath.Join(homes["a"].home, ".local/bin/relay")); err == nil {
		t.Fatal("dry run installed")
	}
	res := app.fleetRun(ctx, []string{"a", "b", "down"}, fleetOpts{parallel: 3}, true)
	for _, r := range res[:2] {
		if r.Action != "upgraded" || r.AfterBuild != coord.Build || r.AgentsOK == nil || !*r.AgentsOK {
			t.Fatalf("%s: %+v agents=%s", r.Host, r, r.Agents)
		}
	}
	if res[2].Action != "unreachable" {
		t.Fatalf("down: %+v", res[2])
	}
	if _, err := os.Stat(filepath.Join(homes["b"].home, ".claude/skills/relay/SKILL.md")); err != nil {
		t.Fatal("claude skill not installed on b")
	}
	agentsMD, _ := os.ReadFile(filepath.Join(homes["b"].home, ".codex/AGENTS.md"))
	if !strings.HasPrefix(string(agentsMD), "# mine\n") || !strings.Contains(string(agentsMD), "relay managed") {
		t.Fatalf("AGENTS.md: %s", agentsMD)
	}
	again := app.fleetRun(ctx, []string{"a", "b"}, fleetOpts{parallel: 2}, true)
	for _, r := range again {
		if r.Action != "current" || !r.UpToDate {
			t.Fatalf("second upgrade not idempotent: %+v", r)
		}
	}
}
