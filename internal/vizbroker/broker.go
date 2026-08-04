// Package vizbroker implements the forced-command protocol exposed by the
// authoritative home service to an optional visualization-only host.
package vizbroker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// Command is deliberately restricted: the enrolled key can read only its
// projection snapshot/stream and append schema-checked acknowledgements.
func Command(args []string) int {
	service := ""
	if len(args) == 2 && args[0] == "--service" {
		service = args[1]
	}
	if service == "" || !strings.HasPrefix(service, "relay-viz-") {
		fmt.Fprintln(os.Stderr, "relay viz-broker: valid --service required")
		return 2
	}
	fields := strings.Fields(strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND")))
	if len(fields) == 2 && fields[0] == "viz-snapshot-v2" && fields[1] == service {
		snapshot, err := authoritySnapshotV2(service)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(snapshot)
		return 0
	}
	if len(fields) == 3 && fields[0] == "viz-resolve" && fields[1] == service {
		resolution, err := authorityResume(fields[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resolution)
		return 0
	}
	if len(fields) == 3 && fields[0] == "viz-target" && fields[1] == service {
		target, err := authorityTarget(fields[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(target)
		return 0
	}
	if len(fields) == 2 && fields[0] == "viz-snapshot" && fields[1] == service {
		items, err := authoritySnapshot()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(items)
		return 0
	}
	sock := eventSocketPath()
	if len(fields) == 4 && fields[0] == "viz-subscribe" && fields[1] == service {
		from, err := strconv.ParseInt(fields[2], 10, 64)
		follow := fields[3] == "1"
		if err != nil || from < 0 || (fields[3] != "0" && fields[3] != "1") {
			fmt.Fprintln(os.Stderr, "relay viz-broker: invalid cursor")
			return 2
		}
		if err := coordrelayd.SubscribeLocal(sock, service, from, follow, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(fields) == 3 && fields[0] == "viz-ack" && fields[1] == service {
		raw, err := base64.RawURLEncoding.DecodeString(fields[2])
		if err != nil || len(raw) > 64<<10 {
			fmt.Fprintln(os.Stderr, "relay viz-broker: invalid acknowledgement")
			return 2
		}
		var meta map[string]any
		if json.Unmarshal(raw, &meta) != nil || !ValidAck(meta) {
			fmt.Fprintln(os.Stderr, "relay viz-broker: acknowledgement schema refused")
			return 2
		}
		kind := "viz_ack"
		if meta["request_kind"] == "update_relayd" {
			kind = "client_ack"
		}
		resp, err := coordrelayd.EmitLocal(sock, service+"-ack", kind, meta)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		return 0
	}
	fmt.Fprintln(os.Stderr, "relay viz-broker: command refused")
	return 126
}

func ValidAck(meta map[string]any) bool {
	for key := range meta {
		switch key {
		case "request_seq", "request_kind", "result", "build", "session_id", "op", "status", "error":
		default:
			return false
		}
	}
	seq, ok := meta["request_seq"].(float64)
	if !ok || seq < 1 || seq != float64(int64(seq)) {
		return false
	}
	result, ok := meta["result"].(string)
	if !ok || len(result) < 1 || len(result) > 8192 {
		return false
	}
	build, ok := meta["build"].(string)
	if !ok || len(build) < 1 || len(build) > 128 || strings.ContainsAny(build, "\r\n\x00") {
		return false
	}
	kind, _ := meta["request_kind"].(string)
	if kind == "update_relayd" || kind == "retire_control" {
		_, hasSession := meta["session_id"]
		_, hasOp := meta["op"]
		if hasSession || hasOp || strings.ContainsRune(result, '\x00') {
			return false
		}
		status, _ := meta["status"].(string)
		if status == "" {
			_, hasError := meta["error"]
			return !hasError
		}
		failure, ok := meta["error"].(string)
		return status == "failed" && ok && len(failure) > 0 && len(failure) <= 2048 && !strings.ContainsRune(failure, '\x00')
	}
	if _, hasStatus := meta["status"]; hasStatus {
		return false
	}
	if _, hasError := meta["error"]; hasError {
		return false
	}
	if kind != "project" || meta["op"] != "upsert" {
		return false
	}
	session, ok := meta["session_id"].(string)
	if !ok || len(session) < 1 || len(session) > 128 || strings.ContainsAny(session, "\r\n\x00") {
		return false
	}
	var receipt struct {
		SessionID string `json:"session_id"`
		Revision  int64  `json:"revision"`
		Surface   string `json:"surface"`
	}
	if json.Unmarshal([]byte(result), &receipt) != nil {
		return false
	}
	return receipt.SessionID == session && receipt.Revision == int64(seq) && strings.HasPrefix(receipt.Surface, "surface:") && len(receipt.Surface) <= 128
}

func eventSocketPath() string {
	if value := os.Getenv("RELAYD_SOCK"); value != "" {
		return value
	}
	return filepath.Join(core.StateRoot(), "relayd.sock")
}

func authoritySnapshot() ([]ports.Presentation, error) {
	sessions, err := (&core.Registry{}).ListSessions()
	if err != nil {
		return nil, err
	}
	items := make([]ports.Presentation, 0, len(sessions))
	for _, session := range sessions {
		if session == nil || session.ID == "" || session.HostID == "" || session.Persist.Name == "" {
			return nil, fmt.Errorf("session registry contains incomplete visualization identity")
		}
		target, err := core.ResolveTarget(session.HostID)
		if err != nil {
			return nil, err
		}
		items = append(items, ports.Presentation{
			SessionID: session.ID, ParentSessionID: session.SourceSessionID,
			Target: session.HostID, TmuxName: session.Persist.Name,
			SSHHost: target.Hostname, SSHUser: target.User, SSHPort: target.Port,
			ProjectionRevision: queuedProjectionRevision(session.VizSurfaceRef),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SessionID < items[j].SessionID })
	return items, nil
}

func queuedProjectionRevision(ref string) int64 {
	const prefix = "viz:queued:"
	if !strings.HasPrefix(ref, prefix) {
		return 0
	}
	revision, err := strconv.ParseInt(strings.TrimPrefix(ref, prefix), 10, 64)
	if err != nil || revision <= 0 {
		return 0
	}
	return revision
}

func authoritySnapshotV2(service string) (*ports.AuthoritySnapshot, error) {
	revision := func() (int64, error) {
		store, err := coordrelayd.NewStore(filepath.Join(core.StateRoot(), "events"))
		if err != nil {
			return 0, err
		}
		return store.LastSeq(service)
	}
	for attempt := 0; attempt < 8; attempt++ {
		before, err := revision()
		if err != nil {
			return nil, err
		}
		current, err := authoritySnapshot()
		if err != nil {
			return nil, err
		}
		after, err := revision()
		if err != nil {
			return nil, err
		}
		if before == after {
			return &ports.AuthoritySnapshot{V: 1, Revision: after, Items: current}, nil
		}
	}
	return nil, fmt.Errorf("visualization authority changed continuously while taking snapshot")
}

func authorityResume(persistName string) (*ports.ResumeResolution, error) {
	if err := shellquote.ValidateSessionName(persistName); err != nil {
		return nil, err
	}
	sessions, err := (&core.Registry{}).ListSessions()
	if err != nil {
		return nil, err
	}
	var matched *core.Session
	for _, session := range sessions {
		if session.Persist.Name != persistName {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("multiple authoritative sessions use persist name %q", persistName)
		}
		matched = session
	}
	if matched == nil {
		return nil, fmt.Errorf("session %q is absent from the authoritative registry", persistName)
	}
	target, err := core.ResolveTarget(matched.HostID)
	if err != nil {
		return nil, err
	}
	return &ports.ResumeResolution{SessionID: matched.ID, Target: matched.HostID, TmuxName: matched.Persist.Name, SSHHost: target.Hostname, SSHUser: target.User, SSHPort: target.Port}, nil
}

func authorityTarget(sessionID string) (*ports.ResumeTarget, error) {
	if err := shellquote.ValidateSessionName(sessionID); err != nil {
		return nil, err
	}
	session, err := (&core.Registry{}).GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	target, err := core.ResolveTarget(session.HostID)
	if err != nil {
		return nil, err
	}
	return &ports.ResumeTarget{Host: target.Hostname, User: target.User, Port: target.Port}, nil
}
