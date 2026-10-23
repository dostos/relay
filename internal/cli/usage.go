package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/shellquote"
)

// Usage log (#75). Every messaging outcome (send, submit, wait, op) is
// appended as one JSON line to $STATE/usage/YYYY-MM-DD.jsonl on the machine
// that executed it, so `relay report` can aggregate patterns. It holds
// receipts and errors only, never message text. RELAY_USAGE_LOG=0 disables.

type usageEvent struct {
	At         time.Time `json:"at"`
	Cmd        string    `json:"cmd"`
	Target     string    `json:"target,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	State      string    `json:"state,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	Exit       int       `json:"exit"`
	Error      string    `json:"error,omitempty"`
	Replayed   bool      `json:"replayed,omitempty"`
	Keyed      bool      `json:"keyed,omitempty"`
	Direct     bool      `json:"direct,omitempty"`
	Bridged    bool      `json:"bridged,omitempty"`
	Source     string    `json:"source,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Build      string    `json:"build"`
}

func usageDir() string { return filepath.Join(core.StateRoot(), "usage") }

func (a *App) recordUsage(ev usageEvent) {
	if os.Getenv("RELAY_USAGE_LOG") == "0" || a.noUsage {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	ev.Build = coord.Build
	ev.Direct = core.DirectMode
	ev.Bridged = os.Getenv("RELAY_BRIDGE_LOCAL_INVOKE") == "1"
	if src := strings.TrimSpace(os.Getenv("RELAY_SOURCE_SESSION_ID")); src != "" {
		ev.Source = src
	}
	if len(ev.Error) > 300 {
		ev.Error = ev.Error[:300]
	}
	if err := os.MkdirAll(usageDir(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(usageDir(), ev.At.Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// targetOf names a session as HOST/NAME for the usage log.
func (a *App) targetOf(id string) string {
	if a.Sessions == nil {
		return ""
	}
	if s, err := a.Sessions.Get(id); err == nil {
		return s.HostID + "/" + s.Persist.Name
	}
	return ""
}

var stderrWriter io.Writer = os.Stderr

func stderr() io.Writer { return stderrWriter }

func joinArgv(argv []string) string {
	out := make([]string, len(argv))
	for i, v := range argv {
		if v != "" && strings.IndexFunc(v, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '/' || r == '.' || r == ':' || r == '=' || r == ',' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) < 0 {
			out[i] = v
		} else {
			out[i] = shellquote.Quote(v)
		}
	}
	return strings.Join(out, " ")
}
