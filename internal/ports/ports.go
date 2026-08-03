// Package ports defines pluggable adapter interfaces for the relay control plane.
// Defaults: Transport=SSH, Persistence=tmux, Visualization=cmux, Coord=relayd.
package ports

import (
	"context"
	"io"
	"time"
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

// HoldingShellLauncher is the narrow capability used to leave Relay's
// freshly-created holding shell. Its acknowledgement proves only that the
// shell evaluated the launch line; interactive message delivery is separate.
type HoldingShellLauncher interface {
	Launch(ctx context.Context, t Transport, h PersistHandle, command string) error
}

// SessionChrome is an optional persistence capability for applying durable
// visual ownership cues. SessionService invokes it on create, adopt, and
// named-session reuse so callers below the CLI layer receive the same chrome.
type SessionChrome interface {
	ApplyChrome(ctx context.Context, t Transport, h PersistHandle) error
}

// Layout describes how to present a session in a visual surface.
type Layout struct {
	Mode            string // "pair" | "remote" | "none"
	Workspace       string // optional cmux workspace ref (e.g. workspace:2)
	Pane            string // optional cmux pane ref — split relative to this pane
	Tab             bool   // if true with Pane set, stack as a tab; default is side-by-side split
	SourceSessionID string // lineage owner whose pane anchors this placement
	SplitDirection  string // right for the first child; down for later siblings
	ExplicitPlace   bool   // explicit workspace/pane flags disable sibling stacking
}

// Presentation identifies what an optional visualization service should
// display. Geometry is deliberately absent: it belongs to user policy on the
// visualization host.
type Presentation struct {
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	Target          string `json:"target"`
	TmuxName        string `json:"tmux_name"`
	SSHHost         string `json:"ssh_host,omitempty"`
	SSHUser         string `json:"ssh_user,omitempty"`
	SSHPort         int    `json:"ssh_port,omitempty"`
}

type ProjectionOp string

const (
	ProjectionUpsert ProjectionOp = "upsert"
	ProjectionDelete ProjectionOp = "delete"
	ProjectionFocus  ProjectionOp = "focus"
)

// ProjectionEvent is display-only state emitted by the authoritative host.
// Revision is the durable visualization-stream sequence, not authority data.
type ProjectionEvent struct {
	V        int          `json:"v"`
	Revision int64        `json:"stream_revision"`
	Op       ProjectionOp `json:"op"`
	Item     Presentation `json:"item"`
}

type ProjectionSink interface {
	ApplyProjection(context.Context, ProjectionEvent) (surfaceRef string, err error)
}

// ProjectedSession is the authority-owned identity and lineage joined to a
// visualization host's local surface. It is a read model, never authority.
type ProjectedSession struct {
	SessionID       string
	ParentSessionID string
	Target          string
	TmuxName        string
	Surface         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ProjectionInventory is an optional Viz capability used on projection-only
// hosts. Implementations must fail when current authority metadata is absent.
type ProjectionInventory interface {
	ProjectionSessions(context.Context) ([]ProjectedSession, error)
}

// ResumeTarget joins authority-resolved routing to an optional client-local
// identity. Private key paths never cross the projection protocol.
type ResumeTarget struct {
	Host     string `json:"host"`
	User     string `json:"user,omitempty"`
	Port     int    `json:"port,omitempty"`
	Identity string `json:"-"`
}

type ResumeResolver interface {
	ResolveProjectedResume(context.Context, string, ResumeResolveOpts) (ResumeTarget, error)
}

type ResumeResolveOpts struct {
	AllowOffline bool
}

// ResumeResolution is the broker-safe authority response. It carries public
// connection coordinates, never credentials or key paths.
type ResumeResolution struct {
	SessionID string `json:"session_id"`
	Target    string `json:"target"`
	TmuxName  string `json:"tmux_name"`
	SSHHost   string `json:"ssh_host"`
	SSHUser   string `json:"ssh_user,omitempty"`
	SSHPort   int    `json:"ssh_port,omitempty"`
}

type AuthoritySnapshot struct {
	V        int            `json:"v"`
	Revision int64          `json:"revision"`
	Items    []Presentation `json:"items"`
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
	// BrandLabels refreshes ◆ RELAY · <project> tab titles + workspace status
	// pills (not workspace descriptions). labels maps session_id → project name.
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
