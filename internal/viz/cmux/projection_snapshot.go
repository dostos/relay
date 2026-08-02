package cmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

func (v *Viz) authoritySnapshotPath() string {
	return filepath.Join(core.StateRoot(), "viz", "authority-snapshot.json")
}

func (v *Viz) fetchAuthoritySnapshot(ctx context.Context) error {
	command := "viz-snapshot-v2 " + v.serviceChannel()
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
	var snapshot ports.AuthoritySnapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil || snapshot.V != 1 || snapshot.Revision < 0 {
		return fmt.Errorf("invalid visualization authority snapshot: %w", err)
	}
	items := snapshot.Items
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
	raw, err := json.Marshal(snapshot)
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

func decodeAuthoritySnapshot(raw []byte) (ports.AuthoritySnapshot, error) {
	var snapshot ports.AuthoritySnapshot
	if err := json.Unmarshal(raw, &snapshot); err == nil && snapshot.V == 1 && snapshot.Revision >= 0 {
		return snapshot, nil
	}
	var legacy []ports.Presentation
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return ports.AuthoritySnapshot{}, fmt.Errorf("invalid visualization authority snapshot: %w", err)
	}
	return ports.AuthoritySnapshot{V: 1, Items: legacy}, nil
}

// updateAuthoritySnapshot applies the already-effected stream event to the
// local authority view. The cursor is saved only after this succeeds, so a
// crash retries both the idempotent pane projection and this snapshot update.
func (v *Viz) updateAuthoritySnapshot(event ports.ProjectionEvent) error {
	if event.Op != ports.ProjectionUpsert && event.Op != ports.ProjectionDelete {
		return fmt.Errorf("unsupported authority projection operation %q", event.Op)
	}
	if event.Item.SessionID == "" {
		return fmt.Errorf("authority projection session_id required")
	}
	raw, err := os.ReadFile(v.authoritySnapshotPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var snapshot ports.AuthoritySnapshot
	if len(raw) > 0 {
		snapshot, err = decodeAuthoritySnapshot(raw)
		if err != nil {
			return err
		}
	}
	if event.Revision <= snapshot.Revision {
		return nil
	}
	items := snapshot.Items
	byID := make(map[string]ports.Presentation, len(items))
	for _, item := range items {
		if _, duplicate := byID[item.SessionID]; duplicate {
			return fmt.Errorf("duplicate session %s in visualization authority snapshot", item.SessionID)
		}
		if err := validatePresentation(item); err != nil {
			return err
		}
		byID[item.SessionID] = item
	}
	if event.Op == ports.ProjectionDelete {
		delete(byID, event.Item.SessionID)
	} else {
		if err := validatePresentation(event.Item); err != nil {
			return err
		}
		byID[event.Item.SessionID] = event.Item
	}
	items = items[:0]
	for _, item := range byID {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SessionID < items[j].SessionID })
	snapshot = ports.AuthoritySnapshot{V: 1, Revision: event.Revision, Items: items}
	raw, err = json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return saveBytes(v.authoritySnapshotPath(), raw, 0o600)
}

func (v *Viz) ResolveProjectedResume(ctx context.Context, persistName string, opts ports.ResumeResolveOpts) (ports.ResumeTarget, error) {
	resolution, err := v.fetchResumeResolution(ctx, persistName)
	if err == nil {
		if resolution.SSHHost == "" {
			return ports.ResumeTarget{}, fmt.Errorf("authoritative resume resolution omitted SSH endpoint")
		}
		identity := v.Targets[resolution.Target].Identity
		return validateResumeTarget(ports.ResumeTarget{Host: resolution.SSHHost, User: resolution.SSHUser, Port: resolution.SSHPort, Identity: expandServicePath(identity)})
	}
	if !opts.AllowOffline {
		return ports.ResumeTarget{}, fmt.Errorf("authoritative resume resolution unavailable: %w; use --offline only to accept the last snapshot", err)
	}
	item, fallbackErr := v.resolveOfflineSnapshot(persistName)
	if fallbackErr != nil {
		return ports.ResumeTarget{}, fmt.Errorf("authoritative resume resolution unavailable: %v; offline snapshot: %w", err, fallbackErr)
	}
	if item.SSHHost != "" {
		identity := v.Targets[item.Target].Identity
		return validateResumeTarget(ports.ResumeTarget{Host: item.SSHHost, User: item.SSHUser, Port: item.SSHPort, Identity: expandServicePath(identity)})
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

func (v *Viz) fetchAuthorityTarget(ctx context.Context, sessionID string) (*ports.ResumeTarget, error) {
	if shellquote.ValidateSessionName(sessionID) != nil {
		return nil, fmt.Errorf("invalid visualization session %q", sessionID)
	}
	args, err := v.controlSSHArgs("viz-target " + v.serviceChannel() + " " + sessionID)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("visualization authority target: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var resolved ports.ResumeTarget
	if err := json.Unmarshal(stdout.Bytes(), &resolved); err != nil || !vizTargetRE.MatchString(resolved.Host) || resolved.Port < 0 || resolved.Port > 65535 {
		return nil, fmt.Errorf("invalid visualization authority target response")
	}
	return &resolved, nil
}

func (v *Viz) resolveOfflineSnapshot(persistName string) (*ports.Presentation, error) {
	raw, err := os.ReadFile(v.authoritySnapshotPath())
	if err != nil {
		return nil, fmt.Errorf("current visualization authority snapshot unavailable: %w", err)
	}
	envelope, err := decodeAuthoritySnapshot(raw)
	if err != nil {
		return nil, err
	}
	snapshot := envelope.Items
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
	return validateResumeTarget(ports.ResumeTarget{Host: mapped.Host, User: mapped.User, Port: mapped.Port, Identity: expandServicePath(mapped.Identity)})
}

func validateResumeTarget(mapped ports.ResumeTarget) (ports.ResumeTarget, error) {
	if !vizTargetRE.MatchString(mapped.Host) || (mapped.User != "" && !vizTargetRE.MatchString(mapped.User)) || mapped.Port < 0 || mapped.Port > 65535 || strings.ContainsAny(mapped.Identity, "\r\n\x00") {
		return ports.ResumeTarget{}, fmt.Errorf("invalid visualization target mapping")
	}
	return mapped, nil
}
