package cmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func (v *Viz) authoritySnapshotPath() string {
	return filepath.Join(core.StateRoot(), "viz", "authority-snapshot.json")
}

func (v *Viz) fetchAuthoritySnapshot(ctx context.Context) error {
	command := "viz-snapshot " + v.serviceChannel()
	args, err := v.controlSSHArgs(command)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("visualization authority snapshot: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() > 4<<20 {
		return fmt.Errorf("visualization authority snapshot exceeds 4 MiB")
	}
	var items []ports.Presentation
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		return fmt.Errorf("invalid visualization authority snapshot: %w", err)
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if seen[item.SessionID] {
			return fmt.Errorf("duplicate session %s in visualization authority snapshot", item.SessionID)
		}
		if err := validatePresentation(item); err != nil {
			return err
		}
		seen[item.SessionID] = true
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return saveBytes(v.authoritySnapshotPath(), raw, 0o600)
}

func saveBytes(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (v *Viz) ResolveProjectedResume(_ context.Context, persistName string) (ports.ResumeTarget, error) {
	raw, err := os.ReadFile(v.authoritySnapshotPath())
	if err != nil {
		return ports.ResumeTarget{}, fmt.Errorf("current visualization authority snapshot unavailable: %w", err)
	}
	var snapshot []ports.Presentation
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return ports.ResumeTarget{}, fmt.Errorf("invalid visualization authority snapshot: %w", err)
	}
	var matched *ports.Presentation
	for i := range snapshot {
		item := &snapshot[i]
		if item.TmuxName != persistName {
			continue
		}
		if matched != nil {
			return ports.ResumeTarget{}, fmt.Errorf("multiple authoritative sessions use persist name %q", persistName)
		}
		matched = item
	}
	if matched == nil {
		return ports.ResumeTarget{}, fmt.Errorf("session %q is absent from the current authoritative projection", persistName)
	}
	mapped, ok := v.Targets[matched.Target]
	if !ok {
		return ports.ResumeTarget{}, fmt.Errorf("no visualization target policy for authority host %q", matched.Target)
	}
	if !vizTargetRE.MatchString(mapped.Host) || (mapped.User != "" && !vizTargetRE.MatchString(mapped.User)) || mapped.Port < 0 || mapped.Port > 65535 || strings.ContainsAny(mapped.Identity, "\r\n\x00") {
		return ports.ResumeTarget{}, fmt.Errorf("invalid visualization target mapping for %q", matched.Target)
	}
	return ports.ResumeTarget{Host: mapped.Host, User: mapped.User, Port: mapped.Port, Identity: expandServicePath(mapped.Identity)}, nil
}
