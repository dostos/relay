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
