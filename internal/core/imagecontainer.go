package core

import (
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// An image-backed container is the third kind a host profile can declare,
// beside a pre-existing `container:` and a `devcontainer:` workspace: relay
// runs it itself from `image:`, one *instance* per session when a session
// names it, and keeps the pane's model unchanged — tmux on the host, the
// pane's inner command crossing into the container through `docker exec`.
//
// The container idles (`sleep infinity`) and the work is the session's inner
// command, so `session exec`, `session send`, `session capture` and the pane's
// own exit status all mean the same thing they mean for every other session.
// A worker whose entrypoint has to be PID 1 does not fit this kind; that is a
// deliberate limit, not an oversight — the pane is the record.

// ImageInstance is one container relay runs from a declared image. Name is the
// instance name (the spec name alone when no session names it); the rest are
// per-instance extras a session may add on top of the spec.
type ImageInstance struct {
	Spec    *ContainerSpec
	Name    string
	Volumes []string // extra named volumes, NAME:/path[:ro]
	Binds   []string // absolute host paths, /abs:/path[:ro]
	GPUs    string   // "" (spec default) | all | none | device list "0,1"
	Network string   // "" (spec default) | docker --network value
}

// ImageBacked reports whether relay runs this container from an image itself.
func (c *ContainerSpec) ImageBacked() bool {
	return c != nil && c.Image != "" && c.Devcontainer == nil
}

// InstanceName is the docker container name for a session's instance of an
// image-backed spec. An unnamed instance is the spec itself, which is what
// `relay container up --container NAME` without --name manages.
func (c *ContainerSpec) InstanceName(session string) string {
	if c == nil {
		return ""
	}
	if session == "" || session == c.Name {
		return c.Name
	}
	return c.Name + "-" + session
}

// ImageInstanceLabel identifies one instance; the spec label
// (ResolvedIDLabel) still identifies every instance of the spec.
func ImageInstanceLabel(instance string) string {
	return "relay.instance=" + instance
}

// BindMount is a host directory bound at a container path.
type BindMount struct {
	Host     string `json:"host"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ParseBindMount reads "/abs/host:/abs/container[:ro]". Both sides must be
// absolute: a relative host path would be resolved against whatever cwd the
// docker client happens to have, and a relative container path is not a
// mount. Named volumes are ParseVolumeMount's job, not this one's.
func ParseBindMount(raw string) (BindMount, error) {
	s := strings.TrimSpace(raw)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return BindMount{}, fmt.Errorf("bind %q must be /abs/host:/abs/container[:ro]", raw)
	}
	m := BindMount{Host: parts[0], Target: parts[1]}
	if !strings.HasPrefix(m.Host, "/") {
		return BindMount{}, fmt.Errorf("bind host path %q must be absolute", m.Host)
	}
	if !strings.HasPrefix(m.Target, "/") {
		return BindMount{}, fmt.Errorf("bind target %q must be absolute", m.Target)
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
		default:
			return BindMount{}, fmt.Errorf("bind %q: mode must be ro or rw", raw)
		}
	}
	return m, nil
}

// gpuArgs renders docker's --gpus for a declared value: nothing for "" or
// none, `--gpus all`, or a device list in the quoted form docker's CSV parser
// needs for more than one device.
func gpuArgs(v string) ([]string, error) {
	switch v {
	case "", "none":
		return nil, nil
	case "all":
		return []string{"--gpus", "all"}, nil
	}
	for _, part := range strings.Split(v, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			return nil, fmt.Errorf("gpus %q: empty device in list", v)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("gpus %q: device %q is not an index (all|none|0,1,…)", v, p)
			}
		}
	}
	return []string{"--gpus", shellquote.Quote(`"device=` + v + `"`)}, nil
}

// imageRunArgs is the `docker run` argv for one instance, without the runtime
// verb. Every value that reaches the host shell is quoted here.
func imageRunArgs(inst ImageInstance) ([]string, error) {
	c := inst.Spec
	if !c.ImageBacked() {
		return nil, fmt.Errorf("container %q is not image-backed", containerName(c))
	}
	name := inst.Name
	if name == "" {
		name = c.Name
	}
	args := []string{"run", "-d",
		"--name", shellquote.Quote(name),
		"--label", shellquote.Quote(c.ResolvedIDLabel()),
		"--label", shellquote.Quote(ImageInstanceLabel(name)),
	}
	gpu := inst.GPUs
	if gpu == "" {
		gpu = c.GPU
	}
	g, err := gpuArgs(gpu)
	if err != nil {
		return nil, err
	}
	args = append(args, g...)
	network := inst.Network
	if network == "" {
		network = c.Network
	}
	if network != "" {
		args = append(args, "--network", shellquote.Quote(network))
	}
	if c.User != "" {
		// The idle process runs as the exec user too, so the container's own
		// identity matches every later exec instead of idling as root.
		args = append(args, "-u", shellquote.Quote(c.User))
	}
	mounts, err := c.VolumeMounts()
	if err != nil {
		return nil, err
	}
	for _, raw := range inst.Volumes {
		vm, err := ParseVolumeMount(raw)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, vm)
	}
	for _, m := range mounts {
		spec := m.Volume + ":" + m.Target
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", shellquote.Quote(spec))
	}
	for _, raw := range inst.Binds {
		b, err := ParseBindMount(raw)
		if err != nil {
			return nil, err
		}
		spec := b.Host + ":" + b.Target
		if b.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", shellquote.Quote(spec))
	}
	for _, env := range c.Env {
		if err := validateEnvName(env); err != nil {
			return nil, err
		}
		// Name only: docker copies the value from the host environment the
		// command runs in, so the value never enters relay.
		args = append(args, "-e", env)
	}
	args = append(args, shellquote.Quote(c.Image), "sleep", "infinity")
	return args, nil
}

// ImageUpCommand brings one instance up, idempotently: a running instance is
// left alone, a stopped one is started, an absent one is created. With
// recreate the existing instance is removed first. The last line of stdout is
// the container id, and the line before it says which of the three happened.
func ImageUpCommand(inst ImageInstance, recreate bool) (string, error) {
	runArgs, err := imageRunArgs(inst)
	if err != nil {
		return "", err
	}
	rt := inst.Spec.RuntimeVerb()
	name := inst.Name
	if name == "" {
		name = inst.Spec.Name
	}
	q := shellquote.Quote(name)
	var b strings.Builder
	b.WriteString("set -e; ")
	if recreate {
		fmt.Fprintf(&b, "%s rm -f %s >/dev/null 2>&1 || true; ", rt, q)
	}
	fmt.Fprintf(&b, "state=$(%s inspect -f '{{.State.Running}}' %s 2>/dev/null || echo absent); ", rt, q)
	b.WriteString("case \"$state\" in ")
	b.WriteString("true) echo running;; ")
	fmt.Fprintf(&b, "false) %s start %s >/dev/null; echo started;; ", rt, q)
	fmt.Fprintf(&b, "*) %s %s >/dev/null; echo created;; ", rt, strings.Join(runArgs, " "))
	b.WriteString("esac; ")
	fmt.Fprintf(&b, "%s inspect -f '{{.Id}}' %s", rt, q)
	// Wrapped in the host's login shell — the only one of the image commands
	// that is — for the same reason the devcontainer up is: `-e NAME` reads
	// the value from the environment the docker client runs in, and on the
	// fleet that value is set by the interactive profile, not by the
	// non-interactive shell ssh gives relay. down/status/stop/start pass no
	// environment through and run plain.
	return HostLoginShell(b.String()), nil
}

// ImageDownCommand removes one instance. Named volumes and bind sources are
// untouched, for the same reason DevcontainerDownCommand leaves them: the
// container is the disposable part.
func ImageDownCommand(c *ContainerSpec, instance string) string {
	rt := c.RuntimeVerb()
	q := shellquote.Quote(instance)
	return fmt.Sprintf("if %s inspect %s >/dev/null 2>&1; then %s rm -f %s >/dev/null && echo removed; else echo no-container; fi", rt, q, rt, q)
}

// ImageStatusCommand prints "RUNNING EXITCODE ID" for one instance, or
// "absent" when it does not exist.
func ImageStatusCommand(c *ContainerSpec, instance string) string {
	return fmt.Sprintf("%s inspect -f '{{.State.Running}} {{.State.ExitCode}} {{.Id}}' %s 2>/dev/null || echo absent",
		c.RuntimeVerb(), shellquote.Quote(instance))
}

// ImageStopCommand / ImageStartCommand pause and resume one instance without
// removing it: the container, its filesystem and its mounts survive, which is
// what lets a caller park work and come back to the same environment.
func ImageStopCommand(c *ContainerSpec, instance string) string {
	return fmt.Sprintf("%s stop %s >/dev/null && echo stopped", c.RuntimeVerb(), shellquote.Quote(instance))
}

func ImageStartCommand(c *ContainerSpec, instance string) string {
	return fmt.Sprintf("%s start %s >/dev/null && echo started", c.RuntimeVerb(), shellquote.Quote(instance))
}

// parseImageStatus reads ImageStatusCommand's output.
func parseImageStatus(out string) (running bool, exitCode int, id string, present bool) {
	line := firstLine(out)
	if line == "" || line == "absent" {
		return false, 0, "", false
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return false, 0, "", false
	}
	running = fields[0] == "true"
	fmt.Sscanf(fields[1], "%d", &exitCode)
	return running, exitCode, fields[2], true
}
