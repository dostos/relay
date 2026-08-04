package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// BootstrapResult summarizes a quiet single-host relayd install.
type BootstrapResult struct {
	HostID      string `json:"host_id"`
	Arch        string `json:"arch"`
	Binary      string `json:"binary"`
	RelayBinary string `json:"relay_binary,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Started     bool   `json:"started"`
	PingOK      bool   `json:"ping_ok"`
	Version     string `json:"version,omitempty"`
	Build       string `json:"build,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// BootstrapService installs the primary relay binary and its host-local event
// coordinator on one host. A relayd pathname is retained only as a symlink to
// the same executable during the compatibility window.
type BootstrapService struct {
	NewTransport TransportFactory
	// LocalRelayRepo is the checkout containing cmd/relay (defaults to sibling of cwd or RELAY_REPO).
	LocalRelayRepo string
}

func (b *BootstrapService) Bootstrap(ctx context.Context, hostID string) (*BootstrapResult, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	t, err := b.NewTransport(hostID)
	if err != nil {
		return nil, err
	}
	out := &BootstrapResult{HostID: hostID}

	// 1) one uname
	stdout, stderr, err := t.Run(ctx, "", "uname -m")
	if err != nil {
		return nil, fmt.Errorf("uname: %w (%s)", err, strings.TrimSpace(stderr))
	}
	remoteArch := strings.TrimSpace(stdout)
	out.Arch = remoteArch
	goarch := mapUnameToGoarch(remoteArch)
	if goarch == "" {
		return nil, fmt.Errorf("unsupported remote arch %q", remoteArch)
	}

	// 2) build the matching primary Linux binary locally. It serves both the
	// stateless client and the host-local event coordinator.
	repo := b.LocalRelayRepo
	if repo == "" {
		repo = os.Getenv("RELAY_REPO")
	}
	if repo == "" {
		// try common locations
		for _, c := range []string{
			filepath.Join(os.Getenv("HOME"), "dev", "relay"),
			".",
		} {
			if _, err := os.Stat(filepath.Join(c, "cmd", "relay")); err == nil {
				repo = c
				break
			}
		}
	}
	if repo == "" {
		return nil, fmt.Errorf("cannot find relay repo (set RELAY_REPO)")
	}
	buildRepo, cleanupBuildRepo, err := exactBuildWorktree(ctx, repo, coord.Build)
	if err != nil {
		return nil, err
	}
	defer cleanupBuildRepo()
	tmpBase := filepath.Join(os.TempDir(), fmt.Sprintf("relay-bootstrap-%s-%d", goarch, time.Now().UnixNano()))
	tmpRelay := tmpBase + "-relay"
	// Stamp the remote with the build of the relay doing the deploying, so
	// "remote build == local build" means exactly "this host was deployed from
	// the relay I am running". Without it every remote reports the default and
	// doctor's drift check fails forever, which would train the reader to
	// ignore it.
	ldflags := "-X github.com/dostos/relay/internal/coord.Build=" + coord.Build
	build := func(output, pkg string) error {
		cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", output, pkg)
		cmd.Dir = buildRepo
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS=linux",
			"GOARCH="+goarch,
		)
		if bOut, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w (%s)", pkg, err, string(bOut))
		}
		return nil
	}
	if err := build(tmpRelay, "./cmd/relay"); err != nil {
		return nil, err
	}
	defer os.Remove(tmpRelay)
	out.Binary = "~/.local/bin/relay"
	out.RelayBinary = "~/.local/bin/relay"

	// 3) upload via SSH cat to a temporary name (avoids ETXTBSY).
	relayData, err := os.ReadFile(tmpRelay)
	if err != nil {
		return nil, err
	}
	if err := t.WriteFile(ctx, "~/.local/bin/relay.new", relayData, "755"); err != nil {
		return nil, fmt.Errorf("upload relay: %w", err)
	}

	// 4) unit + atomic replace + start/restart (still one SSH script).
	unit := `[Unit]
Description=Relay host-local event coordinator
After=default.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=%h/.local/bin/relay service event run
Restart=on-failure
RestartSec=5
Environment=RELAYD_SOCK=%h/.local/state/relay/relayd.sock

[Install]
WantedBy=default.target
`
	_ = t.WriteFile(ctx, "~/.config/systemd/user/relay-event.service", []byte(unit), "644")

	startScript := `
set -e
RELAY="$HOME/.local/bin/relay"
mkdir -p "$HOME/.local/state/relay/events" "$HOME/.config/systemd/user" "$HOME/.local/bin"
mv -f "$HOME/.local/bin/relay.new" "$RELAY"
chmod 755 "$RELAY"
ln -sfn relay "$HOME/.local/bin/relayd.new"
mv -Tf "$HOME/.local/bin/relayd.new" "$HOME/.local/bin/relayd"
if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  systemctl --user daemon-reload
  systemctl --user disable --now relayd.service >/dev/null 2>&1 || true
  systemctl --user enable relay-event.service
  systemctl --user restart relay-event.service
  echo UNIT=systemd
else
  # stop prior nohup if any, then start once
  if "$RELAY" service event ping >/dev/null 2>&1; then
    pkill -f '[r]elayd serve' 2>/dev/null || true
    pkill -f '[r]elay service event run' 2>/dev/null || true
    sleep 0.3
  fi
  nohup "$RELAY" service event run >/tmp/relay-event.log 2>&1 &
  sleep 0.5
  echo UNIT=nohup
fi
"$RELAY" service event ping
`
	stdout, stderr, err = t.Run(ctx, "", startScript)
	unitLine := firstLinePrefix(stdout, "UNIT=")
	if strings.Contains(stdout, "UNIT=systemd") {
		out.Unit = "systemd-user"
	} else {
		out.Unit = "nohup"
	}
	// Keep Detail quiet on success — mid-restart "socket missing" noise is expected.
	if unitLine != "" {
		out.Detail = unitLine
	} else {
		out.Detail = strings.TrimSpace(stdout + "\n" + stderr)
	}
	// Brief settle after restart before the verify ping (socket recreate).
	time.Sleep(400 * time.Millisecond)

	// 5) one ping verify
	remoteBuild, err := ensurePing(ctx, t, coord.Build)
	if err != nil {
		out.PingOK = false
		out.Started = false
		out.Detail = strings.TrimSpace(stdout + "\n" + stderr + "\n" + err.Error())
		return out, err
	}
	out.PingOK = true
	out.Started = true
	out.Version = "0.1.0"
	out.Build = remoteBuild
	return out, nil
}

func exactBuildWorktree(ctx context.Context, repo, build string) (string, func(), error) {
	if build == "" || build == "dev" || strings.Contains(build, "dirty") {
		return "", nil, fmt.Errorf("refuse bootstrap from unverifiable build %q", build)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", build+"^{commit}").CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("resolve deployed build %s: %w (%s)", build, err, strings.TrimSpace(string(out)))
	}
	path, err := os.MkdirTemp("", "relay-bootstrap-source-")
	if err != nil {
		return "", nil, err
	}
	if err := os.Remove(path); err != nil {
		return "", nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "--detach", path, build)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("prepare exact bootstrap source: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	cleanup := func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", path).Run()
	}
	return path, cleanup, nil
}

func firstLinePrefix(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func ensurePing(ctx context.Context, t ports.Transport, expectedBuild string) (string, error) {
	stdout, stderr, err := t.Run(ctx, "", `"$HOME/.local/bin/relay" service event ping`)
	if err != nil {
		return "", fmt.Errorf("ping failed: %w (%s)", err, strings.TrimSpace(stderr))
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Build string `json:"build"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp) != nil || !resp.OK {
		return "", fmt.Errorf("unexpected ping output: %s", strings.TrimSpace(stdout+stderr))
	}
	if resp.Build == "" || resp.Build != expectedBuild {
		return "", fmt.Errorf("relay event service update did not land: running build %q, expected %q", resp.Build, expectedBuild)
	}
	return resp.Build, nil
}

func mapUnameToGoarch(u string) string {
	switch strings.ToLower(u) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l":
		return "arm"
	default:
		return ""
	}
}
