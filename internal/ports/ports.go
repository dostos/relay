// Package ports defines pluggable adapter interfaces for the relay control plane.
// Defaults: Transport=SSH, Persistence=tmux, Coord=relayd. Presentation is
// not a port: Forge (or any terminal) runs `relay resume` in a tab.
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
}

// DiagnosticSource is an optional capability for transports and their
// adapters. It exposes the most useful recent network diagnostic for a
// reconnect UI without making callers parse transport-specific stderr.
// Implementations should return an empty string when no diagnostic is known.
type DiagnosticSource interface {
	LastDiagnostic() string
}

// ReverseUnixForwarder is an optional transport capability used by interactive
// relay panes. It maps a remote Unix socket back to the desktop bridge for the
// lifetime of the persistent attach connection.
type ReverseUnixForwarder interface {
	SetReverseUnixForward(remoteSocket, localSocket string)
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
	Rename(ctx context.Context, t Transport, from, to PersistHandle) error
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

// DeliveryUncertainError marks an adapter error after a mutating input command
// was attempted. Callers must preserve the envelope and must not retry it
// automatically because the target may already have received the message.
type DeliveryUncertainError struct{ Err error }

func (e *DeliveryUncertainError) Error() string { return e.Err.Error() }
func (e *DeliveryUncertainError) Unwrap() error { return e.Err }

// TargetUnavailableError means the adapter conclusively found that the target
// cannot receive input (for example, its pane or remote host is gone). Only
// this pre-mutation outcome may authorize hierarchy failover.
type TargetUnavailableError struct{ Err error }

func (e *TargetUnavailableError) Error() string { return e.Err.Error() }
func (e *TargetUnavailableError) Unwrap() error { return e.Err }

// HoldingShellLauncher is the narrow capability used to leave Relay's
// freshly-created holding shell. Its acknowledgement proves only that the
// shell evaluated the launch line; interactive message delivery is separate.
type HoldingShellLauncher interface {
	Launch(ctx context.Context, t Transport, h PersistHandle, command string) error
}

// GateChoiceResolver applies one explicit human-selected menu index to the
// currently visible interactive gate. It must never choose an index itself.
type GateChoiceResolver interface {
	ResolveGateChoice(ctx context.Context, t Transport, h PersistHandle, selectedOffset int) error
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
