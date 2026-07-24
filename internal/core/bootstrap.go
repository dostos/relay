package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// BootstrapResult summarizes a quiet single-host relayd install.
type BootstrapResult struct {
	HostID   string `json:"host_id"`
	Arch     string `json:"arch"`
	Binary   string `json:"binary"`
	Unit     string `json:"unit,omitempty"`
	Started  bool   `json:"started"`
	PingOK   bool   `json:"ping_ok"`
	Version  string `json:"version,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// BootstrapService installs always-on relayd on one host (IT-safe: single SSH upload).
type BootstrapService struct {
	NewTransport TransportFactory
	// LocalRelayRepo is the checkout containing cmd/relayd (defaults to sibling of cwd or RELAY_REPO).
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

	// 2) build matching linux binary locally
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
			if _, err := os.Stat(filepath.Join(c, "cmd", "relayd")); err == nil {
				repo = c
				break
			}
		}
	}
	if repo == "" {
		return nil, fmt.Errorf("cannot find relay repo (set RELAY_REPO)")
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("relayd-%s-%d", goarch, time.Now().UnixNano()))
	cmd := exec.CommandContext(ctx, "go", "build", "-o", tmp, "./cmd/relayd")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+goarch,
	)
	if runtime.GOOS == "linux" && runtime.GOARCH == goarch {
		// native build ok
	}
	if bOut, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build relayd: %w (%s)", err, string(bOut))
	}
	defer os.Remove(tmp)
	out.Binary = "~/.local/bin/relayd"

	// 3) one upload via ssh cat to a temp name (avoids ETXTBSY on running binary)
	data, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	if err := t.WriteFile(ctx, "~/.local/bin/relayd.new", data, "755"); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}

	// 4) unit + atomic replace + start/restart (still one SSH script)
	unit := `[Unit]
Description=relayd event coordinator (Unix socket only)
After=default.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=%h/.local/bin/relayd serve
Restart=on-failure
RestartSec=5
Environment=RELAYD_SOCK=%h/.local/state/relay/relayd.sock

[Install]
WantedBy=default.target
`
	_ = t.WriteFile(ctx, "~/.config/systemd/user/relayd.service", []byte(unit), "644")

	startScript := `
set -e
RELAYD="$HOME/.local/bin/relayd"
mkdir -p "$HOME/.local/state/relay/events" "$HOME/.config/systemd/user" "$HOME/.local/bin"
mv -f "$HOME/.local/bin/relayd.new" "$RELAYD"
chmod 755 "$RELAYD"
if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  systemctl --user daemon-reload
  systemctl --user enable relayd.service
  systemctl --user restart relayd.service
  echo UNIT=systemd
else
  # stop prior nohup if any, then start once
  if "$RELAYD" ping >/dev/null 2>&1; then
    pkill -f '[r]elayd serve' 2>/dev/null || true
    sleep 0.3
  fi
  nohup "$RELAYD" serve >/tmp/relayd.log 2>&1 &
  sleep 0.5
  echo UNIT=nohup
fi
"$RELAYD" ping
`
	stdout, stderr, err = t.Run(ctx, "", startScript)
	out.Detail = strings.TrimSpace(stdout + "\n" + stderr)
	if strings.Contains(stdout, "UNIT=systemd") {
		out.Unit = "systemd-user"
	} else {
		out.Unit = "nohup"
	}
	// Brief settle after restart before the verify ping (socket recreate).
	time.Sleep(400 * time.Millisecond)

	// 5) one ping verify
	if err := ensurePing(ctx, t); err != nil {
		out.PingOK = false
		out.Started = false
		out.Detail += "\n" + err.Error()
		return out, err
	}
	out.PingOK = true
	out.Started = true
	out.Version = "0.1.0"
	return out, nil
}

func ensurePing(ctx context.Context, t ports.Transport) error {
	stdout, stderr, err := t.Run(ctx, "", `"$HOME/.local/bin/relayd" ping`)
	if err != nil {
		return fmt.Errorf("ping failed: %w (%s)", err, strings.TrimSpace(stderr))
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp) != nil || !resp.OK {
		return fmt.Errorf("unexpected ping output: %s", strings.TrimSpace(stdout+stderr))
	}
	return nil
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
