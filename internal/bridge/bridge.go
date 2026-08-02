// Package bridge connects relay processes inside remote tmux sessions back to
// the relay control plane on the cmux machine.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// SocketEnv is injected into every relay-owned remote tmux session. SSH
	// stream-local forwarding maps this remote path to the desktop socket.
	SocketEnv = "RELAY_BRIDGE_SOCK"
	// LocalInvokeEnv prevents the desktop relay subprocess from forwarding the
	// same request back through the bridge.
	LocalInvokeEnv   = "RELAY_BRIDGE_LOCAL_INVOKE"
	SourceSessionEnv = "RELAY_SOURCE_SESSION_ID"
	SourceHostEnv    = "RELAY_SOURCE_HOST_ID"
	SourcePersistEnv = "RELAY_SOURCE_PERSIST_NAME"
	SourceTokenEnv   = "RELAY_SOURCE_TOKEN"
)

const maxMessageBytes = 4 << 20

// Source identifies the relay session that issued an invocation.
type Source struct {
	SessionID   string `json:"session_id,omitempty"`
	HostID      string `json:"host_id,omitempty"`
	PersistName string `json:"persist_name,omitempty"`
	Token       string `json:"token,omitempty"`
}

// Request is one newline-delimited bridge request.
type Request struct {
	V      int      `json:"v"`
	Op     string   `json:"op"`
	Argv   []string `json:"argv,omitempty"`
	Source Source   `json:"source,omitempty"`
}

// Response is one newline-delimited bridge response.
type Response struct {
	OK       bool   `json:"ok"`
	Build    string `json:"build,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Client invokes the desktop bridge through a local or SSH-forwarded socket.
type Client struct{ SockPath string }

func (c Client) Ping(ctx context.Context) error {
	_, err := c.Status(ctx)
	return err
}

// Status proves the bridge is responsive and returns the running build so
// callers do not confuse an old but live process with the installed control.
func (c Client) Status(ctx context.Context) (*Response, error) {
	resp, err := c.call(ctx, Request{V: 1, Op: "ping"})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("bridge ping: %s", resp.Error)
	}
	return resp, nil
}

func (c Client) Invoke(ctx context.Context, argv []string, source Source) (*Response, error) {
	return c.call(ctx, Request{V: 1, Op: "invoke", Argv: argv, Source: source})
}

func (c Client) call(ctx context.Context, req Request) (*Response, error) {
	if strings.TrimSpace(c.SockPath) == "" {
		return nil, fmt.Errorf("bridge socket required")
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.SockPath)
	if err != nil {
		return nil, fmt.Errorf("desktop relay bridge unavailable at %s: %w", c.SockPath, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("desktop relay bridge returned no response")
	}
	var resp Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Server owns the desktop Unix socket. Remote requests can execute only the
// relay binary directly (never a shell), and invocations are serialized so
// registry writes from separate handoffs cannot overwrite one another.
type Server struct {
	SockPath  string
	RelayBin  string
	Build     string
	Authorize func(Source) error
	ln        net.Listener
	invokeMu  sync.Mutex
}

func (s *Server) Serve() error {
	if s.SockPath == "" || s.RelayBin == "" {
		return fmt.Errorf("bridge socket and relay binary required")
	}
	if err := os.MkdirAll(filepath.Dir(s.SockPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.SockPath)
	ln, err := net.Listen("unix", s.SockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.SockPath, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.ln = ln
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	_ = os.Remove(s.SockPath)
	return s.ln.Close()
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)
	if !sc.Scan() {
		return
	}
	var req Request
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		s.writeResponse(conn, Response{OK: false, ExitCode: 2, Error: "bad bridge request"})
		return
	}
	switch req.Op {
	case "ping":
		s.writeResponse(conn, Response{OK: true})
	case "invoke":
		s.writeResponse(conn, s.invoke(req))
	default:
		s.writeResponse(conn, Response{OK: false, ExitCode: 2, Error: "unknown bridge operation"})
	}
}

func (s *Server) writeResponse(conn net.Conn, resp Response) {
	resp.Build = s.Build
	writeResponse(conn, resp)
}

func (s *Server) invoke(req Request) Response {
	if err := validateArgv(req.Argv); err != nil {
		return Response{OK: false, ExitCode: 2, Error: err.Error()}
	}
	if s.Authorize != nil {
		if err := s.Authorize(req.Source); err != nil {
			return Response{OK: false, ExitCode: 1, Error: err.Error()}
		}
	}
	if serializeInvocation(req.Argv) {
		s.invokeMu.Lock()
		defer s.invokeMu.Unlock()
	}

	cmd := exec.Command(s.RelayBin, req.Argv...)
	cmd.Env = append(desktopInvokeEnv(os.Environ()),
		LocalInvokeEnv+"=1",
		SourceSessionEnv+"="+req.Source.SessionID,
		SourceHostEnv+"="+req.Source.HostID,
		SourcePersistEnv+"="+req.Source.PersistName,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
	}
	resp := Response{OK: err == nil, ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil && resp.Stderr == "" {
		resp.Error = err.Error()
	}
	return resp
}

func serializeInvocation(argv []string) bool {
	var filtered []string
	for _, arg := range argv {
		if arg != "--json" {
			filtered = append(filtered, arg)
		}
	}
	if len(filtered) == 2 {
		reserved := map[string]bool{
			"agent": true, "handoff": true, "history": true, "help": true, "version": true, "targets": true,
			"resolve": true, "log": true,
		}
		if !reserved[filtered[0]] {
			return true
		}
	}
	if len(filtered) < 2 {
		return false
	}
	if filtered[0] == "resolve" {
		return true
	}
	switch filtered[0] + " " + filtered[1] {
	case "agent start", "agent done", "handoff finalize", "handoff reconcile", "parent reply", "parent ack", "parent state", "parent retire", "session cleanup":
		return true
	}
	// `relay handoff -H …` is the launch form; its second token is a flag.
	return filtered[0] == "handoff" && strings.HasPrefix(filtered[1], "-")
}

func desktopInvokeEnv(env []string) []string {
	blocked := map[string]bool{
		"CMUX_WORKSPACE_ID": true,
		"CMUX_SURFACE_REF":  true,
		"CMUX_SURFACE":      true,
		SourceSessionEnv:    true,
		SourceHostEnv:       true,
		SourcePersistEnv:    true,
		SourceTokenEnv:      true,
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked[key] {
			out = append(out, entry)
		}
	}
	return out
}

func validateArgv(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty relay argv")
	}
	if len(argv) > 128 {
		return fmt.Errorf("too many relay arguments")
	}
	for _, arg := range argv {
		if len(arg) > 256*1024 || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("invalid relay argument")
		}
	}
	var filtered []string
	for _, arg := range argv {
		if arg != "--json" {
			filtered = append(filtered, arg)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("empty relay command")
	}
	// Remote panes may create named sessions, operate handoffs, or inspect
	// lineage. Host bootstrap/auth and raw session mutation remain desktop-only.
	reserved := map[string]bool{
		"host": true, "auth": true, "targets": true, "session": true, "sess": true,
		"handoff": true, "agent": true, "parent": true, "policy": true, "msg": true, "gc": true, "events": true,
		"viz": true, "pane": true, "resume": true, "doctor": true, "history": true,
		"help": true, "version": true, "install-cmux-restore": true,
		"resolve": true, "log": true,
		// board and root are reserved so the two-token host shorthand below
		// cannot admit them unchecked. board is re-allowed per subcommand;
		// root stays desktop-only because the apex lifecycle is an operator
		// action and its digest is the human's decision queue.
		"board": true, "root": true,
	}
	if len(filtered) == 2 && !reserved[filtered[0]] && !strings.HasPrefix(filtered[0], "-") && !strings.HasPrefix(filtered[1], "-") {
		return nil
	}
	switch filtered[0] {
	case "session":
		if len(filtered) != 3 || filtered[1] != "cleanup" || strings.HasPrefix(filtered[2], "-") {
			return fmt.Errorf("only relay session cleanup ID is allowed through the desktop bridge")
		}
		return nil
	case "resume":
		// Session discovery must use the host probe, not the optimistic local
		// registry. Keep interactive resume and registry mutation desktop-only.
		if len(filtered) == 3 && filtered[1] == "list" && filtered[2] == "--probe" {
			return nil
		}
		return fmt.Errorf("only relay resume list --probe is allowed through the desktop bridge")
	case "parent":
		if len(filtered) < 2 {
			return fmt.Errorf("parent subcommand required")
		}
		// Remote children may inspect/respond to the durable goal channel, but
		// cannot register, relink, retire, or otherwise mutate local panes.
		allowed := map[string]bool{"inbox": true, "reply": true, "ack": true, "status": true, "send": true}
		if !allowed[filtered[1]] {
			return fmt.Errorf("relay parent %q is not allowed through the desktop bridge", filtered[1])
		}
		return nil
	case "board":
		if len(filtered) < 2 {
			return fmt.Errorf("board subcommand required")
		}
		// Peers coordinate through the board; the caller's identity is taken
		// from the authenticated envelope, so these are safe to forward.
		allowed := map[string]bool{"post": true, "query": true, "watch": true}
		if !allowed[filtered[1]] {
			return fmt.Errorf("relay board %q is not allowed through the desktop bridge", filtered[1])
		}
		return nil
	case "agent", "handoff", "history", "help", "version", "targets", "resolve", "log":
		return nil
	default:
		return fmt.Errorf("relay command %q is not allowed through the desktop bridge", filtered[0])
	}
}

func writeResponse(conn net.Conn, resp Response) {
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}
