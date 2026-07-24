// Package ports defines pluggable adapter interfaces for the relay control plane.
// Defaults: Transport=SSH, Persistence=tmux, Visualization=cmux — none are hardwired into core.
package ports

import (
	"context"
	"io"
)

// Transport reaches a remote host. SSH is the default implementation.
type Transport interface {
	// ID is a stable host identifier (e.g. ssh config Host alias).
	ID() string
	// Run executes command on the remote host (non-interactive). cwd may be empty.
	Run(ctx context.Context, cwd, command string) (stdout, stderr string, err error)
	// RunStream runs command and streams combined stdout/stderr to w until exit or ctx cancel.
	RunStream(ctx context.Context, cwd, command string, w io.Writer) error
	// ReadFile reads a remote path (absolute or ~/…).
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes a remote path (creates parent dirs best-effort).
	WriteFile(ctx context.Context, path string, data []byte, mode string) error
	// Interactive opens an interactive session running command (e.g. attach). Blocks until exit.
	Interactive(ctx context.Context, command string) error
}

// PersistHandle identifies a durable session on a Persistence backend.
type PersistHandle struct {
	Kind string `json:"kind"` // e.g. "tmux"
	Name string `json:"name"` // e.g. tmux session name
}

// Persistence keeps processes alive across transport drops. tmux is the default.
type Persistence interface {
	Kind() string
	Create(ctx context.Context, t Transport, name, cwd, command string) (PersistHandle, error)
	Exists(ctx context.Context, t Transport, h PersistHandle) (bool, error)
	Destroy(ctx context.Context, t Transport, h PersistHandle) error
	Capture(ctx context.Context, t Transport, h PersistHandle, lines int) (string, error)
	Send(ctx context.Context, t Transport, h PersistHandle, text string, enter bool) error
	Resize(ctx context.Context, t Transport, h PersistHandle) error
	// AttachCommand returns a shell command suitable for interactive attach over Transport.
	AttachCommand(h PersistHandle, cwd string) string
	// DeadStatus reports whether the primary pane/process has exited and its exit code if known.
	DeadStatus(ctx context.Context, t Transport, h PersistHandle) (dead bool, code int, err error)
	// InstallEvents wires idle/exit event emission into remote JSONL for this session.
	InstallEvents(ctx context.Context, t Transport, h PersistHandle, silenceSec int) error
	// EventsPath is the remote path of the JSONL event log for this handle.
	EventsPath(h PersistHandle) string
}

// Layout describes how to present a session in a visual surface.
type Layout struct {
	Mode string // "pair" | "remote" | "none"
}

// Viz presents sessions to a human. cmux is the default; may be a no-op.
type Viz interface {
	Kind() string
	Available(ctx context.Context) bool
	Present(ctx context.Context, sessionID, attachCmd string, layout Layout) (surfaceRef string, err error)
	Focus(ctx context.Context, sessionID string) error
	Close(ctx context.Context, sessionID string) error
	Layout(ctx context.Context) (string, error)
}

// Coord is the remote event/coordination bus (default: always-on relayd over SSH).
// Must not introduce TCP listeners or reconnect storms — see IT-safety rules.
type Coord interface {
	Kind() string
	// Ensure checks that the remote coordinator is reachable; returns a clear error if not.
	Ensure(ctx context.Context, t Transport) error
	Emit(ctx context.Context, t Transport, session, kind string, meta map[string]any) (seq int64, err error)
	// Subscribe streams events with seq > fromSeq. Heartbeats use kind=heartbeat.
	// follow=false: replay then exit. follow=true: one long-lived stream.
	Subscribe(ctx context.Context, t Transport, session string, fromSeq int64, follow bool, w io.Writer) error
	// EventsPath is the remote log path for a persist session name (for bindings/docs).
	EventsPath(persistName string) string
}
