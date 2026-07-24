// Package tmux implements ports.Persistence using remote tmux.
package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/dostos/relay/internal/ports"
)

const kind = "tmux"

// Persist is a tmux-backed persistence adapter.
type Persist struct{}

func New() *Persist { return &Persist{} }

func (p *Persist) Kind() string { return kind }

func (p *Persist) Create(ctx context.Context, t ports.Transport, name, cwd, command string) (ports.PersistHandle, error) {
	h := ports.PersistHandle{Kind: kind, Name: name}
	exists, err := p.Exists(ctx, t, h)
	if err != nil {
		return h, err
	}
	if exists {
		return h, nil
	}
	inner := command
	if inner == "" {
		inner = "exec bash -l"
	}
	startDir := `"$HOME"`
	if cwd != "" {
		startDir = remotePathExpr(cwd)
	}
	// Create detached session; working directory best-effort via -c.
	remote := fmt.Sprintf(
		`mkdir -p %s 2>/dev/null || true; tmux has-session -t %s 2>/dev/null || tmux new-session -d -s %s -c %s -- bash -lc %s`,
		startDir,
		shellQuote(name), shellQuote(name),
		startDir,
		shellQuote(inner),
	)
	_, stderr, err := t.Run(ctx, "", remote)
	if err != nil {
		return h, fmt.Errorf("tmux create: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return h, nil
}

func (p *Persist) Exists(ctx context.Context, t ports.Transport, h ports.PersistHandle) (bool, error) {
	_, _, err := t.Run(ctx, "", fmt.Sprintf("tmux has-session -t %s", shellQuote(h.Name)))
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (p *Persist) Destroy(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	_, _, err := t.Run(ctx, "", fmt.Sprintf("tmux kill-session -t %s 2>/dev/null || true", shellQuote(h.Name)))
	return err
}

func (p *Persist) Capture(ctx context.Context, t ports.Transport, h ports.PersistHandle, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -%d", shellQuote(h.Name), lines)
	stdout, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return "", fmt.Errorf("capture: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

func (p *Persist) Send(ctx context.Context, t ports.Transport, h ports.PersistHandle, text string, enter bool) error {
	// Use tmux send-keys -l for literal text.
	cmd := fmt.Sprintf("tmux send-keys -t %s -l -- %s", shellQuote(h.Name), shellQuote(text))
	if enter {
		cmd += fmt.Sprintf("; sleep 0.15; tmux send-keys -t %s Enter", shellQuote(h.Name))
	}
	_, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return fmt.Errorf("send: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

func (p *Persist) Resize(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	// Resync pty winsize to tmux pane size (fixes garbled TUI).
	script := fmt.Sprintf(`
pane=$(tmux display-message -p -t %s '#{pane_tty}')
w=$(tmux display-message -p -t %s '#{pane_width}')
h=$(tmux display-message -p -t %s '#{pane_height}')
stty -F "$pane" cols "$w" rows "$h" 2>/dev/null || stty <"$pane" cols "$w" rows "$h" 2>/dev/null || true
tmux send-keys -t %s C-l
`, shellQuote(h.Name), shellQuote(h.Name), shellQuote(h.Name), shellQuote(h.Name))
	_, _, err := t.Run(ctx, "", script)
	return err
}

func (p *Persist) AttachCommand(h ports.PersistHandle, cwd string) string {
	_ = cwd
	return fmt.Sprintf("tmux new-session -A -s %s", shellQuote(h.Name))
}

func (p *Persist) DeadStatus(ctx context.Context, t ports.Transport, h ports.PersistHandle) (bool, int, error) {
	exists, err := p.Exists(ctx, t, h)
	if err != nil {
		return false, 0, err
	}
	if !exists {
		return true, 0, nil
	}
	stdout, _, err := t.Run(ctx, "", fmt.Sprintf(
		`tmux list-panes -t %s -F '#{pane_dead} #{pane_dead_status}' | head -n1`,
		shellQuote(h.Name),
	))
	if err != nil {
		return false, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) == 0 {
		return false, 0, nil
	}
	dead := fields[0] == "1"
	code := 0
	if len(fields) > 1 {
		code, _ = strconv.Atoi(fields[1])
	}
	return dead, code, nil
}

func (p *Persist) EventsPath(h ports.PersistHandle) string {
	return "~/.local/state/relay/events/" + h.Name + ".jsonl"
}

// InstallEvents wires thin tmux sensors that call host-local `relayd emit` (Unix IPC only).
// relayd must already be running (relay host bootstrap). No outbound network from hooks.
func (p *Persist) InstallEvents(ctx context.Context, t ports.Transport, h ports.PersistHandle, silenceSec int) error {
	if silenceSec <= 0 {
		silenceSec = 45
	}
	hooks := fmt.Sprintf(`
RELAYD="$HOME/.local/bin/relayd"
test -x "$RELAYD" || { echo "relayd missing — run: relay host bootstrap" >&2; exit 1; }
SESS=%s
# silence-action any is required for sole-window idle detection
tmux set-option -t "$SESS" monitor-silence %d
tmux set-option -t "$SESS" silence-action any
tmux set-hook -t "$SESS" pane-died "run-shell -b '$RELAYD emit -s %s --kind exit'"
tmux set-hook -t "$SESS" alert-silence "run-shell -b '$RELAYD emit -s %s --kind idle'"
tmux set-option -t "$SESS" remain-on-exit on
`, shellQuote(h.Name), silenceSec, h.Name, h.Name)
	_, stderr, err := t.Run(ctx, "", hooks)
	if err != nil {
		return fmt.Errorf("install sensors: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

func remotePathExpr(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		rest := strings.ReplaceAll(p[2:], `"`, `\"`)
		return `"$HOME/` + rest + `"`
	}
	return shellQuote(p)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
