// Package tmux implements ports.Persistence using remote tmux.
package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

const kind = "tmux"

const (
	sendConfirmAttempts = 3
	sendConfirmLines    = 14
)

var sendConfirmDelay = 600 * time.Millisecond

// Persist is a tmux-backed persistence adapter.
type Persist struct{}

func New() *Persist { return &Persist{} }

func (p *Persist) Kind() string { return kind }

// tmux otherwise accepts a unique prefix, which can make a retired "apex"
// target the live "apex-v2" session. A leading '=' requires an exact match.
func exactTarget(name string) string { return "=" + name }

func (p *Persist) Create(ctx context.Context, t ports.Transport, name, cwd, command string) (ports.PersistHandle, error) {
	if err := shellquote.ValidateSessionName(name); err != nil {
		return ports.PersistHandle{}, err
	}
	h := ports.PersistHandle{Kind: kind, Name: name}
	exists, err := p.Exists(ctx, t, h)
	if err != nil {
		return h, err
	}
	if exists {
		return h, fmt.Errorf("tmux session %q already exists (pick another --name)", name)
	}
	inner := command
	if inner == "" {
		inner = "exec bash -l"
	}
	startDir := `"$HOME"`
	if cwd != "" {
		expr, err := shellquote.PathExpr(cwd)
		if err != nil {
			return h, err
		}
		startDir = expr
	}
	remote := fmt.Sprintf(
		`mkdir -p %s 2>/dev/null || true; tmux new-session -d -s %s -c %s -- bash -lc %s`,
		startDir,
		shellquote.Quote(name),
		startDir,
		shellquote.Quote(inner),
	)
	_, stderr, err := t.Run(ctx, "", remote)
	if err != nil {
		return h, fmt.Errorf("tmux create: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return h, nil
}

// Rename changes the durable tmux identity without disturbing the processes
// running inside the session.
func (p *Persist) Rename(ctx context.Context, t ports.Transport, from, to ports.PersistHandle) error {
	if err := shellquote.ValidateSessionName(from.Name); err != nil {
		return err
	}
	if err := shellquote.ValidateSessionName(to.Name); err != nil {
		return err
	}
	if from.Name == to.Name {
		return nil
	}
	if exists, err := p.Exists(ctx, t, to); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("tmux session %q already exists", to.Name)
	}
	_, stderr, err := t.Run(ctx, "", fmt.Sprintf(
		"tmux rename-session -t %s %s",
		shellquote.Quote(exactTarget(from.Name)), shellquote.Quote(to.Name),
	))
	if err != nil {
		return fmt.Errorf("tmux rename %q to %q: %w (%s)", from.Name, to.Name, err, strings.TrimSpace(stderr))
	}
	return nil
}

func (p *Persist) Exists(ctx context.Context, t ports.Transport, h ports.PersistHandle) (bool, error) {
	// tmux target lookup accepts unique prefixes, but Relay session names are
	// identities. Compare the listed name exactly so renaming
	// "engram-apps-..." to "engram" does not mistake the source for a
	// conflicting destination.
	_, _, err := t.Run(ctx, "", fmt.Sprintf(
		"tmux list-sessions -F '#{session_name}' 2>/dev/null | grep -Fqx -- %s",
		shellquote.Quote(h.Name),
	))
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (p *Persist) Destroy(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	_, _, err := t.Run(ctx, "", fmt.Sprintf("tmux kill-session -t %s 2>/dev/null || true", shellquote.Quote(exactTarget(h.Name))))
	return err
}

func (p *Persist) Capture(ctx context.Context, t ports.Transport, h ports.PersistHandle, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -%d", shellquote.Quote(exactTarget(h.Name)), lines)
	stdout, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return "", fmt.Errorf("capture: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

func (p *Persist) Send(ctx context.Context, t ports.Transport, h ports.PersistHandle, text string, enter bool) error {
	cmd := fmt.Sprintf("tmux send-keys -t %s -l -- %s", shellquote.Quote(exactTarget(h.Name)), shellquote.Quote(text))
	_, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return fmt.Errorf("send: %w (%s)", err, strings.TrimSpace(stderr))
	}
	if !enter {
		return nil
	}
	marker := text
	if len(marker) > 48 {
		marker = marker[:48]
	}
	for attempt := 0; attempt < sendConfirmAttempts; attempt++ {
		_, stderr, err = t.Run(ctx, "", fmt.Sprintf("tmux send-keys -t %s Enter", shellquote.Quote(exactTarget(h.Name))))
		if err != nil {
			return fmt.Errorf("submit: %w (%s)", err, strings.TrimSpace(stderr))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sendConfirmDelay):
		}
		screen, captureErr := p.Capture(ctx, t, h, sendConfirmLines)
		if captureErr != nil {
			return fmt.Errorf("confirm send: %w", captureErr)
		}
		if !composerHolds(screen, marker) {
			return nil
		}
	}
	return fmt.Errorf("message is still unsent in %s's composer after %d attempts", h.Name, sendConfirmAttempts)
}

func composerHolds(screen, marker string) bool {
	if marker == "" {
		return false
	}
	lines := strings.Split(screen, "\n")
	composer, composerLine := "", -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, ">") || strings.HasPrefix(trimmed, "❯") {
			composer = trimmed
			composerLine = i
		}
	}
	if !strings.Contains(composer, marker) {
		return false
	}
	for _, line := range lines[composerLine+1:] {
		if strings.TrimSpace(line) != "" && len(line) == len(strings.TrimLeft(line, " \t")) {
			return false
		}
	}
	return true
}

func (p *Persist) Resize(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	q := shellquote.Quote(exactTarget(h.Name))
	script := fmt.Sprintf(`
pane=$(tmux display-message -p -t %s '#{pane_tty}')
w=$(tmux display-message -p -t %s '#{pane_width}')
h=$(tmux display-message -p -t %s '#{pane_height}')
stty -F "$pane" cols "$w" rows "$h" 2>/dev/null || stty <"$pane" cols "$w" rows "$h" 2>/dev/null || true
tmux send-keys -t %s C-l
`, q, q, q, q)
	_, _, err := t.Run(ctx, "", script)
	return err
}

func (p *Persist) AttachCommand(h ports.PersistHandle, cwd string) string {
	_ = cwd
	return fmt.Sprintf("tmux new-session -A -s %s", shellquote.Quote(h.Name))
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
		shellquote.Quote(exactTarget(h.Name)),
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

// InstallSensors wires idle/exit detection. emitCmd is supplied by Coord (no hard-coded relayd).
func (p *Persist) InstallSensors(ctx context.Context, t ports.Transport, h ports.PersistHandle, silenceSec int, emitCmd func(kind string) (string, error)) error {
	if silenceSec <= 0 {
		silenceSec = 10
	}
	if err := shellquote.ValidateSessionName(h.Name); err != nil {
		return err
	}
	if emitCmd == nil {
		return fmt.Errorf("emitCmd required")
	}
	// emitCmd returns a remote shell command; session/kind are validated+quoted by Coord.
	exitCmd, err := emitCmd("exit")
	if err != nil {
		return err
	}
	idleCmd, err := emitCmd("idle")
	if err != nil {
		return err
	}
	hooks := fmt.Sprintf(`
SESS=%s
tmux set-option -t "$SESS" monitor-silence %d
tmux set-option -t "$SESS" silence-action any
tmux set-hook -t "$SESS" pane-died "run-shell -b %s"
tmux set-hook -t "$SESS" alert-silence "run-shell -b %s"
tmux set-option -t "$SESS" remain-on-exit on
`, shellquote.Quote(exactTarget(h.Name)), silenceSec,
		shellquote.Quote(exitCmd),
		shellquote.Quote(idleCmd),
	)
	_, stderr, err := t.Run(ctx, "", hooks)
	if err != nil {
		return fmt.Errorf("install sensors: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}
