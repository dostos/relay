package main

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type blockingServer struct {
	closed chan struct{}
	err    error
}

func (s *blockingServer) Serve() error { <-s.closed; return s.err }
func (s *blockingServer) Close() error { close(s.closed); return nil }

func TestServeUntilSignalTreatsListenerCloseAsCleanShutdown(t *testing.T) {
	srv := &blockingServer{closed: make(chan struct{}), err: errors.New("listener closed")}
	sig := make(chan os.Signal, 1)
	sig <- os.Interrupt
	if err := serveUntilSignal(srv, sig); err != nil {
		t.Fatalf("signal shutdown: %v", err)
	}
}

func TestServeUntilSignalPreservesUnpromptedFailure(t *testing.T) {
	want := errors.New("listen failed")
	srv := &blockingServer{closed: make(chan struct{}), err: want}
	close(srv.closed)
	if err := serveUntilSignal(srv, make(chan os.Signal)); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

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
