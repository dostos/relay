// Package ports defines pluggable adapter interfaces for the relay control plane.
// Defaults: Transport=SSH, Persistence=tmux, Coord=relayd. Presentation is
// not a port: Forge (or any terminal) runs `relay resume` in a tab.
package ports

import (
	"context"
	"io"
)

// HoldingShell is the shell a durable session hands its operator when no
// runtime command is given (and after a lost session is recreated). It is
// exec'd from inside `bash -lc`, which has already sourced the login profile,
// so the environment and PATH are the login ones.
//
// It is deliberately not another login shell, and it drops TMOUT: lab hosts
// set TMOUT in /etc/profile (hamburg: TMOUT=600), a login shell re-reads it,
// and the export reaches every shell in the pane -- including the zsh that
// ~/.bashrc execs. That shell then logs itself out after ten idle minutes at a
// prompt, which ends the pane and with it the session. hamburg/oqb was lost
// that way on 2026-10-15, ten minutes after its agent left the shell idle.
const HoldingShell = "env -u TMOUT bash -i"

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
	// Send types text into the session's active pane and, when enter is
	// set, submits it exactly once. The receipt says how far delivery
	// provably got; a non-nil *DeliveryUncertainError accompanies every
	// receipt whose state is not SendSubmitted (or SendDeposited for
	// enter=false).
	Send(ctx context.Context, t Transport, h PersistHandle, text string, enter bool) (SendReceipt, error)
	Resize(ctx context.Context, t Transport, h PersistHandle) error
	AttachCommand(h PersistHandle, cwd string) string
	DeadStatus(ctx context.Context, t Transport, h PersistHandle) (dead bool, code int, err error)
	// InstallSensors wires idle/exit detection. emitCmd(kind) returns a remote
	// shell command supplied by Coord (e.g. relayd emit) — Persistence must not
	// hard-code a Coord implementation.
	InstallSensors(ctx context.Context, t Transport, h PersistHandle, silenceSec int, emitCmd func(kind string) (string, error)) error
}

// ExitSessionAbsent is the exit status of `relay resume --no-create` (and of
// the remote attach command behind it) when the durable session does not
// exist. It is distinct from 0 (clean detach), 1 (error), 2 (usage), 130
// (SIGINT) and 255 (ssh failure), so a presenter can tell "the work is gone"
// from "the network dropped".
const ExitSessionAbsent = 66

// ExitDeliveryUncertain is the exit status of `relay session send` when the
// text was (or may have been) typed but no fresh turn attributable to this
// send was observed. Callers must not resend blindly: inspect the session,
// or retry with the same --idempotency-key, which never types twice.
const ExitDeliveryUncertain = 67

// SendState is how far a session send provably got.
type SendState string

const (
	// SendNotSent: Relay failed before typing anything. Safe to retry.
	SendNotSent SendState = "not_sent"
	// SendDeposited: the text reached the pane's input (enter=false), or it
	// is visibly still sitting in the composer after the single submit.
	SendDeposited SendState = "deposited"
	// SendSubmitted: a turn that started after this send was observed.
	SendSubmitted SendState = "submitted"
	// SendUncertain: input was attempted but neither a fresh turn nor a
	// held composer could be proven (transport loss, unknown TUI, ...).
	SendUncertain SendState = "uncertain"
	// SendQueued: the target was mid-turn; its composer accepted the text
	// (it left the input) while the earlier turn kept running, so the agent
	// will read it when that turn ends. Success, but not a fresh turn (#71).
	SendQueued SendState = "queued"
)

// SendAccepted reports whether a receipt state means the message is in the
// agent's hands (exit 0): a fresh turn, or queued behind a running one.
func SendAccepted(s SendState) bool { return s == SendSubmitted || s == SendQueued }

// ExitTargetBlocked is the exit status of `relay session wait` when the
// target stopped at an approval/login/trust gate that only a human answers.
const ExitTargetBlocked = 65

// ExitWaitTimeout is the exit status of a bounded wait that ran out of time
// (same as timeout(1)); the JSON output carries the argv to resume.
const ExitWaitTimeout = 124

// SendReceipt is the structured outcome of one Persistence.Send.
type SendReceipt struct {
	State SendState `json:"state"`
	// Evidence names the observation that decided State, e.g.
	// "fresh_working_indicator", "working_timer_reset", "composer_released",
	// "composer_holds_text", "no_fresh_turn".
	Evidence string `json:"evidence,omitempty"`
	// Submits is how many Enter keys were sent (0 or 1; never repeated).
	Submits int `json:"submits"`
	// Polls is how many post-submit pane reads were taken.
	Polls int `json:"polls,omitempty"`
	// Leftover is the unrelated text already in the target's composer when
	// a send was refused with evidence composer_not_empty (#70).
	Leftover string `json:"leftover,omitempty"`
}

// ComposerReader is an optional Persistence capability: the text currently
// held in the pane's input composer (placeholder hints excluded). found is
// false when no composer is recognizable.
type ComposerReader interface {
	ComposerDraft(ctx context.Context, t Transport, h PersistHandle) (draft string, found bool, err error)
}

// ExistingAttacher is an optional Persistence capability: an attach command
// that never creates. When the session is missing it prints a line to stderr
// and exits ExitSessionAbsent.
type ExistingAttacher interface {
	AttachExistingCommand(h PersistHandle) string
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
