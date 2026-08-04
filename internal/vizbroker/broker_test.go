package vizbroker

import (
	"encoding/json"
	"testing"
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
