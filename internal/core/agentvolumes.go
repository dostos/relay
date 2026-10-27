package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// AgentVolumesSpec is the host-level default toolkit + home volumes applied to
// every container kind (image-backed, devcontainer, and existing) unless a
// container opts out or overrides the fields itself.
type AgentVolumesSpec struct {
	Toolkit *ToolkitSpec `yaml:"toolkit,omitempty" json:"toolkit,omitempty"`
	Home    *HomeSpec    `yaml:"home,omitempty" json:"home,omitempty"`
}

// AgentVolumeStatus is one agent's binary + auth presence inside a container
// that mounts the toolkit/home volumes.
type AgentVolumeStatus struct {
	Agent   string `json:"agent"`
	Binary  bool   `json:"binary"`
	Version string `json:"version,omitempty"`
	Authed  bool   `json:"authed"`
	Detail  string `json:"detail,omitempty"`
}

// AuthSyncResult reports what relay container auth sync copied into the home
// volume.
type AuthSyncResult struct {
	OK      bool     `json:"ok"`
	HostID  string   `json:"host_id"`
	Home    string   `json:"home_volume"`
	Target  string   `json:"home_target"`
	Agent   string   `json:"agent,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
	Files   []string `json:"files"`
	Skipped []string `json:"skipped,omitempty"`
	Detail  string   `json:"detail,omitempty"`
}

// AuthSyncFile is one allowlisted credential/config path relative to the host
// home, and where it lands under the home volume's $HOME.
type AuthSyncFile struct {
	Agent     string // claude | codex | ccs | cursor
	HostRel   string // under ~, e.g. ".claude/.credentials.json"
	VolumeRel string // under home volume target, same layout by default
	Optional  bool   // missing on host is skip, not error
	DirCopy   bool   // copy a directory tree (still only this path)
}

// AuthSyncAllowlist is the only set of host paths relay will copy into a home
// volume. Hooks, plugins and MCP config are deliberately absent: binding the
// host's ~/.claude would drag them into a container whose toolchain cannot run
// them (see docs/devcontainers.md).
func AuthSyncAllowlist() []AuthSyncFile {
	return []AuthSyncFile{
		// Agent CLIs (credentials only — never hooks/plugins/MCP).
		{Agent: "claude", HostRel: ".claude/.credentials.json", VolumeRel: ".claude/.credentials.json"},
		{Agent: "claude", HostRel: ".claude.json", VolumeRel: ".claude.json", Optional: true},
		{Agent: "codex", HostRel: ".codex/auth.json", VolumeRel: ".codex/auth.json"},
		{Agent: "codex", HostRel: ".codex/config.toml", VolumeRel: ".codex/config.toml", Optional: true},
		// CCS config + instances only — never ~/.ccs/backups or ~/.ccs/shared
		// (multi-GB caches on fleet hosts).
		{Agent: "ccs", HostRel: ".ccs/config.yaml", VolumeRel: ".ccs/config.yaml", Optional: true},
		{Agent: "ccs", HostRel: ".ccs/.session-secret", VolumeRel: ".ccs/.session-secret", Optional: true},
		{Agent: "ccs", HostRel: ".ccs/codex-profiles.yaml", VolumeRel: ".ccs/codex-profiles.yaml", Optional: true},
		{Agent: "ccs", HostRel: ".ccs/codex.settings.json", VolumeRel: ".ccs/codex.settings.json", Optional: true},
		{Agent: "ccs", HostRel: ".ccs/instances", VolumeRel: ".ccs/instances", DirCopy: true, Optional: true},
		{Agent: "cursor", HostRel: ".cursor/argv.json", VolumeRel: ".cursor/argv.json", Optional: true},
		{Agent: "cursor", HostRel: ".cursor/cli-config.json", VolumeRel: ".cursor/cli-config.json", Optional: true},
		{Agent: "cursor", HostRel: ".cursor/auth.json", VolumeRel: ".cursor/auth.json", Optional: true},
		{Agent: "cursor", HostRel: ".config/cursor/auth.json", VolumeRel: ".config/cursor/auth.json", Optional: true},
		{Agent: "cursor", HostRel: ".config/cursor/prompts", VolumeRel: ".config/cursor/prompts", DirCopy: true, Optional: true},

		// Shell / dotfiles for a usable interactive session inside the
		// container. Copied from the *host* home (madrid, hamburg, …), not
		// the Mac hub — keep host dotfiles in sync with the Mac separately.
		// oh-my-zsh is optional and large; skip when absent.
		{Agent: "shell", HostRel: ".zshrc", VolumeRel: ".zshrc", Optional: true},
		{Agent: "shell", HostRel: ".zprofile", VolumeRel: ".zprofile", Optional: true},
		{Agent: "shell", HostRel: ".zshenv", VolumeRel: ".zshenv", Optional: true},
		{Agent: "shell", HostRel: ".oh-my-zsh", VolumeRel: ".oh-my-zsh", DirCopy: true, Optional: true},
		{Agent: "shell", HostRel: ".zprezto", VolumeRel: ".zprezto", DirCopy: true, Optional: true},

		// Git identity + ignore. The forge-default image already ships `git`;
		// this only brings config.
		{Agent: "git", HostRel: ".gitconfig", VolumeRel: ".gitconfig", Optional: true},
		{Agent: "git", HostRel: ".gitignore_global", VolumeRel: ".gitignore_global", Optional: true},
		{Agent: "git", HostRel: ".config/git/config", VolumeRel: ".config/git/config", Optional: true},
		{Agent: "git", HostRel: ".config/git/ignore", VolumeRel: ".config/git/ignore", Optional: true},

		// SSH keys for git remotes. Named keys only — never the whole ~/.ssh
		// (sockets, known_hosts noise, extra identities).
		{Agent: "ssh", HostRel: ".ssh/config", VolumeRel: ".ssh/config", Optional: true},
		{Agent: "ssh", HostRel: ".ssh/id_ed25519", VolumeRel: ".ssh/id_ed25519", Optional: true},
		{Agent: "ssh", HostRel: ".ssh/id_ed25519.pub", VolumeRel: ".ssh/id_ed25519.pub", Optional: true},
		{Agent: "ssh", HostRel: ".ssh/id_ed25519_dostos_mesh", VolumeRel: ".ssh/id_ed25519_dostos_mesh", Optional: true},
		{Agent: "ssh", HostRel: ".ssh/id_ed25519_dostos_mesh.pub", VolumeRel: ".ssh/id_ed25519_dostos_mesh.pub", Optional: true},
		{Agent: "ssh", HostRel: ".ssh/id_rsa", VolumeRel: ".ssh/id_rsa", Optional: true},
		{Agent: "ssh", HostRel: ".ssh/id_rsa.pub", VolumeRel: ".ssh/id_rsa.pub", Optional: true},

		// GitHub CLI auth (hosts.yml). No keychain dump.
		{Agent: "gh", HostRel: ".config/gh/hosts.yml", VolumeRel: ".config/gh/hosts.yml", Optional: true},

		// Optional API keys already kept as files on the host (not keychain).
		{Agent: "secrets", HostRel: ".config/openrouter/key", VolumeRel: ".config/openrouter/key", Optional: true},
		{Agent: "secrets", HostRel: ".config/sops/age/keys.txt", VolumeRel: ".config/sops/age/keys.txt", Optional: true},
	}
}

// DefaultForgeAgents is the toolkit.agents list forge-default / host
// agent_volumes should provision so sessions inside the container see the
// same CLIs the host has (installed into the toolkit volume, libc-matched).
func DefaultForgeAgents() []string {
	return []string{"claude", "codex", "ccs", "cursor-agent", "gh"}
}

// BuiltInAgentRecipes are the provision snippets selectable by name under
// toolkit.agents. Raw toolkit.provision remains the escape hatch.
func BuiltInAgentRecipes() map[string]string {
	return map[string]string{
		"claude": `npm i -g --prefix "$RELAY_TOOLKIT" @anthropic-ai/claude-code`,
		"codex":  `npm i -g --prefix "$RELAY_TOOLKIT" @openai/codex`,
		// Real package is @kaitranntt/ccs (Claude Codex Switch). The old
		// @agentclientprotocol/ccs 404s and bare "ccs" is an unrelated stub.
		"ccs": `npm i -g --prefix "$RELAY_TOOLKIT" @kaitranntt/ccs`,
		"cursor-agent": `HOME="$RELAY_TOOLKIT" bash -c "curl -fsS https://cursor.com/install | bash"
mkdir -p "$RELAY_TOOLKIT/bin"
ln -sfn "$RELAY_TOOLKIT/.local/bin/cursor-agent" "$RELAY_TOOLKIT/bin/cursor-agent"`,
		"gh": `# GitHub CLI into the toolkit bin (hosts.yml still comes from auth sync).
arch=$(uname -m); case "$arch" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo "gh: unsupported arch $arch"; exit 1;; esac
ver=$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest | sed -n 's/.*"tag_name": "v\([^"]*\)".*/\1/p' | head -1)
test -n "$ver" || { echo "gh: could not resolve latest version"; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://github.com/cli/cli/releases/download/v${ver}/gh_${ver}_linux_${arch}.tar.gz" | tar -xz -C "$tmp"
mkdir -p "$RELAY_TOOLKIT/bin"
cp "$tmp/gh_${ver}_linux_${arch}/bin/gh" "$RELAY_TOOLKIT/bin/gh"
chmod +x "$RELAY_TOOLKIT/bin/gh"`,
	}
}

// ComposeAgentRecipes joins built-in install recipes for the named agents into
// a single provision script. Unknown names are refused rather than skipped so
// a typo cannot silently leave an agent uninstalled.
func ComposeAgentRecipes(agents []string, toolkitTarget string) (string, error) {
	if len(agents) == 0 {
		return "", nil
	}
	recipes := BuiltInAgentRecipes()
	var parts []string
	parts = append(parts, "RELAY_TOOLKIT="+shellquote.Quote(strings.TrimRight(toolkitTarget, "/")), "mkdir -p \"$RELAY_TOOLKIT/bin\"")
	for _, name := range agents {
		name = strings.TrimSpace(name)
		body, ok := recipes[name]
		if !ok {
			known := make([]string, 0, len(recipes))
			for k := range recipes {
				known = append(known, k)
			}
			return "", fmt.Errorf("unknown toolkit agent recipe %q (want one of: %s)", name, strings.Join(sortedCopy(known), ", "))
		}
		parts = append(parts, "# recipe:"+name, body)
	}
	return strings.Join(parts, "\n"), nil
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ResolvedProvision is the shell that populates the toolkit: an explicit
// provision: wins; otherwise toolkit.agents recipes are composed.
func (t *ToolkitSpec) ResolvedProvision() (string, error) {
	if t == nil {
		return "", nil
	}
	if strings.TrimSpace(t.Provision) != "" {
		return t.Provision, nil
	}
	return ComposeAgentRecipes(t.Agents, t.Target)
}

// ApplyAgentVolumes returns a copy of c with host-level agent_volumes filled
// in when the spec does not override them and has not opted out.
//
// When a container already declares toolkit:/home: volumes (Forge's
// forge-default does) but left agents/provision empty, the host-level
// toolkit.agents list is inherited so provision still runs.
func (p *HostProfile) ApplyAgentVolumes(c *ContainerSpec) *ContainerSpec {
	if c == nil {
		return nil
	}
	out := *c
	if out.AgentVolumes != nil && !*out.AgentVolumes {
		return resolveToolkitCopy(&out)
	}
	if p == nil || p.AgentVolumes == nil {
		return resolveToolkitCopy(&out)
	}
	if out.Toolkit == nil && p.AgentVolumes.Toolkit != nil {
		tk := *p.AgentVolumes.Toolkit
		out.Toolkit = &tk
	} else if out.Toolkit != nil && p.AgentVolumes.Toolkit != nil {
		tk := *out.Toolkit
		hostTK := p.AgentVolumes.Toolkit
		if len(tk.Agents) == 0 && strings.TrimSpace(tk.Provision) == "" && len(hostTK.Agents) > 0 {
			tk.Agents = append([]string(nil), hostTK.Agents...)
		}
		if strings.TrimSpace(tk.Provision) == "" && strings.TrimSpace(hostTK.Provision) != "" {
			tk.Provision = hostTK.Provision
		}
		out.Toolkit = &tk
	}
	if out.Home == nil && p.AgentVolumes.Home != nil {
		h := *p.AgentVolumes.Home
		out.Home = &h
	}
	return resolveToolkitCopy(&out)
}

func resolveToolkitCopy(c *ContainerSpec) *ContainerSpec {
	if c == nil || c.Toolkit == nil {
		return c
	}
	tk := *c.Toolkit
	if strings.TrimSpace(tk.Provision) == "" && len(tk.Agents) > 0 {
		prov, err := ComposeAgentRecipes(tk.Agents, tk.Target)
		if err == nil {
			tk.Provision = prov
		}
		// Leave Provision empty on error; ToolkitProvisionCommand / callers
		// that validate recipes surface it. Apply works on the parsed form.
	}
	c.Toolkit = &tk
	return c
}

// AuthSyncFilesFor returns the allowlisted files for agent (empty = all).
func AuthSyncFilesFor(agent string) ([]AuthSyncFile, error) {
	agent = strings.TrimSpace(strings.ToLower(agent))
	switch agent {
	case "", "all":
		return AuthSyncAllowlist(), nil
	case "claude", "codex", "ccs", "cursor", "cursor-agent",
		"shell", "git", "ssh", "gh", "secrets":
		if agent == "cursor-agent" {
			agent = "cursor"
		}
		var out []AuthSyncFile
		for _, f := range AuthSyncAllowlist() {
			if f.Agent == agent {
				out = append(out, f)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown --agent %q (want claude|codex|ccs|cursor|shell|git|ssh|gh|secrets)", agent)
	}
}

// AuthSyncCommand builds a host-side script that copies allowlisted credential
// files from the host home into the named home volume via one short-lived
// helper container and `docker cp`. One container for the whole allowlist
// avoids N× image starts (and never bind-mounts ~/.claude as a whole).
func AuthSyncCommand(runtime, homeVolume, homeTarget, user string, files []AuthSyncFile, dryRun bool) (string, error) {
	if homeVolume == "" || homeTarget == "" {
		return "", fmt.Errorf("home volume and target required")
	}
	if runtime == "" {
		runtime = "docker"
	}
	homeTarget = strings.TrimRight(homeTarget, "/")
	var b strings.Builder
	b.WriteString("set -e; ")
	fmt.Fprintf(&b, "HV=%s; HT=%s; ", shellquote.Quote(homeVolume), shellquote.Quote(homeTarget))
	if dryRun {
		b.WriteString("echo DRY_RUN; ")
		for _, f := range files {
			srcQ := shellquote.Quote(f.HostRel)
			fmt.Fprintf(&b, "if [ -e \"$HOME\"/%s ]; then echo WOULD_COPY %s; else ", srcQ, srcQ)
			if f.Optional {
				fmt.Fprintf(&b, "echo SKIP_MISSING %s; ", srcQ)
			} else {
				fmt.Fprintf(&b, "echo MISSING %s; exit 1; ", srcQ)
			}
			b.WriteString("fi; ")
		}
		b.WriteString("echo AUTH_SYNC_OK")
		return HostLoginShell(b.String()), nil
	}

	// One helper; docker cp each path. Paths after the colon are allowlist-
	// constructed absolutes (no host expansion), so they stay unquoted.
	fmt.Fprintf(&b, `CID=$(%s run -d -v "$HV:$HT" alpine:3.20 sleep 7200); `, runtime)
	fmt.Fprintf(&b, `cleanup() { %s rm -f "$CID" >/dev/null 2>&1 || true; }; trap cleanup EXIT; `, runtime)

	for _, f := range files {
		volPath := homeTarget + "/" + f.VolumeRel
		srcQ := shellquote.Quote(f.HostRel)
		parent := homeTarget
		if i := strings.LastIndex(volPath, "/"); i >= 0 {
			parent = volPath[:i]
		}
		fmt.Fprintf(&b, "if [ -e \"$HOME\"/%s ]; then ", srcQ)
		fmt.Fprintf(&b, `%s exec "$CID" mkdir -p %s >/dev/null; `, runtime, shellquote.Quote(parent))
		if f.DirCopy {
			fmt.Fprintf(&b, `%s exec "$CID" sh -c %s >/dev/null; `, runtime,
				shellquote.Quote(fmt.Sprintf("rm -rf %s && mkdir -p %s", shellquote.Quote(volPath), shellquote.Quote(volPath))))
			fmt.Fprintf(&b, `%s cp "$HOME"/%s/. "$CID":%s/ >/dev/null; `, runtime, srcQ, volPath)
		} else {
			fmt.Fprintf(&b, `%s cp "$HOME"/%s "$CID":%s >/dev/null; `, runtime, srcQ, volPath)
		}
		fmt.Fprintf(&b, "echo COPIED %s; ", srcQ)
		fmt.Fprintf(&b, "else ")
		if f.Optional {
			fmt.Fprintf(&b, "echo SKIP_MISSING %s; ", srcQ)
		} else {
			fmt.Fprintf(&b, "echo MISSING %s; exit 1; ", srcQ)
		}
		b.WriteString("fi; ")
	}
	if user != "" && user != "root" && user != "0" {
		var uid int
		if _, err := fmt.Sscanf(user, "%d", &uid); err == nil {
			fmt.Fprintf(&b, `%s exec "$CID" chown -R %d %s; `, runtime, uid, shellquote.Quote(homeTarget))
		} else {
			b.WriteString("echo chown-deferred; ")
		}
	}
	b.WriteString("cleanup; trap - EXIT; echo AUTH_SYNC_OK")
	return HostLoginShell(b.String()), nil
}

// AuthSync runs the allowlisted credential copy into the home volume for hostID.
func (s *ContainerService) AuthSync(ctx context.Context, hostID, containerName, agent string, dryRun bool) (*AuthSyncResult, error) {
	t, spec, err := s.resolve(ctx, hostID, containerName)
	if err != nil {
		// Allow sync against host defaults with a synthetic name when the
		// caller passes "-" / "" and agent_volumes is set — but the CLI always
		// has a container. Fall through.
		return nil, err
	}
	profile, err := s.Profiles.Get(ctx, hostID, false)
	if err != nil {
		return nil, err
	}
	spec = profile.ApplyAgentVolumes(spec)
	if spec.Home == nil || spec.Home.Volume == "" {
		return nil, fmt.Errorf("container %q has no home volume (set home: or host agent_volumes.home)", containerName)
	}
	files, err := AuthSyncFilesFor(agent)
	if err != nil {
		return nil, err
	}
	cmd, err := AuthSyncCommand(spec.RuntimeVerb(), spec.Home.Volume, spec.Home.Target, spec.User, files, dryRun)
	if err != nil {
		return nil, err
	}
	out, errOut, runErr := t.Run(ctx, "", cmd)
	combined := strings.TrimSpace(out + "\n" + errOut)
	res := &AuthSyncResult{
		HostID: hostID,
		Home:   spec.Home.Volume,
		Target: spec.Home.Target,
		Agent:  agent,
		DryRun: dryRun,
	}
	for _, line := range strings.Split(combined, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "COPIED "):
			res.Files = append(res.Files, strings.TrimPrefix(line, "COPIED "))
		case strings.HasPrefix(line, "WOULD_COPY "):
			res.Files = append(res.Files, strings.TrimPrefix(line, "WOULD_COPY "))
		case strings.HasPrefix(line, "SKIP_MISSING "):
			res.Skipped = append(res.Skipped, strings.TrimPrefix(line, "SKIP_MISSING "))
		}
	}
	if runErr != nil {
		res.Detail = combined
		return res, fmt.Errorf("auth sync failed on %s: %w\n--- output ---\n%s", hostID, runErr, combined)
	}
	res.OK = true
	res.Detail = firstLine(combined)
	return res, nil
}

// AgentStatusProbeCommand asks a running container which toolkit agents are
// present and whether their allowlisted credential files exist under $HOME.
func AgentStatusProbeCommand(c *ContainerSpec, ref ContainerRef) (string, error) {
	agents := []string{"claude", "codex", "ccs", "cursor-agent"}
	if c != nil && c.Toolkit != nil && len(c.Toolkit.Agents) > 0 {
		agents = append([]string(nil), c.Toolkit.Agents...)
	}
	var body strings.Builder
	body.WriteString("set +e; ")
	for _, a := range agents {
		bin := a
		cred := ""
		switch a {
		case "claude":
			cred = "$HOME/.claude/.credentials.json"
		case "codex":
			cred = "$HOME/.codex/auth.json"
		case "ccs":
			cred = "$HOME/.ccs"
		case "cursor-agent", "cursor":
			bin = "cursor-agent"
			cred = "$HOME/.cursor/auth.json"
		}
		fmt.Fprintf(&body,
			`bin=$(command -v %s 2>/dev/null); ver=""; [ -n "$bin" ] && ver=$(%s --version 2>/dev/null | head -n1); auth=no; `,
			shellquote.Quote(bin), shellquote.Quote(bin))
		if cred != "" {
			fmt.Fprintf(&body, `if [ -e %s ] || [ -e "$HOME/.claude.json" ]; then auth=yes; fi; `, shellquote.Quote(cred))
		}
		fmt.Fprintf(&body, `if [ -n "$bin" ]; then echo "AGENT %s present $auth $ver"; else echo "AGENT %s absent $auth"; fi; `, a, a)
	}
	return ContainerExec(c.RuntimeVerb(), ref, body.String(), false)
}

// ParseAgentStatusLines reads AgentStatusProbeCommand output.
func ParseAgentStatusLines(out string) []AgentVolumeStatus {
	var rows []AgentVolumeStatus
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[0] != "AGENT" {
			continue
		}
		st := AgentVolumeStatus{Agent: fields[1], Binary: fields[2] == "present"}
		if len(fields) >= 4 {
			st.Authed = fields[3] == "yes"
		}
		if len(fields) >= 5 {
			st.Version = strings.Join(fields[4:], " ")
		}
		rows = append(rows, st)
	}
	return rows
}

// probeAgentStatuses fills st.Agents when the container is running and mounts a toolkit.
func probeAgentStatuses(ctx context.Context, t ports.Transport, spec *ContainerSpec, st *ContainerStatus) {
	if st == nil || !st.Running || st.ContainerID == "" || spec == nil || spec.Toolkit == nil {
		return
	}
	ref := spec.RefFor(st.ContainerID, "")
	if st.Instance != "" {
		ref = spec.RefFor(st.Instance, "")
	}
	if st.User != "" {
		ref.User = st.User
	}
	cmd, err := AgentStatusProbeCommand(spec, ref)
	if err != nil {
		return
	}
	out, _, err := t.Run(ctx, "", cmd)
	if err != nil {
		return
	}
	st.Agents = ParseAgentStatusLines(out)
}
