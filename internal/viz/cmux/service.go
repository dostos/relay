package cmux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dostos/relay/internal/controlstate"
	"github.com/dostos/relay/internal/coord"
	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/selfupdate"
	"github.com/dostos/relay/internal/shellquote"
)

func (v *Viz) serviceChannel() string { return "relay-viz-" + v.ServiceID }

func localRelaydSocket() string {
	if socket := os.Getenv("RELAYD_SOCK"); socket != "" {
		return socket
	}
	return filepath.Join(core.StateRoot(), "relayd.sock")
}

func (v *Viz) queueAvailable() bool {
	if shellquote.ValidateSessionName(v.serviceChannel()) != nil {
		return false
	}
	_, err := coordrelayd.PingLocal(localRelaydSocket())
	return err == nil
}

func (v *Viz) queuePresentation(req ports.Presentation) (int64, error) {
	if !v.queueAvailable() {
		return 0, fmt.Errorf("visualization request queue unavailable")
	}
	resp, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "present", map[string]any{
		"session_id": req.SessionID,
		"target":     req.Target,
		"tmux_name":  req.TmuxName,
	})
	if err != nil {
		return 0, err
	}
	return resp.Seq, nil
}

// QueueUpdate emits a durable update signal. Source, repository, branch, and
// install policy are never supplied by the sender; they are fixed in the
// visualization host's owner-only config.
func (v *Viz) QueueUpdate() (int64, error) {
	if v.ServiceID == "" || v.Control != nil {
		return 0, fmt.Errorf("update signals are emitted by the control host")
	}
	resp, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "update_relayd", nil)
	if err != nil {
		return 0, err
	}
	return resp.Seq, nil
}

func (v *Viz) QueueControlMigration() (int64, error) {
	if v.ServiceID == "" || v.Control != nil {
		return 0, fmt.Errorf("control migration is requested by the destination control host")
	}
	resp, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "migrate_control", nil)
	if err != nil {
		return 0, err
	}
	return resp.Seq, nil
}

func (v *Viz) cursorPath() string {
	return filepath.Join(core.StateRoot(), "viz", "service-"+v.ServiceID+".cursor")
}

func (v *Viz) loadCursor() int64 {
	raw, err := os.ReadFile(v.cursorPath())
	if err != nil {
		return 0
	}
	seq, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return seq
}

func (v *Viz) saveCursor(seq int64) error {
	path := v.cursorPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (v *Viz) controlSSHArgs(remoteCommand string) ([]string, error) {
	if v.Control == nil || !vizTargetRE.MatchString(v.Control.Host) || (v.Control.User != "" && !vizTargetRE.MatchString(v.Control.User)) || v.Control.Port < 0 || v.Control.Port > 65535 {
		return nil, fmt.Errorf("valid visualization control target required")
	}
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5"}
	if v.Control.Identity != "" {
		args = append(args, "-i", expandServicePath(v.Control.Identity))
	}
	if v.Control.Port > 0 {
		args = append(args, "-p", strconv.Itoa(v.Control.Port))
	}
	target := v.Control.Host
	if v.Control.User != "" {
		target = v.Control.User + "@" + target
	}
	return append(args, target, remoteCommand), nil
}

func (v *Viz) remoteRelayd(command string) string {
	bin := v.Control.Relayd
	if bin == "" {
		bin = ".local/bin/relayd"
	}
	if strings.HasPrefix(bin, "/") {
		return shellquote.Quote(bin) + " " + command
	}
	bin = strings.TrimPrefix(bin, "~/")
	return `"$HOME"/` + shellquote.Quote(bin) + " " + command
}

// Follow consumes the durable visualization stream over an outbound SSH
// connection from the optional client to the always-on control host.
func (v *Viz) Follow(ctx context.Context, follow bool) error {
	if v.ServiceID == "" || v.Control == nil {
		return fmt.Errorf("visualization service_id and control are required")
	}
	command := v.remoteRelayd(fmt.Sprintf("subscribe -s %s --from %d", shellquote.Quote(v.serviceChannel()), v.loadCursor()))
	if follow {
		command += " -f"
	}
	args, err := v.controlSSHArgs(command)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		var event coord.Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Heartbeat || event.Seq <= v.loadCursor() {
			continue
		}
		result, handleErr := v.handleServiceEvent(ctx, event)
		if handleErr != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("viz event %d %s: %w", event.Seq, event.Kind, handleErr)
		}
		if err := v.emitAck(ctx, event, result); err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		if err := v.saveCursor(event.Seq); err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		if event.Kind == "update_relayd" {
			_ = cmd.Process.Kill()
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("visualization control stream: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (v *Viz) handleServiceEvent(ctx context.Context, event coord.Event) (string, error) {
	switch event.Kind {
	case "present":
		req := ports.Presentation{
			SessionID: stringMeta(event.Meta, "session_id"),
			Target:    stringMeta(event.Meta, "target"),
			TmuxName:  stringMeta(event.Meta, "tmux_name"),
		}
		return v.PresentTarget(ctx, req)
	case "update_relayd":
		return v.updateRelayd(ctx)
	case "migrate_control":
		return v.migrateControl(ctx)
	case "viz_ack":
		return "ignored", nil
	default:
		return "", fmt.Errorf("unsupported visualization event %q", event.Kind)
	}
}

func (v *Viz) migrateControl(ctx context.Context) (string, error) {
	bundle, err := controlstate.Export(&core.Registry{})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	args, err := v.controlSSHArgs(v.remoteRelayd("control import"))
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("control import: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var response struct {
		OK      bool                 `json:"ok"`
		Summary controlstate.Summary `json:"summary"`
	}
	if json.Unmarshal(stdout.Bytes(), &response) != nil || !response.OK {
		return "", fmt.Errorf("invalid control import response: %s", strings.TrimSpace(stdout.String()))
	}
	result, _ := json.Marshal(response.Summary)
	return string(result), nil
}

func stringMeta(meta map[string]any, key string) string {
	value, _ := meta[key].(string)
	return value
}

func (v *Viz) emitAck(ctx context.Context, event coord.Event, result string) error {
	meta, _ := json.Marshal(map[string]any{"request_seq": event.Seq, "request_kind": event.Kind, "result": result, "build": coord.Build})
	command := v.remoteRelayd("emit -s " + shellquote.Quote(v.serviceChannel()+"-ack") + " --kind viz_ack --meta " + shellquote.Quote(string(meta)))
	args, err := v.controlSSHArgs(command)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("visualization ack: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (v *Viz) updateRelayd(ctx context.Context) (string, error) {
	if v.Update == nil || strings.TrimSpace(v.Update.Repo) == "" {
		return "", fmt.Errorf("visualization update policy is not configured")
	}
	home, _ := os.UserHomeDir()
	result, err := selfupdate.Apply(ctx, selfupdate.Plan{
		Repo:       expandServicePath(v.Update.Repo),
		Remote:     v.Update.Remote,
		Branch:     v.Update.Branch,
		InstallDir: filepath.Join(home, ".local", "bin"),
		StateDir:   core.StateRoot(),
	})
	if err != nil {
		return "", err
	}
	return result.Build, nil
}

func expandServicePath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
