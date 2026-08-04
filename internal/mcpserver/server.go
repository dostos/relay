// Package mcpserver exposes Relay's existing authenticated CLI boundary as one
// narrow MCP stdio tool. It owns framing and size limits only; semantic policy
// remains in the home command boundary invoked by the child relay process.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	maxMessageBytes = 1 << 20
	maxOutputBytes  = 1 << 20
	maxArgBytes     = 256 << 10
)

type ExecuteResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

type Executor func(context.Context, []string) (ExecuteResult, error)

type Server struct {
	Execute Executor
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Execute == nil {
		return fmt.Errorf("relay MCP executor is unavailable")
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := encoder.Encode(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}}); err != nil {
				return err
			}
			continue
		}
		result, rpcErr := s.handle(ctx, req)
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		if err := encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("relay MCP input: %w", err)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		protocol := "2025-06-18"
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) == nil && params.ProtocolVersion != "" {
			protocol = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "relay", "version": "1"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        "relay",
			"description": "Call the authenticated Relay control plane. Start with argv [\"agent\",\"protocol\"] and follow returned next/argv. Security, login, trust, credential, and permission gates must be surfaced without choosing an answer.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"argv":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 64},
					"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "default": 120},
				},
				"required": []string{"argv"},
			},
		}}}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Argv           []string `json:"argv"`
			TimeoutSeconds int      `json:"timeout_seconds"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil || call.Name != "relay" {
		return nil, &rpcError{Code: -32602, Message: "invalid relay tool call"}
	}
	if err := validateArgs(call.Arguments.Argv); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	timeout := call.Arguments.TimeoutSeconds
	if timeout == 0 {
		timeout = 120
	}
	if timeout < 1 || timeout > 600 {
		return nil, &rpcError{Code: -32602, Message: "timeout_seconds must be between 1 and 600"}
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	result, err := s.Execute(callCtx, call.Arguments.Argv)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	text := result.Stdout
	if result.Stderr != "" {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "[stderr]\n" + result.Stderr
	}
	if len(text) > maxOutputBytes {
		text = text[:maxOutputBytes] + "\n[relay MCP output truncated]"
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": result,
		"isError":           result.ExitCode != 0,
	}, nil
}

func validateArgs(args []string) error {
	if len(args) == 0 || len(args) > 64 {
		return fmt.Errorf("argv must contain 1 to 64 entries")
	}
	total := 0
	for _, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("argv contains a NUL byte")
		}
		total += len(arg)
	}
	if total > maxArgBytes {
		return fmt.Errorf("argv exceeds %d bytes", maxArgBytes)
	}
	// Prevent a tool call from recursively replacing the stdio server. This is
	// a transport-liveness invariant, not semantic Relay authorization.
	if args[0] == "mcp" {
		return fmt.Errorf("nested relay mcp commands are unavailable")
	}
	return nil
}

func RelayExecutor(executable string) Executor {
	return func(ctx context.Context, args []string) (ExecuteResult, error) {
		if executable == "" {
			return ExecuteResult{}, fmt.Errorf("relay executable is unavailable")
		}
		cmdArgs := append([]string{"--json"}, args...)
		cmd := exec.CommandContext(ctx, executable, cmdArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		result := ExecuteResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if err == nil {
			return result, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ExecuteResult{}, fmt.Errorf("relay tool call timed out")
		}
		return ExecuteResult{}, fmt.Errorf("start relay client: %w", err)
	}
}

func Command(args []string) int {
	if len(args) == 2 && args[0] == "install" && args[1] == "cursor" {
		return installCursor()
	}
	if len(args) != 1 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: relay mcp serve | relay mcp install cursor")
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := (&Server{Execute: RelayExecutor(executable)}).Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
