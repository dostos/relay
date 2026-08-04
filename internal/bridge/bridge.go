// Package bridge connects relay processes inside remote tmux sessions back to
// the relay control plane on the cmux machine.
package bridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

const (
	maxMessageBytes        = 4 << 20
	maxCommandOutputBytes  = 256 << 10
	maxCommandReceiptBytes = maxMessageBytes
	requestReadTimeout     = 10 * time.Second
)

// Source identifies the relay session that issued an invocation.
type Source struct {
	SessionID   string `json:"session_id,omitempty"`
	HostID      string `json:"host_id,omitempty"`
	PersistName string `json:"persist_name,omitempty"`
	Token       string `json:"token,omitempty"`
	// RequestID is populated by the receiving boundary from Request.RequestID.
	// It is never accepted as caller identity material inside Source itself.
	RequestID string `json:"-"`
}

// Request is one newline-delimited bridge request.
type Request struct {
	V         int      `json:"v"`
	Op        string   `json:"op"`
	RequestID string   `json:"request_id,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Source    Source   `json:"source,omitempty"`
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
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}
	request := Request{V: 2, Op: "invoke", RequestID: requestID, Argv: argv, Source: source}
	response, err := c.call(ctx, request)
	if err == nil || ctx.Err() != nil {
		return response, err
	}
	// A dropped response is safe to retry with the same request ID: completed
	// effects return their receipt and pending effects are never repeated.
	return c.call(ctx, request)
}

func newRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create bridge request id: %w", err)
	}
	return fmt.Sprintf("%x", raw[:]), nil
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
	SockPath         string
	RelayBin         string
	Build            string
	Authorize        func(Source) error
	AuthorizeRequest func(Source, []string) error
	ReceiptDir       string
	ln               net.Listener
	invokeMu         sync.Mutex
	receiptMu        sync.Mutex
}

func (s *Server) Serve() error {
	if s.SockPath == "" || s.RelayBin == "" {
		return fmt.Errorf("bridge socket and relay binary required")
	}
	if err := os.MkdirAll(filepath.Dir(s.SockPath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.SockPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("bridge already owns %s", s.SockPath)
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	if conn, dialErr := net.DialTimeout("unix", s.SockPath, 300*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("bridge already listening at %s", s.SockPath)
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
	raw, err := readRequestLine(conn)
	if err != nil {
		s.writeResponse(conn, Response{OK: false, ExitCode: 2, Error: err.Error()})
		return
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeResponse(conn, Response{OK: false, ExitCode: 2, Error: "bad bridge request"})
		return
	}
	if req.V != 1 && req.V != 2 {
		s.writeResponse(conn, Response{OK: false, ExitCode: 2, Error: "unsupported bridge protocol version"})
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

func readRequestLine(conn net.Conn) ([]byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	reader := bufio.NewReader(io.LimitReader(conn, maxMessageBytes+2))
	line, err := reader.ReadBytes('\n')
	if len(line) > maxMessageBytes+1 || (len(line) == maxMessageBytes+1 && line[len(line)-1] != '\n') {
		return nil, fmt.Errorf("bridge request exceeds %d bytes", maxMessageBytes)
	}
	if err != nil || len(line) == 0 || line[len(line)-1] != '\n' {
		return nil, fmt.Errorf("bad bridge request")
	}
	return line[:len(line)-1], nil
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
	if req.RequestID == "" {
		// Protocol v1 compatibility cannot provide cross-retry idempotency, but
		// it still receives a unique durable execution receipt at this boundary.
		var err error
		req.RequestID, err = newRequestID()
		if err != nil {
			return Response{OK: false, ExitCode: 1, Error: err.Error()}
		}
	}
	if !validRequestID(req.RequestID) {
		return Response{OK: false, ExitCode: 2, Error: "invalid bridge request id"}
	}
	req.Source.RequestID = req.RequestID
	if s.AuthorizeRequest != nil {
		if err := s.AuthorizeRequest(req.Source, req.Argv); err != nil {
			return Response{OK: false, ExitCode: 1, Error: err.Error()}
		}
	}
	cached, receiptPath, err := s.claimRequest(req)
	if err != nil {
		return Response{OK: false, ExitCode: 1, Error: err.Error()}
	}
	if cached != nil {
		return *cached
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
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
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
	if stdout.truncated || stderr.truncated {
		resp.OK = false
		resp.ExitCode = 1
		resp.Error = fmt.Sprintf("relay command output exceeds %d bytes per stream", maxCommandOutputBytes)
	}
	if receiptPath != "" {
		if err := s.completeRequest(receiptPath, req, resp); err != nil {
			return Response{OK: false, ExitCode: 1, Error: "command effect completed but its durable receipt failed: " + err.Error()}
		}
	}
	return resp
}

type cappedBuffer struct {
	data      []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := maxCommandOutputBytes - len(b.data)
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return original, nil
}

func (b *cappedBuffer) String() string { return string(b.data) }

type commandReceipt struct {
	V          int       `json:"v"`
	RequestID  string    `json:"request_id"`
	SourceID   string    `json:"source_session_id"`
	ArgvDigest string    `json:"argv_digest"`
	State      string    `json:"state"`
	Response   *Response `json:"response,omitempty"`
}

func validRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func requestArgvDigest(argv []string) string {
	raw, _ := json.Marshal(argv)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:])
}

func (s *Server) claimRequest(req Request) (*Response, string, error) {
	if s.ReceiptDir == "" {
		return nil, "", nil
	}
	s.receiptMu.Lock()
	defer s.receiptMu.Unlock()
	if err := os.MkdirAll(s.ReceiptDir, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(s.ReceiptDir, req.RequestID+".json")
	receipt := commandReceipt{V: 1, RequestID: req.RequestID, SourceID: req.Source.SessionID, ArgvDigest: requestArgvDigest(req.Argv), State: "pending"}
	raw, _ := json.Marshal(receipt)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, "", err
		}
		if err := syncDirectory(s.ReceiptDir); err != nil {
			return nil, "", err
		}
		return nil, path, nil
	}
	if !os.IsExist(err) {
		return nil, "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCommandReceiptBytes {
		return nil, "", fmt.Errorf("bridge request receipt is unsafe or exceeds %d bytes", maxCommandReceiptBytes)
	}
	existingRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var existing commandReceipt
	if err := json.Unmarshal(existingRaw, &existing); err != nil || existing.V != 1 || existing.RequestID != req.RequestID || existing.SourceID != req.Source.SessionID || existing.ArgvDigest != receipt.ArgvDigest {
		return nil, "", fmt.Errorf("bridge request receipt conflicts with request %s", req.RequestID)
	}
	if existing.State == "complete" && existing.Response != nil {
		return existing.Response, "", nil
	}
	if existing.State != "pending" {
		return nil, "", fmt.Errorf("invalid bridge request receipt state for %s", req.RequestID)
	}
	return nil, "", fmt.Errorf("bridge request %s is pending from an earlier delivery; effect is intentionally not repeated", req.RequestID)
}

func (s *Server) completeRequest(path string, req Request, response Response) error {
	receipt := commandReceipt{V: 1, RequestID: req.RequestID, SourceID: req.Source.SessionID, ArgvDigest: requestArgvDigest(req.Argv), State: "complete", Response: &response}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.ReceiptDir, ".command-receipt-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncDirectory(s.ReceiptDir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
	// Command authority is evaluated once by Server.AuthorizeRequest after
	// bridge identity authentication. This function is deliberately syntax
	// only; duplicating semantic allowlists here caused authenticated apex and
	// manager repair operations to be rejected before lineage policy ran.
	return nil
}

func writeResponse(conn net.Conn, resp Response) {
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}
