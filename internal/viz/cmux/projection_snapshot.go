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
