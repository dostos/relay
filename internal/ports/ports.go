// Package ports defines pluggable adapter interfaces for the relay control plane.
// Defaults: Transport=SSH, Persistence=tmux, Visualization=cmux, Coord=relayd.
package ports

import (
	"context"
	"io"
)

// Transport reaches a remote host. SSH is the default implementation.
type Transport interface {
	ID() string
	Run(ctx context.Context, cwd, command string) (stdout, stderr string, err error)
	RunStream(ctx context.Context, cwd, command string, w io.Writer) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode string) error
	Interactive(ctx context.Context, command string) error
	// InteractiveCommand returns a local shell command that opens an interactive
	// session running remoteCmd (for Viz present). Default SSH: "ssh -t HOST -- …".
	InteractiveCommand(remoteCmd string) string
}

// PersistHandle identifies a durable session on a Persistence backend.
type PersistHandle struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
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
	AttachCommand(h PersistHandle, cwd string) string
	DeadStatus(ctx context.Context, t Transport, h PersistHandle) (dead bool, code int, err error)
	// InstallSensors wires idle/exit detection. emitCmd(kind) returns a remote
	// shell command supplied by Coord (e.g. relayd emit) — Persistence must not
	// hard-code a Coord implementation.
	InstallSensors(ctx context.Context, t Transport, h PersistHandle, silenceSec int, emitCmd func(kind string) (string, error)) error
}

// Layout describes how to present a session in a visual surface.
type Layout struct {
	Mode      string // "pair" | "remote" | "none"
	Workspace string // optional cmux workspace ref (e.g. workspace:2)
	Pane      string // optional cmux pane ref — split relative to this pane
	Tab       bool   // if true with Pane set, stack as a tab; default is side-by-side split
}

// Viz presents sessions to a human. cmux is the default; may be a no-op.
type Viz interface {
	Kind() string
	Available(ctx context.Context) bool
	Present(ctx context.Context, sessionID, attachCmd string, layout Layout) (surfaceRef string, err error)
	Focus(ctx context.Context, sessionID string) error
	Close(ctx context.Context, sessionID string) error
	Layout(ctx context.Context) (string, error)
	// SaveRestorable snapshots live panes for restart restore (cmux Vault / manual).
	// No-op adapters return (0, nil).
	SaveRestorable(ctx context.Context) (saved int, err error)
	// RestoreSaved re-attaches saved panes after cmux restart (manual path).
	RestoreSaved(ctx context.Context) (restored int, err error)
	// BrandLabels refreshes ◆ RELAY · <project> tab titles + workspace pills.
	// labels maps session_id → project display name.
	BrandLabels(ctx context.Context, labels map[string]string) error
}

// Coord is the remote event/coordination bus (default: always-on relayd over SSH).
type Coord interface {
	Kind() string
	Ensure(ctx context.Context, t Transport) error
	Emit(ctx context.Context, t Transport, session, kind string, meta map[string]any) (seq int64, err error)
	Subscribe(ctx context.Context, t Transport, session string, fromSeq int64, follow bool, w io.Writer) error
	EventsPath(persistName string) string
	// SensorCommand returns a remote shell command that emits kind for session
	// (used by Persistence sensors). Validates session and kind defensively.
	SensorCommand(session, kind string) (string, error)
}
