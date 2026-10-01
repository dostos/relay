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
// target the live "apex-v2" session. A leading '=' requires an exact session
// match. Commands whose grammar expects a pane need the trailing ':' so tmux
// resolves the active pane within that exact session.
func exactSession(name string) string      { return "=" + name }
func exactSessionScope(name string) string { return "=" + name + ":" }
func exactPane(name string) string         { return exactSessionScope(name) }

// resolvePaneID returns the live tmux pane id (%N) for a session's active
// pane. send-keys against a bare exact-name session target ("=name") fails
// with "can't find pane" on current tmux; even "=name:" can miss when the
// caller retries across renames. An explicit pane id is the durable target.
func (p *Persist) resolvePaneID(ctx context.Context, t ports.Transport, h ports.PersistHandle) (string, error) {
	out, stderr, err := t.Run(ctx, "", fmt.Sprintf(
		"tmux display-message -p -t %s '#{pane_id}'",
		shellquote.Quote(exactPane(h.Name)),
	))
	if err != nil {
		return "", fmt.Errorf("resolve pane id for %s: %w (%s)", h.Name, err, strings.TrimSpace(stderr))
	}
	pane := strings.TrimSpace(out)
	if !strings.HasPrefix(pane, "%") {
		return "", fmt.Errorf("resolve pane id for %s: malformed id %q", h.Name, pane)
	}
	return pane, nil
}

func (p *Persist) captureTarget(ctx context.Context, t ports.Transport, target string, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	stdout, stderr, err := t.Run(ctx, "", fmt.Sprintf("tmux capture-pane -t %s -p -S -%d", target, lines))
	if err != nil {
		return "", fmt.Errorf("capture: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

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
		shellquote.Quote(exactSession(from.Name)), shellquote.Quote(to.Name),
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
	stdout, stderr, err := t.Run(ctx, "", fmt.Sprintf(
		"if tmux has-session -t %s 2>/dev/null; then printf relay-live; else printf relay-absent; fi",
		shellquote.Quote(exactSession(h.Name)),
	))
	if err != nil {
		return false, fmt.Errorf("tmux existence probe: %w (%s)", err, strings.TrimSpace(stderr))
	}
	switch strings.TrimSpace(stdout) {
	case "relay-live":
		return true, nil
	case "relay-absent":
		return false, nil
	default:
		return false, fmt.Errorf("tmux existence probe returned malformed result")
	}
}

func (p *Persist) Destroy(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	_, _, err := t.Run(ctx, "", fmt.Sprintf("tmux kill-session -t %s 2>/dev/null || true", shellquote.Quote(exactSession(h.Name))))
	return err
}

func (p *Persist) Capture(ctx context.Context, t ports.Transport, h ports.PersistHandle, lines int) (string, error) {
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return "", err
	}
	return p.captureTarget(ctx, t, shellquote.Quote(pane), lines)
}

// Launch acknowledges the holding shell, not any particular runtime. The
// shell stamps a tmux option immediately before evaluating the command; agent
// readiness and job exit are verified by their existing lifecycle paths.
func (p *Persist) Launch(ctx context.Context, t ports.Transport, h ports.PersistHandle, command string) error {
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("launch command required")
	}
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return err
	}
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	target := shellquote.Quote(pane)
	// set-option still needs a session-scoped target; the pane id is only for
	// keystrokes. Keep the ack option on the exact session so show-option and
	// the typed launch line agree.
	optTarget := shellquote.Quote(exactPane(h.Name))
	line := fmt.Sprintf("tmux set-option -t %s @relay_launch_ack %s; %s", optTarget, shellquote.Quote(token), command)
	if _, stderr, err := t.Run(ctx, "", fmt.Sprintf("tmux send-keys -t %s -l -- %s", target, shellquote.Quote(line))); err != nil {
		return fmt.Errorf("type launch: %w (%s)", err, strings.TrimSpace(stderr))
	}
	// Submit exactly once. Retrying Enter is not idempotent: after the shell
	// execs an interactive runtime, a later Enter can approve its security gate.
	if _, stderr, err := t.Run(ctx, "", fmt.Sprintf("tmux send-keys -t %s Enter", target)); err != nil {
		return fmt.Errorf("submit launch: %w (%s)", err, strings.TrimSpace(stderr))
	}
	for attempt := 0; attempt < sendConfirmAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return &ports.DeliveryUncertainError{Err: ctx.Err()}
		case <-time.After(sendConfirmDelay):
		}
		out, _, _ := t.Run(ctx, "", fmt.Sprintf("tmux show-option -t %s -v @relay_launch_ack 2>/dev/null", optTarget))
		if strings.TrimSpace(out) == token {
			_, _, _ = t.Run(ctx, "", fmt.Sprintf("tmux set-option -u -t %s @relay_launch_ack", optTarget))
			return nil
		}
	}
	return fmt.Errorf("holding shell did not acknowledge launch in %s after %d polls", h.Name, sendConfirmAttempts)
}

func (p *Persist) ResolveGateChoice(ctx context.Context, t ports.Transport, h ports.PersistHandle, selectedOffset int) error {
	if selectedOffset < 0 {
		return fmt.Errorf("gate choice offset must be non-negative")
	}
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return err
	}
	target := shellquote.Quote(pane)
	keys := []string{"Home"}
	for i := 0; i < selectedOffset; i++ {
		keys = append(keys, "Down")
	}
	keys = append(keys, "Enter")
	_, stderr, err := t.Run(ctx, "", fmt.Sprintf("tmux send-keys -t %s %s", target, strings.Join(keys, " ")))
	if err != nil {
		return fmt.Errorf("submit explicit gate choice: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

func (p *Persist) Send(ctx context.Context, t ports.Transport, h ports.PersistHandle, text string, enter bool) error {
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return &ports.DeliveryUncertainError{Err: err}
	}
	target := shellquote.Quote(pane)
	cmd := fmt.Sprintf("tmux send-keys -t %s -l -- %s", target, shellquote.Quote(text))
	_, stderr, err := t.Run(ctx, "", cmd)
	if err != nil {
		return &ports.DeliveryUncertainError{Err: fmt.Errorf("send: %w (%s)", err, strings.TrimSpace(stderr))}
	}
	if !enter {
		return nil
	}
	marker := text
	if len(marker) > 48 {
		marker = marker[:48]
	}
	var lastScreen string
	for attempt := 0; attempt < sendConfirmAttempts; attempt++ {
		_, stderr, err = t.Run(ctx, "", fmt.Sprintf("tmux send-keys -t %s Enter", target))
		if err != nil {
			return &ports.DeliveryUncertainError{Err: fmt.Errorf("submit: %w (%s)", err, strings.TrimSpace(stderr))}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sendConfirmDelay):
		}
		screen, captureErr := p.captureTarget(ctx, t, target, sendConfirmLines)
		if captureErr != nil {
			return &ports.DeliveryUncertainError{Err: fmt.Errorf("confirm send: %w", captureErr)}
		}
		lastScreen = screen
		if messageSubmitted(screen, marker) {
			return nil
		}
	}
	if depositedWithoutTurn(lastScreen, marker) {
		return &ports.DeliveryUncertainError{Err: fmt.Errorf("text reached %s's input but no turn started after %d submit attempts", h.Name, sendConfirmAttempts)}
	}
	return &ports.DeliveryUncertainError{Err: fmt.Errorf("message is still unsent in %s's composer after %d attempts", h.Name, sendConfirmAttempts)}
}

// messageSubmitted reports positive evidence that a turn began or that a
// recognizable composer released the typed marker. Marker-on-screen alone is
// not enough: Codex often shows deposited text with no ›/>/❯ glyph, and the
// old "marker present ∧ composer not holding" check treated that as success.
func messageSubmitted(screen, marker string) bool {
	if turnStarted(screen) {
		return true
	}
	if marker == "" || !hasComposerGlyph(screen) {
		return false
	}
	return !composerHolds(screen, marker)
}

func depositedWithoutTurn(screen, marker string) bool {
	if marker == "" || turnStarted(screen) {
		return false
	}
	if composerHolds(screen, marker) {
		return true
	}
	// Codex idle composers often lack a prompt glyph; deposited text still
	// sits above the status line with no Working indicator.
	return strings.Contains(screen, marker) || strings.Contains(screen, "[Pasted Content ")
}

func turnStarted(screen string) bool {
	lower := strings.ToLower(screen)
	for _, needle := range []string{
		"esc to interrupt",
		"• working",
		"● working",
		"working (",
		"working…",
		"working...",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func hasComposerGlyph(screen string) bool {
	for _, line := range strings.Split(screen, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, ">") || strings.HasPrefix(trimmed, "❯") {
			return true
		}
	}
	return false
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
	if composerLine < 0 {
		return false
	}
	// Codex replaces large bracketed pastes with an opaque placeholder. The
	// original marker is then absent even though the composer still owns the
	// message; treating that as delivered recreates the stuck-without-Enter bug.
	pastedContent := strings.Contains(composer, "[Pasted Content ")
	if !pastedContent && !strings.Contains(composer, marker) {
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
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return err
	}
	q := shellquote.Quote(pane)
	script := fmt.Sprintf(`
pane=$(tmux display-message -p -t %s '#{pane_tty}')
w=$(tmux display-message -p -t %s '#{pane_width}')
h=$(tmux display-message -p -t %s '#{pane_height}')
stty -F "$pane" cols "$w" rows "$h" 2>/dev/null || stty <"$pane" cols "$w" rows "$h" 2>/dev/null || true
tmux send-keys -t %s C-l
`, q, q, q, q)
	_, _, err = t.Run(ctx, "", script)
	return err
}

// AttachCommand attaches a local surface to the remote session, and says which
// of the two things it did.
//
// This used to be `tmux new-session -A`, whose -A means "attach if it exists,
// else create". That is the right behaviour and the wrong report: when the host
// had rebooted, or the session had been killed, the surface came up on a fresh
// shell showing a healthy prompt with no indication that the work it was
// attached to is gone. Verified live on hamburg 2026-09-05 -- killing the
// remote session and reattaching produced a clean prompt and a brand new
// session of the same name, silently.
//
// A surface that looks alive while the agent behind it is gone is the exact
// failure this control plane is worst at catching, so the create path
// announces itself.
func (p *Persist) AttachCommand(h ports.PersistHandle, cwd string) string {
	_ = cwd
	q := shellquote.Quote(h.Name)
	// The notice is written INSIDE the new session, as its first line, not
	// before it. Printing beforehand does not work: `exec tmux new-session`
	// repaints the screen and erases it. An earlier version paused two seconds
	// so the line could be read, which is still a warning that disappears --
	// the same "looks healthy" failure a moment later. As the session's first
	// scrollback line it stays until the operator scrolls past it.
	banner := fmt.Sprintf(
		`printf '\033[33mrelay: session %%s was not running on %%s; its previous work is gone. This is a NEW session.\033[0m\n' %s $(hostname -s); exec bash -l`,
		q)
	return fmt.Sprintf(
		"if tmux has-session -t %s 2>/dev/null; then exec tmux attach -t %s; "+
			"else exec tmux new-session -s %s -- bash -lc %s; fi",
		q, q, q, shellquote.Quote(banner))
}

func (p *Persist) DeadStatus(ctx context.Context, t ports.Transport, h ports.PersistHandle) (bool, int, error) {
	exists, err := p.Exists(ctx, t, h)
	if err != nil {
		return false, 0, err
	}
	if !exists {
		return true, 0, nil
	}
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return false, 0, err
	}
	stdout, _, err := t.Run(ctx, "", fmt.Sprintf(
		`tmux list-panes -t %s -F '#{pane_dead} #{pane_dead_status}' | head -n1`,
		shellquote.Quote(pane),
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
	// tmux turns a non-zero run-shell hook into a visible "returned 1"
	// message. Sensor emission is retryable telemetry: relayd restarts must not
	// overwrite an agent pane/status history with one failure per session.
	exitCmd = "{ " + exitCmd + "; } || :"
	idleCmd = "{ " + idleCmd + "; } || :"
	hooks := fmt.Sprintf(`
SESS=%s
tmux set-option -t "$SESS" monitor-silence %d
tmux set-option -t "$SESS" silence-action any
tmux set-hook -t "$SESS" pane-died "run-shell -b %s"
tmux set-hook -t "$SESS" alert-silence "run-shell -b %s"
tmux set-option -t "$SESS" remain-on-exit on
`, shellquote.Quote(exactSessionScope(h.Name)), silenceSec,
		shellquote.Quote(exitCmd),
		shellquote.Quote(idleCmd),
	)
	// Fresh sessions (especially right after create/rename) can briefly reject
	// exact-name targets with "no such window". Wait until the active pane is
	// resolvable, then install; retry only on those transient target misses.
	var lastErr error
	for attempt := 0; attempt < sendConfirmAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sendConfirmDelay):
			}
		}
		if _, err := p.resolvePaneID(ctx, t, h); err != nil {
			lastErr = err
			continue
		}
		_, stderr, err := t.Run(ctx, "", hooks)
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("install sensors: %w (%s)", err, strings.TrimSpace(stderr))
		detail := strings.ToLower(strings.TrimSpace(stderr) + " " + err.Error())
		if !strings.Contains(detail, "no such window") && !strings.Contains(detail, "no such session") && !strings.Contains(detail, "can't find") {
			return lastErr
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("install sensors: session %s never became targetable", h.Name)
	}
	return lastErr
}
