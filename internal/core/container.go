package core

import (
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// ContainerRef is the resolved container binding carried on a Session.
type ContainerRef struct {
	Runtime string `json:"runtime"`        // docker (default)
	Ref     string `json:"ref"`            // container name/id to `docker exec` into
	CWD     string `json:"cwd,omitempty"`  // working dir inside the container
	User    string `json:"user,omitempty"` // exec uid[:gid]
	Home    string `json:"home,omitempty"` // container $HOME (for cred resolution)
	// PathPrepend is prepended to PATH inside the container. It is how agent
	// CLIs installed into a toolkit named volume become resolvable without
	// changing the image.
	PathPrepend string `json:"path_prepend,omitempty"`
	// SpecName is the host.yaml `containers:` handle this ref resolved from,
	// so teardown can find the spec again without re-deriving it.
	SpecName string `json:"spec_name,omitempty"`
	// Ephemeral marks a container whose lifetime is this session's: relay
	// brought it up, and relay removes it when the session is torn down. The
	// named volumes are never removed with it — the toolkit and the agent's
	// $HOME are exactly the state meant to outlive the container.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// ContainerExec builds a host-side shell command that runs `inner` inside the
// container. tty=true adds -it and an interactive login shell (for a tmux pane);
// tty=false uses -i and a non-interactive login shell (ad-hoc capture / probes).
// cwd is applied inside the shell so the container's own PATH is present.
func ContainerExec(runtime string, ref ContainerRef, inner string, tty bool) (string, error) {
	if ref.Ref == "" {
		return "", fmt.Errorf("container ref required")
	}
	if runtime == "" {
		runtime = "docker"
	}
	execFlag, shell := "-i", "bash -lc"
	if tty {
		execFlag, shell = "-it", "bash -ilc"
	}
	args := []string{runtime, "exec", execFlag}
	if ref.User != "" {
		args = append(args, "-u", shellquote.Quote(ref.User))
	}
	if ref.Home != "" {
		args = append(args, "-e", shellquote.Quote("HOME="+ref.Home))
	}
	args = append(args, shellquote.Quote(ref.Ref))

	script := inner
	if ref.CWD != "" {
		cd, err := shellquote.PathExpr(ref.CWD)
		if err != nil {
			return "", err
		}
		if tty {
			script = fmt.Sprintf("cd %s; exec %s", cd, inner)
		} else {
			script = fmt.Sprintf("cd %s && %s", cd, inner)
		}
	} else if tty {
		script = "exec " + inner
	}
	if ref.PathPrepend != "" {
		// Prefixed onto the composed script, never onto `inner`: with a tty the
		// inner command is the argument of `exec`, and `exec export PATH=...`
		// is not a command. Set in-shell rather than via -e so the image's own
		// login-shell PATH is still built first and merely prefixed.
		p, err := shellquote.PathExpr(ref.PathPrepend)
		if err != nil {
			return "", err
		}
		script = fmt.Sprintf("export PATH=%s:$PATH; %s", p, script)
	}
	args = append(args, shell, shellquote.Quote(script))
	return strings.Join(args, " "), nil
}

// ContainerSpec declares a container target in a host profile (host.yaml).
type ContainerSpec struct {
	Name       string         `yaml:"name" json:"name"`
	Runtime    string         `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Container  string         `yaml:"container" json:"container"`
	Image      string         `yaml:"image,omitempty" json:"image,omitempty"`
	User       string         `yaml:"user,omitempty" json:"user,omitempty"`
	DefaultCWD string         `yaml:"default_cwd,omitempty" json:"default_cwd,omitempty"`
	Toolchain  string         `yaml:"toolchain,omitempty" json:"toolchain,omitempty"`
	Hooks      string         `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	PathMap    []PathMapEntry `yaml:"path_map,omitempty" json:"path_map,omitempty"`
	Expose     []string       `yaml:"expose,omitempty" json:"expose,omitempty"`
	Env        []string       `yaml:"env,omitempty" json:"env,omitempty"`

	// Devcontainer makes this a relay-managed Dev Containers workspace,
	// brought up by the host's `devcontainer` CLI against the repo's own
	// .devcontainer/devcontainer.json. When set, Container is resolved at
	// `up` time from the id label rather than declared.
	Devcontainer *DevcontainerSpec `yaml:"devcontainer,omitempty" json:"devcontainer,omitempty"`
	// Toolkit is a named volume carrying agent CLI installs.
	Toolkit *ToolkitSpec `yaml:"toolkit,omitempty" json:"toolkit,omitempty"`
	// Home is a named volume that becomes $HOME for in-container agent execs,
	// so agent credentials and config survive container recreation.
	Home *HomeSpec `yaml:"home,omitempty" json:"home,omitempty"`
	// Volumes are extra named volume mounts, each "NAME:/container/path".
	Volumes []string `yaml:"volumes,omitempty" json:"volumes,omitempty"`
}

// RefFor builds the exec binding for a resolved container id, carrying the
// toolkit PATH entry and the volume-backed $HOME so every downstream exec sees
// the same agent installs and the same credentials.
func (c *ContainerSpec) RefFor(containerID, cwd string) ContainerRef {
	ref := ContainerRef{
		Runtime: c.RuntimeVerb(),
		Ref:     containerID,
		CWD:     cwd,
		User:    c.User,
	}
	if c.Home != nil && c.Home.Target != "" {
		ref.Home = c.Home.Target
	}
	if bin := c.Toolkit.ResolvedBinDir(); bin != "" {
		ref.PathPrepend = bin
	}
	return ref
}

// RuntimeVerb is the container CLI to invoke (default docker).
func (c *ContainerSpec) RuntimeVerb() string {
	if c == nil || c.Runtime == "" {
		return "docker"
	}
	return c.Runtime
}

// ResolveCWD picks the container working dir for a local repo: path_map first,
// then default_cwd, then "/".
func (c *ContainerSpec) ResolveCWD(localRepo string) string {
	if c == nil {
		return "/"
	}
	if localRepo != "" {
		if cwd, ok := matchPathMap(c.PathMap, localRepo); ok {
			return cwd
		}
	}
	if c.DefaultCWD != "" {
		return c.DefaultCWD
	}
	return "/"
}

// ResolveContainer finds a container spec by name in the host profile.
func (p *HostProfile) ResolveContainer(name string) (*ContainerSpec, error) {
	if p == nil {
		return nil, fmt.Errorf("nil host profile")
	}
	for i := range p.Containers {
		if p.Containers[i].Name == name {
			return &p.Containers[i], nil
		}
	}
	avail := make([]string, 0, len(p.Containers))
	for i := range p.Containers {
		avail = append(avail, p.Containers[i].Name)
	}
	return nil, fmt.Errorf("container %q not in host profile; available: %s", name, strings.Join(avail, ", "))
}

// ClassifyContainerVerify inspects combined stdout+stderr from an in-container
// agent probe. It returns ok=false with actionable guidance when a known
// failure signature is present; ok=true means no known failure was detected.
func ClassifyContainerVerify(output string) (ok bool, guidance string) {
	low := strings.ToLower(output)
	switch {
	case strings.Contains(output, "GLIBC_") && strings.Contains(low, "not found"):
		return false, "container libc is older than the host toolchain requires; use a self-contained agent that runs here, or a newer base image (node ≥18 needs glibc ≥2.28)"
	case strings.Contains(low, "cannot be used with root") || strings.Contains(low, "root/sudo"):
		return false, "agent refuses to run as root; set a non-root user: (default is the host owner uid)"
	case strings.Contains(low, "permission denied"):
		return false, "permission denied on the agent binary; user: uid must match the owner of the bound files (the host file-owner uid)"
	case strings.Contains(low, "command not found"):
		return false, "agent binary not present in the container; declare a provision: command or use toolchain: bind"
	case strings.Contains(low, "syntaxerror") || strings.Contains(low, "unexpected token"):
		return false, "agent hook/plugin failed under the container toolchain; set hooks: off (default) or the container node is too old"
	}
	return true, ""
}
