package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// DevcontainerSpec declares a Dev Containers spec workspace on the host. relay
// does not reimplement the spec: it shells out to the `devcontainer` CLI on the
// host, against the repo's own .devcontainer/devcontainer.json, and injects the
// relay-managed volume mounts and env on top. Image builds, features and
// lifecycle commands stay the CLI's job.
type DevcontainerSpec struct {
	// WorkspaceFolder is the HOST path holding .devcontainer/ (or .devcontainer.json).
	WorkspaceFolder string `yaml:"workspace_folder" json:"workspace_folder"`
	// Config overrides the devcontainer.json path (CLI --config).
	Config string `yaml:"config,omitempty" json:"config,omitempty"`
	// IDLabel is the name=value label identifying this container. Defaults to
	// relay.container=<spec name>, which is also how `down`/`status` find it.
	IDLabel string `yaml:"id_label,omitempty" json:"id_label,omitempty"`
	// GPU maps to --gpu-availability: all | detect | none.
	GPU string `yaml:"gpu,omitempty" json:"gpu,omitempty"`
	// SkipPostCreate maps to --skip-post-create.
	SkipPostCreate bool `yaml:"skip_post_create,omitempty" json:"skip_post_create,omitempty"`
	// CLI is the devcontainer binary (default "devcontainer"). Set it when the
	// CLI is only on an interactive PATH or lives at an absolute path.
	CLI string `yaml:"cli,omitempty" json:"cli,omitempty"`
}

// ToolkitSpec is a docker named volume carrying agent CLI installs, shared by
// every container on the host that mounts it. It exists because neither
// binding the host's own agent closure (breaks on a container with older
// glibc) nor reinstalling per container (repeated, and lost on recreate) is
// durable. The volume is populated once, inside a container, so its binaries
// are linked against the container's libc by construction.
type ToolkitSpec struct {
	Volume string `yaml:"volume" json:"volume"`                       // docker named volume
	Target string `yaml:"target" json:"target"`                       // mount point in-container
	BinDir string `yaml:"bin_dir,omitempty" json:"bin_dir,omitempty"` // PATH entry; default <target>/bin
	// Provision populates the volume. Run once, guarded by a stamp file, as the
	// container's exec user. Declarative data — relay has no per-agent install logic.
	Provision string `yaml:"provision,omitempty" json:"provision,omitempty"`
}

// HomeSpec is a named volume that becomes $HOME for in-container agent execs.
// Agent credentials and config (~/.claude.json, ~/.codex, ~/.cursor) are
// written by the agent to its own $HOME; pointing HOME at a volume persists all
// of them across container recreation without relay knowing any agent's layout,
// and without binding the host's ~/.claude (which drags host hooks and plugins
// into a container whose toolchain cannot run them).
type HomeSpec struct {
	Volume string `yaml:"volume" json:"volume"`
	Target string `yaml:"target" json:"target"`
}

// ResolvedBinDir is the PATH entry for the toolkit volume.
func (t *ToolkitSpec) ResolvedBinDir() string {
	if t == nil || t.Target == "" {
		return ""
	}
	if t.BinDir != "" {
		return t.BinDir
	}
	return strings.TrimRight(t.Target, "/") + "/bin"
}

// StampPath marks a completed provision so it is not repeated.
func (t *ToolkitSpec) StampPath() string {
	if t == nil || t.Target == "" {
		return ""
	}
	return strings.TrimRight(t.Target, "/") + "/.relay-toolkit-ok"
}

// DevcontainerCLI is the binary to invoke.
func (d *DevcontainerSpec) DevcontainerCLI() string {
	if d == nil || d.CLI == "" {
		return "devcontainer"
	}
	return d.CLI
}

// ResolvedIDLabel is the name=value label relay uses to find this container.
func (c *ContainerSpec) ResolvedIDLabel() string {
	if c == nil {
		return ""
	}
	if c.Devcontainer != nil && c.Devcontainer.IDLabel != "" {
		return c.Devcontainer.IDLabel
	}
	return "relay.container=" + c.Name
}

// VolumeMounts returns every named volume this spec mounts: the toolkit, the
// home volume, then any extra `volumes:` entries, in that order.
func (c *ContainerSpec) VolumeMounts() ([]VolumeMount, error) {
	if c == nil {
		return nil, nil
	}
	out := []VolumeMount{}
	if c.Toolkit != nil && c.Toolkit.Volume != "" {
		if c.Toolkit.Target == "" {
			return nil, fmt.Errorf("toolkit.target required when toolkit.volume is set")
		}
		out = append(out, VolumeMount{Volume: c.Toolkit.Volume, Target: c.Toolkit.Target})
	}
	if c.Home != nil && c.Home.Volume != "" {
		if c.Home.Target == "" {
			return nil, fmt.Errorf("home.target required when home.volume is set")
		}
		out = append(out, VolumeMount{Volume: c.Home.Volume, Target: c.Home.Target})
	}
	for _, raw := range c.Volumes {
		vm, err := ParseVolumeMount(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, vm)
	}
	return out, nil
}

// VolumeMount is a docker named volume bound at a container path.
type VolumeMount struct {
	Volume string `json:"volume"`
	Target string `json:"target"`
}

// ParseVolumeMount reads a "NAME:/container/path" entry. The name must be a
// docker volume name, never a host path — a bind mount belongs in `expose:`,
// and silently accepting one here would create a directory on the host as root.
func ParseVolumeMount(raw string) (VolumeMount, error) {
	s := strings.TrimSpace(raw)
	i := strings.Index(s, ":")
	if i <= 0 || i == len(s)-1 {
		return VolumeMount{}, fmt.Errorf("volume %q must be NAME:/container/path", raw)
	}
	name, target := s[:i], s[i+1:]
	if strings.ContainsAny(name, "/~.") {
		return VolumeMount{}, fmt.Errorf("volume source %q looks like a host path; named volumes only (use expose: for bind mounts)", name)
	}
	if !strings.HasPrefix(target, "/") {
		return VolumeMount{}, fmt.Errorf("volume target %q must be absolute", target)
	}
	return VolumeMount{Volume: name, Target: target}, nil
}

// DevcontainerUpCommand builds the host-side `devcontainer up` invocation.
// Env values are read from the host environment by name, never carried as
// literals, so no secret passes through relay's own state or logs.
func DevcontainerUpCommand(c *ContainerSpec, removeExisting bool) (string, error) {
	if c == nil || c.Devcontainer == nil {
		return "", fmt.Errorf("container %q has no devcontainer: block", containerName(c))
	}
	d := c.Devcontainer
	if d.WorkspaceFolder == "" {
		return "", fmt.Errorf("devcontainer.workspace_folder required")
	}
	wf, err := shellquote.PathExpr(d.WorkspaceFolder)
	if err != nil {
		return "", err
	}
	args := []string{d.DevcontainerCLI(), "up", "--workspace-folder", wf}
	args = append(args, "--id-label", shellquote.Quote(c.ResolvedIDLabel()))
	if d.Config != "" {
		cfg, err := shellquote.PathExpr(d.Config)
		if err != nil {
			return "", err
		}
		args = append(args, "--config", cfg)
	}
	switch d.GPU {
	case "":
	case "all", "detect", "none":
		args = append(args, "--gpu-availability", d.GPU)
	default:
		return "", fmt.Errorf("devcontainer.gpu must be all|detect|none, got %q", d.GPU)
	}
	if removeExisting {
		args = append(args, "--remove-existing-container")
	}
	if d.SkipPostCreate {
		args = append(args, "--skip-post-create")
	}
	mounts, err := c.VolumeMounts()
	if err != nil {
		return "", err
	}
	for _, m := range mounts {
		args = append(args, "--mount",
			shellquote.Quote(fmt.Sprintf("type=volume,source=%s,target=%s", m.Volume, m.Target)))
	}
	for _, name := range c.Env {
		if err := validateEnvName(name); err != nil {
			return "", err
		}
		// Expanded by the host shell at run time: the value never enters relay.
		args = append(args, "--remote-env", fmt.Sprintf(`"%s=$%s"`, name, name))
	}
	args = append(args, "--log-format", "json")
	return HostLoginShell(strings.Join(args, " ")), nil
}

// HostLoginShell wraps a command so it runs under the host's interactive login
// shell. relay's ssh transport runs commands non-interactively, which is fine
// for /usr/bin/docker but not for the devcontainer CLI: a global npm install
// under nvm is only on the PATH an interactive shell builds (verified on
// hamburg — `node -v` is empty under `bash -lc`, v20.19.5 under `bash -ilc`).
// The wrapped text keeps $HOME and $NAME unexpanded for the inner shell, which
// is what makes ~-paths and --remote-env passthrough resolve on the host.
func HostLoginShell(cmd string) string {
	return "bash -ilc " + shellquote.Quote(cmd)
}

// validateEnvName rejects anything that is not a plain environment variable
// name, since the name is interpolated into a host shell word.
func validateEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("empty env name")
	}
	for i, r := range name {
		alpha := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if alpha || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return fmt.Errorf("env name %q is not a valid shell identifier", name)
	}
	return nil
}

func containerName(c *ContainerSpec) string {
	if c == nil {
		return ""
	}
	return c.Name
}

// DevcontainerResolveCommand finds the running container id by its id label.
// This is the fallback (and the check used by status/down) when the CLI's own
// JSON outcome is unavailable.
func DevcontainerResolveCommand(c *ContainerSpec) string {
	return fmt.Sprintf("%s ps -q --filter %s --filter status=running",
		c.RuntimeVerb(), shellquote.Quote("label="+c.ResolvedIDLabel()))
}

// DevcontainerDownCommand removes the container. The devcontainer CLI has no
// `down` verb, so this goes through the runtime, scoped to the id label.
func DevcontainerDownCommand(c *ContainerSpec) string {
	label := shellquote.Quote("label=" + c.ResolvedIDLabel())
	rt := c.RuntimeVerb()
	return fmt.Sprintf("ids=$(%s ps -aq --filter %s); if [ -n \"$ids\" ]; then %s rm -f $ids; else echo no-container; fi", rt, label, rt)
}

// ToolkitProvisionCommand runs the declared provision command inside the
// container, once. It is guarded by a stamp file on the volume so a recreated
// container reuses the already-populated volume instead of reinstalling.
func ToolkitProvisionCommand(c *ContainerSpec, ref ContainerRef, force bool) (string, error) {
	if c == nil || c.Toolkit == nil || c.Toolkit.Provision == "" {
		return "", fmt.Errorf("container %q has no toolkit.provision", containerName(c))
	}
	stamp, err := shellquote.PathExpr(c.Toolkit.StampPath())
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf("set -e; %s; mkdir -p $(dirname %s); date -Is > %s", c.Toolkit.Provision, stamp, stamp)
	inner := body
	if !force {
		inner = fmt.Sprintf("if [ -f %s ]; then echo already-provisioned; else %s; fi", stamp, body)
	}
	return ContainerExec(c.RuntimeVerb(), ref, inner, false)
}

// DevcontainerMetadataCommand reads the devcontainer CLI's own metadata label
// off a running container. The label is where `remoteUser` survives, which
// matters because a devcontainer's Config.User is still root: the CLI applies
// remoteUser only to execs it runs itself, so relay has to read it and apply it
// too, or every agent exec lands as root (verified on hamburg, 2026-09-05).
func DevcontainerMetadataCommand(runtime, containerID string) string {
	return fmt.Sprintf("%s inspect -f '{{index .Config.Labels \"devcontainer.metadata\"}}' %s",
		runtime, shellquote.Quote(containerID))
}

// ParseRemoteUser extracts remoteUser from the devcontainer.metadata array.
// The array is layered (image, then features, then the config); the last entry
// that names a remoteUser wins, matching the CLI's own merge order.
func ParseRemoteUser(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var entries []struct {
		RemoteUser string `json:"remoteUser"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return ""
	}
	user := ""
	for _, e := range entries {
		if e.RemoteUser != "" {
			user = e.RemoteUser
		}
	}
	return user
}

// ContainerPrepareCommand makes the relay-managed named volumes writable by the
// exec user. A named volume whose mount point does not exist in the image is
// created root:root 0755 by docker, so a non-root agent cannot write to it —
// and agents must not run as root. Run as root, guarded so a populated toolkit
// is not re-walked on every up.
func ContainerPrepareCommand(c *ContainerSpec, containerID, user string) (string, error) {
	if user == "" || user == "root" || user == "0" {
		return "", nil
	}
	mounts, err := c.VolumeMounts()
	if err != nil {
		return "", err
	}
	targets := make([]string, 0, len(mounts))
	for _, m := range mounts {
		q, err := shellquote.PathExpr(m.Target)
		if err != nil {
			return "", err
		}
		targets = append(targets, q)
	}
	if len(targets) == 0 {
		return "", nil
	}
	u := shellquote.Quote(user)
	joined := strings.Join(targets, " ")
	body := fmt.Sprintf(
		"uid=$(id -u %s) || exit 1; for d in %s; do if [ \"$(stat -c %%u \"$d\")\" != \"$uid\" ]; then chown -R %s \"$d\"; fi; done; echo prepared",
		u, joined, u)
	// Root exec, and only here: the volumes are chowned once so every later
	// exec — agent included — can run unprivileged.
	rootRef := ContainerRef{Runtime: c.RuntimeVerb(), Ref: containerID, User: "0"}
	return ContainerExec(c.RuntimeVerb(), rootRef, body, false)
}

// ResolveContainerUser picks the uid/name in-container execs run as: an
// explicit `user:` first, then the devcontainer CLI's recorded remoteUser.
// Empty means "the image default", which for a devcontainer is root.
func ResolveContainerUser(ctx context.Context, t ports.Transport, c *ContainerSpec, containerID string) string {
	if c.User != "" {
		return c.User
	}
	if c.Devcontainer == nil || containerID == "" {
		return ""
	}
	out, _, err := t.Run(ctx, "", DevcontainerMetadataCommand(c.RuntimeVerb(), containerID))
	if err != nil {
		return ""
	}
	return ParseRemoteUser(out)
}

// ContainerService brings relay-managed devcontainers up and down on a host.
type ContainerService struct {
	NewTransport TransportFactory
	Profiles     *ProfileService
}

// ContainerStatus is the reported state of one declared container.
type ContainerStatus struct {
	OK          bool   `json:"ok"`
	Name        string `json:"name"`
	HostID      string `json:"host_id"`
	IDLabel     string `json:"id_label"`
	ContainerID string `json:"container_id,omitempty"`
	Running     bool   `json:"running"`
	User        string `json:"user,omitempty"`
	Toolkit     string `json:"toolkit,omitempty"`
	Home        string `json:"home,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

func (s *ContainerService) resolve(ctx context.Context, hostID, name string) (ports.Transport, *ContainerSpec, error) {
	if hostID == "" {
		return nil, nil, fmt.Errorf("host required")
	}
	if s == nil || s.Profiles == nil || s.NewTransport == nil {
		return nil, nil, fmt.Errorf("container service is not configured")
	}
	profile, err := s.Profiles.Get(ctx, hostID, false)
	if err != nil {
		return nil, nil, err
	}
	spec, err := profile.ResolveContainer(name)
	if err != nil {
		return nil, nil, err
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return nil, nil, err
	}
	return t, spec, nil
}

// devcontainerOutcome is the CLI's --log-format json result line.
type devcontainerOutcome struct {
	Outcome     string `json:"outcome"`
	ContainerID string `json:"containerId"`
	RemoteUser  string `json:"remoteUser"`
	Message     string `json:"message"`
	Description string `json:"description"`
}

// parseDevcontainerOutcome scans CLI output for the JSON result line. The CLI
// interleaves progress lines, so the outcome is found by scanning for the last
// line that parses as an object carrying "outcome".
func parseDevcontainerOutcome(out string) (devcontainerOutcome, bool) {
	var found devcontainerOutcome
	ok := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, "\"outcome\"") {
			continue
		}
		var o devcontainerOutcome
		if json.Unmarshal([]byte(line), &o) == nil && o.Outcome != "" {
			found, ok = o, true
		}
	}
	return found, ok
}

// Up brings the devcontainer up and populates the toolkit volume. It returns
// the resolved container id so downstream verbs can `docker exec` into it.
func (s *ContainerService) Up(ctx context.Context, hostID, name string, removeExisting, forceProvision bool) (*ContainerStatus, error) {
	t, spec, err := s.resolve(ctx, hostID, name)
	if err != nil {
		return nil, err
	}
	st := &ContainerStatus{Name: name, HostID: hostID, IDLabel: spec.ResolvedIDLabel()}
	upCmd, err := DevcontainerUpCommand(spec, removeExisting)
	if err != nil {
		return nil, err
	}
	out, errOut, runErr := t.Run(ctx, "", upCmd)
	combined := strings.TrimSpace(out + "\n" + errOut)
	outcome, haveOutcome := parseDevcontainerOutcome(combined)
	if runErr != nil || (haveOutcome && outcome.Outcome != "success") {
		detail := outcome.Description
		if detail == "" {
			detail = outcome.Message
		}
		if detail == "" {
			detail = combined
		}
		if strings.Contains(combined, "command not found") || strings.Contains(combined, "not found") && strings.Contains(combined, spec.Devcontainer.DevcontainerCLI()) {
			return nil, fmt.Errorf("devcontainer CLI not found on %s: install it (npm i -g @devcontainers/cli, needs node >= 18) or set devcontainer.cli to its absolute path\n--- output ---\n%s", hostID, combined)
		}
		return nil, fmt.Errorf("devcontainer up failed on %s: %s", hostID, detail)
	}
	st.ContainerID = outcome.ContainerID
	if st.ContainerID == "" {
		id, _, err := t.Run(ctx, "", DevcontainerResolveCommand(spec))
		if err != nil {
			return nil, fmt.Errorf("devcontainer up reported no container id and lookup by label failed: %w", err)
		}
		st.ContainerID = firstLine(id)
	}
	if st.ContainerID == "" {
		return nil, fmt.Errorf("devcontainer up succeeded but no container carries label %s", spec.ResolvedIDLabel())
	}
	st.Running = true
	if spec.Toolkit != nil {
		st.Toolkit = spec.Toolkit.Volume
	}
	if spec.Home != nil {
		st.Home = spec.Home.Volume
	}
	user := spec.User
	if user == "" {
		user = outcome.RemoteUser
	}
	if user == "" {
		user = ResolveContainerUser(ctx, t, spec, st.ContainerID)
	}
	st.User = user
	prep, err := ContainerPrepareCommand(spec, st.ContainerID, user)
	if err != nil {
		return nil, err
	}
	if prep != "" {
		pOut, pErr, pRunErr := t.Run(ctx, "", prep)
		if pRunErr != nil {
			return nil, fmt.Errorf("could not make the relay volumes writable by %s in %s: %w\n--- output ---\n%s",
				user, st.ContainerID, pRunErr, strings.TrimSpace(pOut+"\n"+pErr))
		}
	}
	if spec.Toolkit != nil && spec.Toolkit.Provision != "" {
		ref := spec.RefFor(st.ContainerID, "")
		ref.User = user
		provCmd, err := ToolkitProvisionCommand(spec, ref, forceProvision)
		if err != nil {
			return nil, err
		}
		pOut, pErr, pRunErr := t.Run(ctx, "", provCmd)
		pCombined := strings.TrimSpace(pOut + "\n" + pErr)
		if pRunErr != nil {
			return nil, fmt.Errorf("toolkit provision failed in %s: %w\n--- output ---\n%s", st.ContainerID, pRunErr, pCombined)
		}
		st.Detail = firstLine(pCombined)
	}
	st.OK = true
	return st, nil
}

// Down removes the container. The named volumes survive by design: the toolkit
// and the agent's $HOME are the state worth keeping across a recreate.
func (s *ContainerService) Down(ctx context.Context, hostID, name string) (*ContainerStatus, error) {
	t, spec, err := s.resolve(ctx, hostID, name)
	if err != nil {
		return nil, err
	}
	out, errOut, runErr := t.Run(ctx, "", DevcontainerDownCommand(spec))
	if runErr != nil {
		return nil, fmt.Errorf("container down failed on %s: %w\n%s", hostID, runErr, strings.TrimSpace(out+"\n"+errOut))
	}
	return &ContainerStatus{
		OK: true, Name: name, HostID: hostID,
		IDLabel: spec.ResolvedIDLabel(), Running: false,
		Detail: firstLine(strings.TrimSpace(out + "\n" + errOut)),
	}, nil
}

// Status reports whether the declared container is currently running.
func (s *ContainerService) Status(ctx context.Context, hostID, name string) (*ContainerStatus, error) {
	t, spec, err := s.resolve(ctx, hostID, name)
	if err != nil {
		return nil, err
	}
	st := &ContainerStatus{OK: true, Name: name, HostID: hostID, IDLabel: spec.ResolvedIDLabel()}
	if spec.Toolkit != nil {
		st.Toolkit = spec.Toolkit.Volume
	}
	if spec.Home != nil {
		st.Home = spec.Home.Volume
	}
	if spec.Devcontainer == nil {
		// A plain declared container: report the configured ref as-is.
		st.ContainerID = spec.Container
		out, _, _ := t.Run(ctx, "", fmt.Sprintf("%s inspect -f {{.State.Running}} %s 2>/dev/null || echo false",
			spec.RuntimeVerb(), shellquote.Quote(spec.Container)))
		st.Running = strings.TrimSpace(firstLine(out)) == "true"
		return st, nil
	}
	out, _, err := t.Run(ctx, "", DevcontainerResolveCommand(spec))
	if err != nil {
		st.Detail = err.Error()
		return st, nil
	}
	st.ContainerID = firstLine(out)
	st.Running = st.ContainerID != ""
	return st, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
