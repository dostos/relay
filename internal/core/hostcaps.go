package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/ports"
)

// HostCaps is the cheap capability probe Forge (and other clients) use before
// offering container sessions on a host.
type HostCaps struct {
	OK           bool             `json:"ok"`
	HostID       string           `json:"host_id"`
	Runtime      string           `json:"runtime"`
	Docker       DockerCaps       `json:"docker"`
	Devcontainer DevcontainerCaps `json:"devcontainer_cli"`
	GPU          GPUCaps          `json:"gpu"`
	Detail       string           `json:"detail,omitempty"`
}

// DockerCaps reports docker CLI + daemon reachability for the login user.
type DockerCaps struct {
	Present    bool   `json:"present"`
	Version    string `json:"version,omitempty"`
	Daemon     bool   `json:"daemon"`
	UserCanRun bool   `json:"user_can_run"`
	Reason     string `json:"reason,omitempty"`
}

// DevcontainerCaps reports the Dev Containers CLI on the interactive PATH.
type DevcontainerCaps struct {
	Present bool   `json:"present"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// GPUCaps reports whether an NVIDIA runtime is visible on the host.
type GPUCaps struct {
	Nvidia bool   `json:"nvidia"`
	Reason string `json:"reason,omitempty"`
}

// HostCapsCommand is a single login-shell script that probes docker, the
// devcontainer CLI and nvidia in one ssh round trip — the same PATH
// HostLoginShell gives `container up`.
func HostCapsCommand() string {
	const script = `set +e
echo RELAY_CAPS_BEGIN
if command -v docker >/dev/null 2>&1; then
  echo DOCKER_PRESENT yes
  echo DOCKER_VERSION "$(docker --version 2>/dev/null | head -n1)"
  if docker info >/dev/null 2>&1; then
    echo DOCKER_DAEMON yes
    echo DOCKER_USER yes
  else
    echo DOCKER_DAEMON no
    echo DOCKER_USER no
    if docker info 2>&1 | grep -qi 'permission denied\|dial unix'; then
      echo DOCKER_REASON docker group missing or daemon socket not usable by this user
    else
      echo DOCKER_REASON docker daemon not reachable
    fi
  fi
else
  echo DOCKER_PRESENT no
  echo DOCKER_DAEMON no
  echo DOCKER_USER no
  echo DOCKER_REASON docker not found on PATH; install docker
fi
dc_path=$(command -v devcontainer 2>/dev/null)
if [ -n "$dc_path" ]; then
  echo DC_PRESENT yes
  echo DC_PATH "$dc_path"
  echo DC_VERSION "$(devcontainer --version 2>/dev/null | head -n1)"
else
  echo DC_PRESENT no
  echo DC_REASON devcontainer CLI not found: install it (npm i -g @devcontainers/cli, needs node >= 18) or set devcontainer.cli to its absolute path
fi
if command -v nvidia-smi >/dev/null 2>&1 || docker info 2>/dev/null | grep -qi nvidia; then
  echo GPU_NVIDIA yes
else
  echo GPU_NVIDIA no
  echo GPU_REASON no nvidia-smi and no nvidia runtime reported by docker info
fi
echo RELAY_CAPS_END`
	return HostLoginShell(script)
}

// ParseHostCaps reads the line-oriented probe HostCapsCommand prints.
func ParseHostCaps(hostID, output string) (*HostCaps, error) {
	caps := &HostCaps{HostID: hostID, Runtime: "docker"}
	in := false
	found := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "RELAY_CAPS_BEGIN":
			in, found = true, true
			continue
		case line == "RELAY_CAPS_END":
			in = false
			continue
		}
		if !in {
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		val = strings.TrimSpace(val)
		switch key {
		case "DOCKER_PRESENT":
			caps.Docker.Present = val == "yes"
		case "DOCKER_VERSION":
			caps.Docker.Version = val
		case "DOCKER_DAEMON":
			caps.Docker.Daemon = val == "yes"
		case "DOCKER_USER":
			caps.Docker.UserCanRun = val == "yes"
		case "DOCKER_REASON":
			caps.Docker.Reason = val
		case "DC_PRESENT":
			caps.Devcontainer.Present = val == "yes"
		case "DC_PATH":
			caps.Devcontainer.Path = val
		case "DC_VERSION":
			caps.Devcontainer.Version = val
		case "DC_REASON":
			caps.Devcontainer.Reason = val
		case "GPU_NVIDIA":
			caps.GPU.Nvidia = val == "yes"
		case "GPU_REASON":
			caps.GPU.Reason = val
		}
	}
	if !found {
		return nil, fmt.Errorf("host caps produced no result block")
	}
	caps.OK = caps.Docker.Present && caps.Docker.Daemon && caps.Docker.UserCanRun
	return caps, nil
}

// Caps probes one host's container-session capabilities.
func (p *ProfileService) Caps(ctx context.Context, hostID string) (*HostCaps, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	if p == nil || p.NewTransport == nil {
		return nil, fmt.Errorf("profile service is not configured")
	}
	t, err := p.NewTransport(hostID)
	if err != nil {
		return nil, err
	}
	return CapsOn(ctx, t, hostID)
}

// CapsOn is Caps against an already-open transport (tests).
func CapsOn(ctx context.Context, t ports.Transport, hostID string) (*HostCaps, error) {
	out, errOut, err := t.Run(ctx, "", HostCapsCommand())
	combined := strings.TrimSpace(out + "\n" + errOut)
	caps, perr := ParseHostCaps(hostID, combined)
	if perr != nil {
		if err != nil {
			return nil, fmt.Errorf("host caps on %s: %w\n--- output ---\n%s", hostID, err, combined)
		}
		return nil, fmt.Errorf("host caps on %s: %w\n--- output ---\n%s", hostID, perr, combined)
	}
	if err != nil && !caps.OK {
		caps.Detail = err.Error()
	}
	return caps, nil
}
