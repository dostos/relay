package core

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// UpsertPathMapEntry adds or updates one path_map entry in a host.yaml
// document, preserving everything else in the file.
//
// It edits a yaml.Node tree rather than round-tripping through HostProfile.
// Re-marshalling the struct would produce a valid file and silently delete
// every comment in it, and these files are commented: they carry the reasons a
// mapping exists. A tool that erases the reasoning to record a fact is not a
// good trade.
//
// One cost remains and is not fixable at this layer: yaml.v3 keeps comments but
// not blank lines, so a file with paragraph breaks comes back with them
// collapsed. Verified on a real host profile -- content and comments identical,
// four blank lines gone. Reformatting is the price of a safe structural edit;
// deleting the comments would not have been.
func UpsertPathMapEntry(document []byte, match, remoteCWD string) ([]byte, error) {
	match = strings.TrimSpace(match)
	remoteCWD = strings.TrimSpace(remoteCWD)
	if match == "" {
		return nil, fmt.Errorf("match required")
	}
	if remoteCWD == "" {
		return nil, fmt.Errorf("remote_cwd required")
	}

	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("parse host profile: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("host profile is not a YAML document")
	}
	body := root.Content[0]
	if body.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("host profile is not a mapping")
	}

	seq := findOrCreateSequence(body, "path_map")

	// An existing entry for the same match is UPDATED, never duplicated: two
	// entries claiming the same repo is a file that answers a question twice.
	for _, entry := range seq.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if value := mappingValue(entry, "match"); value != nil && value.Value == match {
			if cwd := mappingValue(entry, "remote_cwd"); cwd != nil {
				cwd.Value = remoteCWD
				cwd.Tag = "!!str"
				return encodeDocument(&root)
			}
			entry.Content = append(entry.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "remote_cwd"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: remoteCWD})
			return encodeDocument(&root)
		}
	}

	seq.Content = append(seq.Content, &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "match"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: match},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "remote_cwd"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: remoteCWD},
		},
	})
	return encodeDocument(&root)
}

func encodeDocument(root *yaml.Node) ([]byte, error) {
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func findOrCreateSequence(body *yaml.Node, key string) *yaml.Node {
	if existing := mappingValue(body, key); existing != nil {
		if existing.Kind == yaml.SequenceNode {
			return existing
		}
		// A key that exists but is not a list (usually an empty `path_map:`)
		// is converted rather than refused.
		existing.Kind = yaml.SequenceNode
		existing.Tag = "!!seq"
		existing.Value = ""
		existing.Content = nil
		return existing
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	body.Content = append(body.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, seq)
	return seq
}

// MapRepo records where a local repo lives on a host, in that host's own
// host.yaml.
//
// The mapping lives on the host and not in the client that wrote it: relay's
// own verbs resolve --repo through this same file, so a client keeping a
// private copy would be a second answer to one question, and the two would
// drift. A client may EDIT this; it may not own it.
func (p *ProfileService) MapRepo(ctx context.Context, t ports.Transport, hostID, match, remoteCWD string) (*HostProfile, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	path := RemoteHostProfilePath()
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", path, hostID, err)
	}
	updated, err := UpsertPathMapEntry(raw, match, remoteCWD)
	if err != nil {
		return nil, err
	}
	if err := t.WriteFile(ctx, path, updated, "644"); err != nil {
		return nil, fmt.Errorf("write %s on %s: %w", path, hostID, err)
	}
	return p.Fetch(ctx, hostID)
}

// UpsertContainerEntry declares a devcontainer in a host.yaml, preserving the
// rest of the file for the same reason UpsertPathMapEntry does.
//
// It writes the minimum that `relay container open` needs: a name and the
// workspace folder holding .devcontainer. Toolkit and home volumes are NOT
// invented here -- those are choices about what an agent needs, and guessing
// them would put volumes on a shared host that nobody asked for.
func UpsertContainerEntry(document []byte, name, workspaceFolder string) ([]byte, error) {
	name = strings.TrimSpace(name)
	workspaceFolder = strings.TrimSpace(workspaceFolder)
	if name == "" {
		return nil, fmt.Errorf("container name required")
	}
	if workspaceFolder == "" {
		return nil, fmt.Errorf("workspace_folder required")
	}

	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("parse host profile: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("host profile is not a YAML document")
	}
	body := root.Content[0]
	if body.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("host profile is not a mapping")
	}

	seq := findOrCreateSequence(body, "containers")
	for _, entry := range seq.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if value := mappingValue(entry, "name"); value != nil && value.Value == name {
			dev := mappingValue(entry, "devcontainer")
			if dev == nil || dev.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("container %q already exists and is not a devcontainer; leaving it alone", name)
			}
			if folder := mappingValue(dev, "workspace_folder"); folder != nil {
				folder.Value = workspaceFolder
				folder.Tag = "!!str"
				return encodeDocument(&root)
			}
			dev.Content = append(dev.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "workspace_folder"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: workspaceFolder})
			return encodeDocument(&root)
		}
	}

	seq.Content = append(seq.Content, &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "devcontainer"},
			{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "workspace_folder"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: workspaceFolder},
			}},
		},
	})
	return encodeDocument(&root)
}

// DeclareContainer records a devcontainer in the host's own profile.
func (p *ProfileService) DeclareContainer(ctx context.Context, t ports.Transport, hostID, name, workspaceFolder string) (*HostProfile, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	path := RemoteHostProfilePath()
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", path, hostID, err)
	}
	updated, err := UpsertContainerEntry(raw, name, workspaceFolder)
	if err != nil {
		return nil, err
	}
	if err := t.WriteFile(ctx, path, updated, "644"); err != nil {
		return nil, fmt.Errorf("write %s on %s: %w", path, hostID, err)
	}
	return p.Fetch(ctx, hostID)
}

// ImageContainerDecl is the set of fields `relay host container --image` writes.
type ImageContainerDecl struct {
	Name       string
	Image      string
	User       string
	GPU        string
	Network    string
	Home       string   // VOLUME:/target
	Toolkit    string   // VOLUME:/target
	Agents     []string // toolkit.agents recipes (claude, codex, …)
	Volumes    []string // NAME:/path[:ro]
	Binds      []string // /abs:/abs[:ro] → expose:
	Env        []string
	DefaultCWD string
}

// Spec builds the ContainerSpec this declaration records.
func (d ImageContainerDecl) Spec() (*ContainerSpec, error) {
	name := strings.TrimSpace(d.Name)
	image := strings.TrimSpace(d.Image)
	if name == "" {
		return nil, fmt.Errorf("container name required")
	}
	if image == "" {
		return nil, fmt.Errorf("image required")
	}
	spec := &ContainerSpec{
		Name:       name,
		Image:      image,
		User:       strings.TrimSpace(d.User),
		GPU:        strings.TrimSpace(d.GPU),
		Network:    strings.TrimSpace(d.Network),
		DefaultCWD: strings.TrimSpace(d.DefaultCWD),
		Volumes:    append([]string(nil), d.Volumes...),
		Expose:     append([]string(nil), d.Binds...),
		Env:        append([]string(nil), d.Env...),
	}
	if d.Home != "" {
		vm, err := ParseVolumeMount(d.Home)
		if err != nil {
			return nil, fmt.Errorf("home: %w", err)
		}
		spec.Home = &HomeSpec{Volume: vm.Volume, Target: vm.Target}
	}
	if d.Toolkit != "" {
		vm, err := ParseVolumeMount(d.Toolkit)
		if err != nil {
			return nil, fmt.Errorf("toolkit: %w", err)
		}
		spec.Toolkit = &ToolkitSpec{Volume: vm.Volume, Target: vm.Target, Agents: append([]string(nil), d.Agents...)}
	} else if len(d.Agents) > 0 {
		return nil, fmt.Errorf("--agents requires --toolkit")
	}
	if spec.Toolkit != nil && len(spec.Toolkit.Agents) > 0 {
		if _, err := ComposeAgentRecipes(spec.Toolkit.Agents, spec.Toolkit.Target); err != nil {
			return nil, err
		}
	}
	for _, v := range spec.Volumes {
		if _, err := ParseVolumeMount(v); err != nil {
			return nil, fmt.Errorf("volume: %w", err)
		}
	}
	for _, b := range spec.Expose {
		if _, err := ParseBindMount(b); err != nil {
			return nil, fmt.Errorf("bind: %w", err)
		}
	}
	for _, e := range spec.Env {
		if err := validateEnvName(e); err != nil {
			return nil, err
		}
	}
	if spec.GPU != "" {
		switch spec.GPU {
		case "all", "none":
		default:
			if _, err := gpuArgs(spec.GPU); err != nil {
				return nil, fmt.Errorf("gpu: %w", err)
			}
		}
	}
	return spec, nil
}

// UpsertImageContainerEntry declares or replaces an image-backed container in
// host.yaml. A pre-existing entry of another kind is refused.
func UpsertImageContainerEntry(document []byte, decl ImageContainerDecl) ([]byte, error) {
	spec, err := decl.Spec()
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("parse host profile: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("host profile is not a YAML document")
	}
	body := root.Content[0]
	if body.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("host profile is not a mapping")
	}
	entry, err := containerSpecYAMLNode(spec)
	if err != nil {
		return nil, err
	}
	seq := findOrCreateSequence(body, "containers")
	for i, existing := range seq.Content {
		if existing.Kind != yaml.MappingNode {
			continue
		}
		if value := mappingValue(existing, "name"); value != nil && value.Value == spec.Name {
			if mappingValue(existing, "devcontainer") != nil {
				return nil, fmt.Errorf("container %q already exists as a devcontainer; leaving it alone", spec.Name)
			}
			if mappingValue(existing, "container") != nil && mappingValue(existing, "image") == nil {
				return nil, fmt.Errorf("container %q already exists as an exec-into-existing container; leaving it alone", spec.Name)
			}
			seq.Content[i] = entry
			return encodeDocument(&root)
		}
	}
	seq.Content = append(seq.Content, entry)
	return encodeDocument(&root)
}

func containerSpecYAMLNode(spec *ContainerSpec) (*yaml.Node, error) {
	raw, err := yaml.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("marshal container spec produced no document")
	}
	return doc.Content[0], nil
}

// DeclareImageContainer records an image-backed container in the host profile.
func (p *ProfileService) DeclareImageContainer(ctx context.Context, t ports.Transport, hostID string, decl ImageContainerDecl) (*HostProfile, *ContainerSpec, error) {
	if hostID == "" {
		return nil, nil, fmt.Errorf("host required")
	}
	spec, err := decl.Spec()
	if err != nil {
		return nil, nil, err
	}
	path := RemoteHostProfilePath()
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s on %s: %w", path, hostID, err)
	}
	updated, err := UpsertImageContainerEntry(raw, decl)
	if err != nil {
		return nil, nil, err
	}
	if err := t.WriteFile(ctx, path, updated, "644"); err != nil {
		return nil, nil, fmt.Errorf("write %s on %s: %w", path, hostID, err)
	}
	profile, err := p.Fetch(ctx, hostID)
	if err != nil {
		return nil, nil, err
	}
	return profile, spec, nil
}

// CloneRepoCommand clones a GitHub repo onto a host and reports what it found.
//
// It uses the HOST's gh, so the host's own credentials apply and no token from
// this machine is forwarded to a shared box. An existing checkout is left
// alone rather than re-cloned: the directory may hold work.
//
// The output ends in one machine-readable line, because the caller has to
// branch on whether the repo carries a .devcontainer and reading that out of
// git's own chatter would be guesswork.
func CloneRepoCommand(repo, parent string) (string, error) {
	if strings.TrimSpace(repo) == "" {
		return "", fmt.Errorf("repo required")
	}
	parentExpr, err := shellquote.PathExpr(parent)
	if err != nil {
		return "", err
	}
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	if name == "" || strings.ContainsAny(name, "/ \t\n") {
		return "", fmt.Errorf("repo %q has no usable name", repo)
	}
	// A raw literal: this script is full of shell quoting, and escaping it
	// through an interpreted Go string is how the last two attempts broke.
	const script = `set -e
mkdir -p %[1]s
cd %[1]s
if [ -d %[2]s/.git ]; then state=exists; else gh repo clone %[3]s %[2]s >&2; state=cloned; fi
if [ -d %[2]s/.devcontainer ] || [ -f %[2]s/.devcontainer.json ]; then dev=yes; else dev=no; fi
printf 'RELAY_CLONE %%s %%s/%%s %%s\n' "$state" "$(pwd)" %[2]s "$dev"`
	return fmt.Sprintf(script, parentExpr, shellquote.Quote(name), shellquote.Quote(repo)), nil
}

// ParseCloneResult reads the one line CloneRepoCommand ends with.
func ParseCloneResult(output string) (state, path string, devcontainer bool, err error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 4 && fields[0] == "RELAY_CLONE" {
			return fields[1], fields[2], fields[3] == "yes", nil
		}
	}
	return "", "", false, fmt.Errorf("clone produced no result line")
}

// RemoteDirsCommand lists the immediate subdirectories of dir on a host.
//
// It exists so a client can offer a folder picker without speaking SSH itself.
// Bounded on purpose: one level, directories only, capped, so a picker pointed
// at a large tree cannot hang on a listing nobody could read anyway.
func RemoteDirsCommand(dir string) (string, error) {
	expr, err := shellquote.PathExpr(dir)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("cd %s 2>/dev/null && ls -1ApL 2>/dev/null | grep '/$' | head -500", expr), nil
}

// AgentVolumesDecl is the host-level toolkit + home default written by
// `relay host agent-volumes`.
type AgentVolumesDecl struct {
	Toolkit string   // VOLUME:/target
	Home    string   // VOLUME:/target
	Agents  []string // toolkit.agents recipes
}

// UpsertAgentVolumes sets or replaces the top-level agent_volumes: block.
// Comments elsewhere in the file are preserved (yaml.Node edit); the
// agent_volumes subtree itself is rewritten as a unit.
func UpsertAgentVolumes(document []byte, decl AgentVolumesDecl) ([]byte, error) {
	tkMount, err := ParseVolumeMount(decl.Toolkit)
	if err != nil {
		return nil, fmt.Errorf("toolkit: %w", err)
	}
	homeMount, err := ParseVolumeMount(decl.Home)
	if err != nil {
		return nil, fmt.Errorf("home: %w", err)
	}
	agents := decl.Agents
	if len(agents) == 0 {
		agents = DefaultForgeAgents()
	}
	if _, err := ComposeAgentRecipes(agents, tkMount.Target); err != nil {
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("parse host profile: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("host profile is not a YAML document")
	}
	body := root.Content[0]
	if body.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("host profile is not a mapping")
	}

	agentsSeq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, a := range agents {
		agentsSeq.Content = append(agentsSeq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: a})
	}
	av := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "toolkit"},
			{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "volume"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: tkMount.Volume},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "target"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: tkMount.Target},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "agents"},
				agentsSeq,
			}},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "home"},
			{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "volume"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: homeMount.Volume},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "target"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: homeMount.Target},
			}},
		},
	}

	replaced := false
	for i := 0; i+1 < len(body.Content); i += 2 {
		if body.Content[i].Value == "agent_volumes" {
			body.Content[i+1] = av
			replaced = true
			break
		}
	}
	if !replaced {
		body.Content = append(body.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "agent_volumes"}, av)
	}
	return encodeDocument(&root)
}

// SetAgentVolumes writes host-level agent_volumes into the host's profile.
func (p *ProfileService) SetAgentVolumes(ctx context.Context, t ports.Transport, hostID string, decl AgentVolumesDecl) (*HostProfile, error) {
	if hostID == "" {
		return nil, fmt.Errorf("host required")
	}
	path := RemoteHostProfilePath()
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", path, hostID, err)
	}
	updated, err := UpsertAgentVolumes(raw, decl)
	if err != nil {
		return nil, err
	}
	if err := t.WriteFile(ctx, path, updated, "644"); err != nil {
		return nil, fmt.Errorf("write %s on %s: %w", path, hostID, err)
	}
	return p.Fetch(ctx, hostID)
}
