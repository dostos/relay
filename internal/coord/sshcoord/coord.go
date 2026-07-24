// Package sshcoord implements ports.Coord by SSH-execing remote relayd (unix-socket daemon).
// IT safety: one long-lived SSH stream per Subscribe; no TCP listeners; no reconnect storms here.
package sshcoord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// Coord talks to always-on relayd over SSH.
type Coord struct{}

func New() *Coord { return &Coord{} }

func (c *Coord) Kind() string { return "relayd" }

func (c *Coord) EventsPath(persistName string) string {
	return "~/" + coord.EventsRel + "/" + persistName + ".jsonl"
}

// remoteBin is PATH-independent (non-interactive ssh often lacks ~/.local/bin).
const remoteBin = `"$HOME/.local/bin/relayd"`

func (c *Coord) Ensure(ctx context.Context, t ports.Transport) error {
	stdout, stderr, err := t.Run(ctx, "", remoteBin+` ping`)
	if err != nil {
		return fmt.Errorf("relayd not available on %s (%v: %s) — run: relay host bootstrap -H %s", t.ID(), err, strings.TrimSpace(stderr), t.ID())
	}
	var resp coord.Response
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp) == nil && resp.OK {
		return nil
	}
	// ping CLI may print plain text
	if strings.Contains(stdout, "ok") || strings.Contains(stdout, coord.Version) || strings.TrimSpace(stdout) != "" {
		return nil
	}
	return fmt.Errorf("relayd ping failed on %s: %s", t.ID(), strings.TrimSpace(stdout+stderr))
}

func (c *Coord) Emit(ctx context.Context, t ports.Transport, session, kind string, meta map[string]any) (int64, error) {
	cmd := fmt.Sprintf("%s emit -s %s --kind %s", remoteBin, shellQuote(session), shellQuote(kind))
	if meta != nil {
		b, _ := json.Marshal(meta)
		cmd += " --meta " + shellQuote(string(b))
	}
	stdout, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return 0, fmt.Errorf("relayd emit: %w (%s)", err, strings.TrimSpace(stderr))
	}
	var resp coord.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err == nil && resp.OK {
		return resp.Seq, nil
	}
	// tolerate "seq=N" plain output
	for _, f := range strings.Fields(stdout) {
		if strings.HasPrefix(f, "seq=") {
			n, _ := strconv.ParseInt(strings.TrimPrefix(f, "seq="), 10, 64)
			return n, nil
		}
	}
	return 0, nil
}

func (c *Coord) Subscribe(ctx context.Context, t ports.Transport, session string, fromSeq int64, follow bool, w io.Writer) error {
	cmd := fmt.Sprintf("%s subscribe -s %s --from %d", remoteBin, shellQuote(session), fromSeq)
	if follow {
		cmd += " -f"
	}
	return t.RunStream(ctx, "", cmd, w)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
