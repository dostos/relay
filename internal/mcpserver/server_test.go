package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestServerAdvertisesAndExecutesOneRelayTool(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"relay","arguments":{"argv":["agent","protocol"]}}}`,
	}, "\n") + "\n"
	var called []string
	server := &Server{Execute: func(_ context.Context, args []string) (ExecuteResult, error) {
		called = append([]string{}, args...)
		return ExecuteResult{Stdout: `{"ok":true}`, ExitCode: 0}, nil
	}}
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 || strings.Join(called, " ") != "agent protocol" {
		t.Fatalf("lines=%d called=%v output=%s", len(lines), called, output.String())
	}
	var listed map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil || !strings.Contains(lines[1], `"name":"relay"`) {
		t.Fatalf("tool list=%s err=%v", lines[1], err)
	}
	if !strings.Contains(lines[2], `"exit_code":0`) || !strings.Contains(lines[2], `"isError":false`) {
		t.Fatalf("tool result=%s", lines[2])
	}
}

func TestServerRejectsNestedMCPAndOversizedFrames(t *testing.T) {
	server := &Server{Execute: func(context.Context, []string) (ExecuteResult, error) {
		t.Fatal("executor called for invalid request")
		return ExecuteResult{}, nil
	}}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"relay","arguments":{"argv":["mcp","serve"]}}}` + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "nested relay mcp") {
		t.Fatalf("unexpected response: %s", output.String())
	}
	tooLarge := strings.Repeat("x", maxMessageBytes+1)
	if err := server.Serve(context.Background(), strings.NewReader(tooLarge), &bytes.Buffer{}); err == nil {
		t.Fatal("oversized input accepted")
	}
}
