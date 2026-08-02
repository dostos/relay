package main

import (
	"encoding/json"
	"testing"
)

func TestVizBrokerRefusesCommandsOutsideProjectionProtocol(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "relayd status")
	if code := cmdVizBroker([]string{"--service", "relay-viz-mac"}); code != 126 {
		t.Fatalf("broker exit=%d, want refusal", code)
	}
}

func TestVizBrokerRequiresPinnedService(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "viz-subscribe relay-viz-mac 0 0")
	if code := cmdVizBroker([]string{"--service", "other"}); code != 2 {
		t.Fatalf("broker exit=%d, want invalid configuration", code)
	}
}

func TestValidVizAckAcceptsSeparateProjectionAndLifecycleSchemas(t *testing.T) {
	projectResult, _ := json.Marshal(map[string]any{"session_id": "sess-1", "revision": 7, "surface": "surface:9"})
	project := map[string]any{
		"request_seq": float64(7), "request_kind": "project", "op": "upsert",
		"session_id": "sess-1", "result": string(projectResult), "build": "2438e49",
	}
	update := map[string]any{
		"request_seq": float64(8), "request_kind": "update_relayd",
		"result": "2438e49", "build": "2438e49",
	}
	if !validVizAck(project) || !validVizAck(update) {
		t.Fatal("valid projection or lifecycle acknowledgement refused")
	}
	update["session_id"] = "sess-injected"
	if validVizAck(update) {
		t.Fatal("lifecycle acknowledgement accepted projection-only fields")
	}
}
