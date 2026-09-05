package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// What is left of this file after the apex retired: where this machine's
// control plane is declared, and where a project's rules file lives. Neither
// governs anything. The enrollment and apex model the file was named for is
// gone; see docs/2026-09-05-retire-hierarchy-plan.md.

// RootService is the remaining namespace for those two concerns. It holds no
// state: its only method takes what it needs as parameters.
type RootService struct{}

// ControlPlane describes where governance actually runs. Enrolling a root
// makes its subtree governed, but governance only happens while the machine
// holding the registry, the inboxes, and the watcher processes is awake — a
// laptop that sleeps takes the router with it, and an escalation raised in the
// meantime is not routed until it wakes. Relay cannot detect that on its own,
// so it never claims always-on without being told.
type ControlPlane struct {
	Host       string `json:"host"`
	HostID     string `json:"host_id,omitempty"`
	AlwaysOn   bool   `json:"always_on"`
	DeclaredBy string `json:"declared_by,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

type controlPlaneDeclaration struct {
	V        int    `json:"v"`
	HostID   string `json:"host_id"`
	AlwaysOn bool   `json:"always_on"`
}

func controlPlaneDeclarationPath() string {
	return filepath.Join(ConfigRoot(), "control-plane.json")
}

// DescribeControlPlane reports the locality caveat honestly. Declare
// RELAY_CONTROL_PLANE_ALWAYS_ON=1 only on a host that does not sleep.
func DescribeControlPlane() ControlPlane {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "this machine"
	}
	cp := ControlPlane{Host: host}
	if raw, err := os.ReadFile(filepath.Join(ConfigRoot(), "host.yaml")); err == nil {
		if profile, parseErr := ParseHostProfileYAML(raw); parseErr == nil {
			cp.HostID = strings.TrimSpace(profile.HostID)
		}
	}
	if raw, err := os.ReadFile(controlPlaneDeclarationPath()); err == nil {
		var declaration controlPlaneDeclaration
		if json.Unmarshal(raw, &declaration) == nil && declaration.V == 1 && cp.HostID != "" && declaration.HostID == cp.HostID && declaration.AlwaysOn {
			cp.AlwaysOn = true
			cp.DeclaredBy = "host_config"
		}
	}
	switch strings.TrimSpace(os.Getenv("RELAY_CONTROL_PLANE_ALWAYS_ON")) {
	case "1", "true", "yes":
		cp.AlwaysOn = true
		cp.DeclaredBy = "environment"
	}
	if !cp.AlwaysOn {
		cp.Warning = "governance runs on " + host +
			"; enrolled subtrees are governed only while it is awake. An escalation raised" +
			" while it sleeps is not routed until it wakes. Run relay root control-plane --always-on" +
			" only on a host that does not sleep."
	}
	return cp
}

// SetLocalControlPlaneAlwaysOn persists the human's availability policy in a
// host-identity-bound config so shells, relayd, the supervisor and restarts all
// report the same value. This is a declaration, not a liveness inference.
func SetLocalControlPlaneAlwaysOn(alwaysOn bool) (*ControlPlane, error) {
	hostPath := filepath.Join(ConfigRoot(), "host.yaml")
	raw, err := os.ReadFile(hostPath)
	if err != nil {
		return nil, fmt.Errorf("read local host profile: %w", err)
	}
	profile, err := ParseHostProfileYAML(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(profile.HostID) == "" {
		return nil, fmt.Errorf("local host profile has no host_id")
	}
	next, err := json.MarshalIndent(controlPlaneDeclaration{V: 1, HostID: strings.TrimSpace(profile.HostID), AlwaysOn: alwaysOn}, "", "  ")
	if err != nil {
		return nil, err
	}
	path := controlPlaneDeclarationPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := writeOwnerFile(tmp, append(next, '\n')); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(tmp, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return nil, err
	}
	if err := dir.Close(); err != nil {
		return nil, err
	}
	cp := DescribeControlPlane()
	return &cp, nil
}

// RulesDir is where human-authored per-project rules live. It defaults inside
// the relay config root, but RELAY_RULES_DIR lets it point at a versioned
// workspace directory — rules are the human's delegation envelope, and keeping
// them in version control is the point.
func RulesDir() string {
	if v := strings.TrimSpace(os.Getenv("RELAY_RULES_DIR")); v != "" {
		return v
	}
	return filepath.Join(ConfigRoot(), "rules")
}

// RulesPath resolves one project's rule file.
func RulesPath(project string) (string, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", fmt.Errorf("project required")
	}
	if strings.ContainsAny(project, "/\\") || project == ".." {
		return "", fmt.Errorf("project %q must be a bare name", project)
	}
	return filepath.Join(RulesDir(), project+".md"), nil
}
