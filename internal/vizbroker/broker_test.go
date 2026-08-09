package vizbroker

import (
	"encoding/json"
	"testing"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func TestValidAckAcceptsProjectionAndLifecycleSchemas(t *testing.T) {
	projectResult, _ := json.Marshal(map[string]any{"session_id": "sess-1", "revision": 7, "surface": "surface:9"})
	project := map[string]any{
		"request_seq": float64(7), "request_kind": "project", "op": "upsert",
		"session_id": "sess-1", "result": string(projectResult), "build": "build-1",
	}
	lifecycle := map[string]any{
		"request_seq": float64(8), "request_kind": "update_relayd",
		"result": "build-1", "build": "build-1",
	}
	if !ValidAck(project) || !ValidAck(lifecycle) {
		t.Fatal("valid projection or lifecycle acknowledgement refused")
	}
	failure := map[string]any{
		"request_seq": float64(9), "request_kind": "update_relayd",
		"result": "old-build", "build": "old-build", "status": "failed", "error": "worktree is dirty",
	}
	if !ValidAck(failure) {
		t.Fatal("typed lifecycle failure acknowledgement refused")
	}
	failure["error"] = ""
	if ValidAck(failure) {
		t.Fatal("empty lifecycle failure accepted")
	}
	lifecycle["session_id"] = "sess-injected"
	if ValidAck(lifecycle) {
		t.Fatal("lifecycle acknowledgement accepted projection-only fields")
	}
}

func TestBrokerRefusesCommandsOutsideProjectionProtocol(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "relay service status")
	if code := Command([]string{"--service", "relay-viz-mac"}); code != 126 {
		t.Fatalf("broker exit=%d want=126", code)
	}
}

// A headless root is a service, not a pane. It carries HostID "local", which is
// not a resolvable SSH alias -- and because an invalid alias aborts the WHOLE
// snapshot, including one froze every viz client: the Mac's `relay viz serve`
// crash-looped on `SSH target "local" is absent from authority config`, never
// replaced its local snapshot, and its dashboard kept showing sessions that had
// been destroyed hours before. Observed 2026-08-09.
func TestAuthoritySnapshotSkipsHeadlessRoots(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)

	reg := &core.Registry{}
	headless := &core.Session{
		ID: "sess-headless", HostID: "local",
		Persist: ports.PersistHandle{Kind: "headless", Name: "apex-hermes"},
		Labels:  map[string]string{"headless": "true", "role": "parent"},
	}
	if err := reg.PutSession(headless); err != nil {
		t.Fatalf("save headless: %v", err)
	}

	items, err := authoritySnapshot()
	if err != nil {
		t.Fatalf("a headless root must not break the snapshot for every viz "+
			"client: %v", err)
	}
	for _, item := range items {
		if item.SessionID == "sess-headless" {
			t.Fatalf("headless root was projected; it has no pane to present")
		}
	}
}

// The strictness that caught real registry corruption must survive: a session
// that is NOT headless and carries an unusable target is still a hard error.
func TestAuthoritySnapshotStillRejectsAnIncompleteRealSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)

	reg := &core.Registry{}
	broken := &core.Session{
		ID: "sess-broken", HostID: "definitely-not-a-configured-alias",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "broken"},
	}
	if err := reg.PutSession(broken); err != nil {
		t.Fatalf("save broken: %v", err)
	}

	if _, err := authoritySnapshot(); err == nil {
		t.Fatal("a real session with an unresolvable target must still fail loudly")
	}
}
