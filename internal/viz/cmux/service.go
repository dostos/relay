package cmux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	resp, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "project", map[string]any{
		"op":                string(ports.ProjectionUpsert),
		"session_id":        req.SessionID,
		"parent_session_id": req.ParentSessionID,
		"target":            req.Target,
		"tmux_name":         req.TmuxName,
	})
	if err != nil {
		return 0, err
	}
	return resp.Seq, nil
}

func (v *Viz) queueClose(sessionID string) error {
	if !v.queueAvailable() {
		return fmt.Errorf("visualization request queue unavailable")
	}
	_, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "project", map[string]any{
		"op":         string(ports.ProjectionDelete),
		"session_id": sessionID,
	})
	return err
}

func (v *Viz) queueFocus(req ports.Presentation) error {
	if !v.queueAvailable() {
		return fmt.Errorf("visualization request queue unavailable")
	}
	_, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "project", map[string]any{
		"op": string(ports.ProjectionFocus), "session_id": req.SessionID,
		"parent_session_id": req.ParentSessionID, "target": req.Target, "tmux_name": req.TmuxName,
	})
	return err
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

// QueueControlRetirement asks the optional display host to remove its legacy
// control processes after the authoritative host has been verified.
func (v *Viz) QueueControlRetirement() (int64, error) {
	if v.ServiceID == "" || v.Control != nil {
		return 0, fmt.Errorf("control retirement is requested by the control host")
	}
	resp, err := coordrelayd.EmitLocal(localRelaydSocket(), v.serviceChannel(), "retire_control", nil)
	if err != nil {
		return 0, err
	}
	return resp.Seq, nil
}

func (v *Viz) cursorPath() string {
	return filepath.Join(core.StateRoot(), "viz", "service-"+v.ServiceID+".cursor")
}

func (v *Viz) loadCursor() int64 {
	return loadSequence(v.cursorPath())
}

func loadSequence(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	seq, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return seq
}

func (v *Viz) saveCursor(seq int64) error {
	return saveSequence(v.cursorPath(), seq)
}

func saveSequence(path string, seq int64) error {
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
	if err := retireLocalAuthorityState(); err != nil {
		return err
	}
	followBit := 0
	if follow {
		followBit = 1
	}
	command := fmt.Sprintf("viz-subscribe %s %d %d", v.serviceChannel(), v.loadCursor(), followBit)
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

// retireLocalAuthorityState makes the Viz host projection-only. The archived
// files are recoverable, but no longer sit at paths a local Relay command can
// mistake for current session, lineage, inbox, or credential authority.
func retireLocalAuthorityState() error {
	return core.RetireLocalAuthorityState()
}

func (v *Viz) handleServiceEvent(ctx context.Context, event coord.Event) (string, error) {
	switch event.Kind {
	case "project":
		projection := ports.ProjectionEvent{
			V: 1, Revision: event.Seq, Op: ports.ProjectionOp(stringMeta(event.Meta, "op")),
			Item: presentationFromMeta(event.Meta),
		}
		surface, err := v.ApplyProjection(ctx, projection)
		if err != nil {
			return "", err
		}
		location := v.locationOfSurface(ctx, surface)
		result, _ := json.Marshal(map[string]any{
			"session_id": projection.Item.SessionID, "revision": projection.Revision,
			"surface": surface, "workspace": location.Workspace, "pane": location.Pane,
			"parent_session_id": projection.Item.ParentSessionID,
		})
		return string(result), nil
	case "present":
		req := presentationFromMeta(event.Meta)
		surface, err := v.ApplyProjection(ctx, ports.ProjectionEvent{V: 1, Revision: event.Seq, Op: ports.ProjectionUpsert, Item: req})
		if err != nil {
			return "", err
		}
		location := v.locationOfSurface(ctx, surface)
		result, _ := json.Marshal(map[string]string{
			"surface": surface, "workspace": location.Workspace, "pane": location.Pane,
			"parent_session_id": req.ParentSessionID,
		})
		return string(result), nil
	case "close":
		if _, err := v.ApplyProjection(ctx, ports.ProjectionEvent{V: 1, Revision: event.Seq, Op: ports.ProjectionDelete, Item: presentationFromMeta(event.Meta)}); err != nil {
			return "", err
		}
		return "closed", nil
	case "update_relayd":
		return v.updateRelayd(ctx)
	case "retire_control":
		return v.retireControl(ctx)
	case "viz_ack":
		return "ignored", nil
	default:
		return "", fmt.Errorf("unsupported visualization event %q", event.Kind)
	}
}

func presentationFromMeta(meta map[string]any) ports.Presentation {
	return ports.Presentation{
		SessionID: stringMeta(meta, "session_id"), ParentSessionID: stringMeta(meta, "parent_session_id"),
		Target: stringMeta(meta, "target"), TmuxName: stringMeta(meta, "tmux_name"),
	}
}

func (v *Viz) retireControl(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("legacy visualization control retirement is macOS-only")
	}
	uid := strconv.Itoa(os.Getuid())
	label := "com.dostos.relay-supervisor"
	domain := "gui/" + uid + "/" + label
	if err := exec.CommandContext(ctx, "launchctl", "print", domain).Run(); err == nil {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "launchctl", "bootout", domain)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("disable legacy supervisor: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
	}
	if exec.CommandContext(ctx, "launchctl", "print", domain).Run() == nil {
		return "", fmt.Errorf("legacy supervisor still loaded after bootout")
	}
	home, _ := os.UserHomeDir()
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	removedPlist := false
	if err := os.Remove(plist); err == nil {
		removedPlist = true
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("remove legacy supervisor registration: %w", err)
	}

	stoppedBridge := false
	socket := core.DesktopBridgeSocketPath()
	out, err := exec.CommandContext(ctx, "lsof", "-t", socket).Output()
	if err == nil {
		for _, field := range strings.Fields(string(out)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr != nil || pid == os.Getpid() {
				continue
			}
			command, commandErr := exec.CommandContext(ctx, "ps", "-p", field, "-o", "command=").Output()
			if commandErr != nil || !isLegacyBridgeCommand(string(command)) {
				return "", fmt.Errorf("refuse to stop unrecognized desktop socket owner pid %s", field)
			}
			process, findErr := os.FindProcess(pid)
			if findErr != nil {
				return "", findErr
			}
			if signalErr := process.Signal(syscall.SIGTERM); signalErr != nil {
				return "", fmt.Errorf("stop legacy desktop bridge pid %d: %w", pid, signalErr)
			}
			stoppedBridge = true
		}
	} else if _, ok := err.(*exec.ExitError); !ok {
		return "", fmt.Errorf("inspect legacy desktop bridge: %w", err)
	}
	for attempt := 0; attempt < 20 && socketOwned(ctx, socket); attempt++ {
		time.Sleep(100 * time.Millisecond)
	}
	if socketOwned(ctx, socket) {
		return "", fmt.Errorf("legacy desktop bridge still owns %s", socket)
	}
	vizLoaded := exec.CommandContext(ctx, "launchctl", "print", "gui/"+uid+"/com.dostos.relay-viz").Run() == nil
	if !vizLoaded {
		return "", fmt.Errorf("visualization follower is not loaded after control retirement")
	}
	result, _ := json.Marshal(map[string]any{
		"supervisor_plist_removed": removedPlist,
		"supervisor_loaded":        false,
		"bridge_stopped":           stoppedBridge,
		"bridge_socket_owned":      false,
		"viz_preserved":            vizLoaded,
	})
	return string(result), nil
}

func socketOwned(ctx context.Context, socket string) bool {
	out, err := exec.CommandContext(ctx, "lsof", "-t", socket).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func isLegacyBridgeCommand(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	return len(fields) >= 2 && filepath.Base(fields[0]) == "relayd" && fields[1] == "bridge"
}

func stringMeta(meta map[string]any, key string) string {
	value, _ := meta[key].(string)
	return value
}

func (v *Viz) emitAck(ctx context.Context, event coord.Event, result string) error {
	if event.Kind != "project" || stringMeta(event.Meta, "op") != string(ports.ProjectionUpsert) {
		return nil
	}
	meta, _ := json.Marshal(map[string]any{
		"request_seq": event.Seq, "request_kind": event.Kind, "result": result, "build": coord.Build,
		"session_id": stringMeta(event.Meta, "session_id"), "op": stringMeta(event.Meta, "op"),
	})
	command := "viz-ack " + v.serviceChannel() + " " + base64.RawURLEncoding.EncodeToString(meta)
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
