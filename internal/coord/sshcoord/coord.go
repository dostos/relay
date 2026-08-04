// Package sshcoord implements ports.Coord by SSH-execing remote relayd (unix-socket daemon).
package sshcoord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// Coord talks to always-on relayd over SSH.
type Coord struct{}

func New() *Coord { return &Coord{} }

func (c *Coord) Kind() string { return "relayd" }

func (c *Coord) EventsPath(persistName string) string {
	return "~/" + coord.EventsRel + "/" + persistName + ".jsonl"
}

const remoteBin = `"$HOME/.local/bin/relay" service event`

func (c *Coord) Ensure(ctx context.Context, t ports.Transport) error {
	stdout, stderr, err := t.Run(ctx, "", remoteBin+` ping`)
	if err != nil {
		return fmt.Errorf("relayd not available on %s (%v: %s) — run: relay host bootstrap -H %s", t.ID(), err, strings.TrimSpace(stderr), t.ID())
	}
	var resp coord.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil || !resp.OK {
		return fmt.Errorf("relayd ping failed on %s: %s", t.ID(), strings.TrimSpace(stdout+stderr))
	}
	return nil
}

func (c *Coord) Emit(ctx context.Context, t ports.Transport, session, kind string, meta map[string]any) (int64, error) {
	if err := shellquote.ValidateSessionName(session); err != nil {
		return 0, err
	}
	cmd := fmt.Sprintf("%s emit -s %s --kind %s", remoteBin, shellquote.Quote(session), shellquote.Quote(kind))
	if meta != nil {
		b, _ := json.Marshal(meta)
		cmd += " --meta " + shellquote.Quote(string(b))
	}
	stdout, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return 0, fmt.Errorf("relayd emit: %w (%s)", err, strings.TrimSpace(stderr))
	}
	var resp coord.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err == nil && resp.OK {
		return resp.Seq, nil
	}
	return 0, fmt.Errorf("relayd emit: unexpected response %q", strings.TrimSpace(stdout))
}

func (c *Coord) Subscribe(ctx context.Context, t ports.Transport, session string, fromSeq int64, follow bool, w io.Writer) error {
	if err := shellquote.ValidateSessionName(session); err != nil {
		return err
	}
	cmd := fmt.Sprintf("%s subscribe -s %s --from %d", remoteBin, shellquote.Quote(session), fromSeq)
	if follow {
		cmd += " -f"
	}
	return t.RunStream(ctx, "", cmd, w)
}

// SensorCommand returns a remote emit command after validating session and kind.
// stdout/stderr are discarded: tmux run-shell otherwise paints {"ok":true,"seq":N}
// into the live pane (visible in cmux).
func (c *Coord) SensorCommand(session, kind string) (string, error) {
	if err := shellquote.ValidateSessionName(session); err != nil {
		return "", err
	}
	if err := shellquote.ValidateEventKind(kind); err != nil {
		return "", err
	}
	return fmt.Sprintf("$HOME/.local/bin/relay service event emit -s %s --kind %s >/dev/null 2>&1",
		shellquote.Quote(session), shellquote.Quote(kind)), nil
}

// RemoteBuild reports which build of relayd is installed on a host.
//
// Ensure() deliberately stays permissive — a version mismatch is not a reason
// to refuse work mid-flight — so drift needs somewhere else to become visible.
// This is that somewhere: `relay doctor -H HOST` compares it against the local
// build and says so when a host is running code from an older install.
func (c *Coord) RemoteBuild(ctx context.Context, t ports.Transport) (string, error) {
	stdout, stderr, err := t.Run(ctx, "", remoteBin+` ping`)
	if err != nil {
		return "", fmt.Errorf("relayd ping on %s: %w (%s)", t.ID(), err, strings.TrimSpace(stderr))
	}
	var resp coord.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil {
		return "", fmt.Errorf("relayd ping on %s: %w", t.ID(), err)
	}
	if resp.Build == "" {
		// Predates build stamping, which is itself the answer: this relayd was
		// installed before the stamp existed, so it is certainly not current.
		return "unstamped", nil
	}
	return resp.Build, nil
}
