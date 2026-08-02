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
	"github.com/dostos/relay/internal/shellquote"
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

func (v *Viz) ResolveProjectedResume(ctx context.Context, persistName string, opts ports.ResumeResolveOpts) (ports.ResumeTarget, error) {
	resolution, err := v.fetchResumeResolution(ctx, persistName)
	if err == nil {
		return v.localResumeTarget(resolution.Target)
	}
	if !opts.AllowOffline {
		return ports.ResumeTarget{}, fmt.Errorf("authoritative resume resolution unavailable: %w; use --offline only to accept the last snapshot", err)
	}
	item, fallbackErr := v.resolveOfflineSnapshot(persistName)
	if fallbackErr != nil {
		return ports.ResumeTarget{}, fmt.Errorf("authoritative resume resolution unavailable: %v; offline snapshot: %w", err, fallbackErr)
	}
	return v.localResumeTarget(item.Target)
}

func (v *Viz) fetchResumeResolution(ctx context.Context, persistName string) (*ports.ResumeResolution, error) {
	if err := shellquote.ValidateSessionName(persistName); err != nil {
		return nil, err
	}
	args, err := v.controlSSHArgs("viz-resolve " + v.serviceChannel() + " " + persistName)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("visualization authority resolve: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() > 64<<10 {
		return nil, fmt.Errorf("visualization authority resolve exceeds 64 KiB")
	}
	var resolution ports.ResumeResolution
	if err := json.Unmarshal(stdout.Bytes(), &resolution); err != nil {
		return nil, fmt.Errorf("invalid visualization authority resolution: %w", err)
	}
	if resolution.TmuxName != persistName || resolution.SessionID == "" || resolution.Target == "" {
		return nil, fmt.Errorf("visualization authority returned mismatched resume identity")
	}
	return &resolution, nil
}

func (v *Viz) resolveOfflineSnapshot(persistName string) (*ports.Presentation, error) {
	raw, err := os.ReadFile(v.authoritySnapshotPath())
	if err != nil {
		return nil, fmt.Errorf("current visualization authority snapshot unavailable: %w", err)
	}
	var snapshot []ports.Presentation
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("invalid visualization authority snapshot: %w", err)
	}
	var matched *ports.Presentation
	for i := range snapshot {
		item := &snapshot[i]
		if item.TmuxName != persistName {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("multiple authoritative sessions use persist name %q", persistName)
		}
		matched = item
	}
	if matched == nil {
		return nil, fmt.Errorf("session %q is absent from the current authoritative projection", persistName)
	}
	return matched, nil
}

func (v *Viz) localResumeTarget(target string) (ports.ResumeTarget, error) {
	mapped, ok := v.Targets[target]
	if !ok {
		return ports.ResumeTarget{}, fmt.Errorf("no visualization target policy for authority host %q", target)
	}
	if !vizTargetRE.MatchString(mapped.Host) || (mapped.User != "" && !vizTargetRE.MatchString(mapped.User)) || mapped.Port < 0 || mapped.Port > 65535 || strings.ContainsAny(mapped.Identity, "\r\n\x00") {
		return ports.ResumeTarget{}, fmt.Errorf("invalid visualization target mapping for %q", target)
	}
	return ports.ResumeTarget{Host: mapped.Host, User: mapped.User, Port: mapped.Port, Identity: expandServicePath(mapped.Identity)}, nil
}
