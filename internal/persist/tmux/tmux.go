// Package tmux implements ports.Persistence using remote tmux.
package tmux

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

const kind = "tmux"

const (
	// sendConfirmAttempts bounds launch-ack and sensor-install polls.
	sendConfirmAttempts = 3
	// sendConfirmPolls bounds post-submit pane reads for a send. Reads are
	// cheap and idempotent; only the Enter is not, and it is sent once.
	sendConfirmPolls = 8
	sendConfirmLines = 14
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
		inner = "exec " + ports.HoldingShell
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

// Send types text and, when enter is set, presses Enter exactly once.
//
// Success must be attributable to *this* send (#59). Before typing, Send
// captures the pane; after the single Enter it polls (to tolerate delayed
// rendering) for evidence that a turn began after the snapshot:
//
//   - a working indicator that was not on the pre-send screen at all,
//   - a working indicator whose elapsed timer restarted below the pre-send
//     value (the agent was mid-turn and a new turn began), or
//   - a recognizable composer that released the text while the text newly
//     appeared in the transcript.
//
// A `Working` line already present in scrollback before the send can never
// satisfy the check on its own. Enter is never repeated: a second Enter after
// delayed output can submit a different composer state or approve a gate, so
// an unconfirmed submit returns a *DeliveryUncertainError with a deposited or
// uncertain receipt instead.
func (p *Persist) Send(ctx context.Context, t ports.Transport, h ports.PersistHandle, text string, enter bool) (ports.SendReceipt, error) {
	rc := ports.SendReceipt{State: ports.SendNotSent}
	pane, err := p.resolvePaneID(ctx, t, h)
	if err != nil {
		return rc, err
	}
	target := shellquote.Quote(pane)
	var before string
	if enter {
		before, err = p.captureTarget(ctx, t, target, sendConfirmLines)
		if err != nil {
			return rc, fmt.Errorf("snapshot before send: %w", err)
		}
	}
	if text != "" {
		rc.State = ports.SendUncertain
		cmd := fmt.Sprintf("tmux send-keys -t %s -l -- %s", target, shellquote.Quote(text))
		if _, stderr, err := t.Run(ctx, "", cmd); err != nil {
			return rc, &ports.DeliveryUncertainError{Err: fmt.Errorf("send: %w (%s)", err, strings.TrimSpace(stderr))}
		}
		rc.State = ports.SendDeposited
		rc.Evidence = "typed"
	}
	if !enter {
		return rc, nil
	}
	rc.State = ports.SendUncertain
	marker := text
	if len(marker) > 48 {
		marker = marker[:48]
	}
	if _, stderr, err := t.Run(ctx, "", fmt.Sprintf("tmux send-keys -t %s Enter", target)); err != nil {
		return rc, &ports.DeliveryUncertainError{Err: fmt.Errorf("submit: %w (%s)", err, strings.TrimSpace(stderr))}
	}
	rc.Submits = 1
	var lastScreen string
	for poll := 0; poll < sendConfirmPolls; poll++ {
		select {
		case <-ctx.Done():
			return rc, &ports.DeliveryUncertainError{Err: ctx.Err()}
		case <-time.After(sendConfirmDelay):
		}
		screen, captureErr := p.captureTarget(ctx, t, target, sendConfirmLines)
		rc.Polls++
		if captureErr != nil {
			// A failed read is not evidence either way; keep polling.
			continue
		}
		lastScreen = screen
		if ev := freshTurn(before, screen, marker); ev != "" {
			rc.State = ports.SendSubmitted
			rc.Evidence = ev
			return rc, nil
		}
	}
	if lastScreen == "" {
		rc.Evidence = "unobservable"
		return rc, &ports.DeliveryUncertainError{Err: fmt.Errorf("could not read %s's pane after submit; delivery unknown", h.Name)}
	}
	if depositedWithoutTurn(before, lastScreen, marker) {
		rc.State = ports.SendDeposited
		rc.Evidence = "composer_holds_text"
		return rc, &ports.DeliveryUncertainError{Err: fmt.Errorf("text reached %s's input but no turn started after this send (Enter pressed once, not repeated)", h.Name)}
	}
	rc.Evidence = "no_fresh_turn"
	return rc, &ports.DeliveryUncertainError{Err: fmt.Errorf("no fresh turn observed in %s after submit; message may be unsent or still pending", h.Name)}
}

// freshTurn returns the evidence name when after shows a turn that began
// after before was captured, or "" when it does not.
func freshTurn(before, after, marker string) string {
	prev := workingLines(before)
	cur := workingLines(after)
	if len(cur) > 0 {
		seen := map[string]bool{}
		for _, l := range prev {
			seen[l] = true
		}
		var fresh []string
		for _, l := range cur {
			if !seen[l] {
				fresh = append(fresh, l)
			}
		}
		if len(fresh) > 0 {
			if len(prev) == 0 {
				return "fresh_working_indicator"
			}
			// Something already looked like a turn before the send: either a
			// stale line in scrollback or a live turn whose timer keeps
			// ticking (our text is then only queued). Only a timer that
			// restarted below every pre-send reading proves a new turn.
			prevMax, prevOK := maxElapsed(prev)
			curMin, curOK := minElapsed(fresh)
			if prevOK && curOK && curMin < prevMax {
				return "working_timer_reset"
			}
			if !prevOK && curOK {
				// Pre-send indicators carried no timer (stale text such as
				// "Working..."); a fresh, timed indicator is a new turn.
				return "fresh_working_indicator"
			}
		}
	}
	if marker != "" && hasComposerGlyph(after) && !composerHolds(after, marker) &&
		strings.Count(after, marker) > strings.Count(before, marker) {
		return "composer_released"
	}
	return ""
}

func workingLines(screen string) []string {
	var out []string
	for _, line := range strings.Split(screen, "\n") {
		if turnStarted(line) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

var elapsedRE = regexp.MustCompile(`\((?:(\d+)h\s*)?(?:(\d+)m\s*)?(\d+)s\b`)

func elapsedSeconds(line string) (int, bool) {
	m := elapsedRE.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	s, _ := strconv.Atoi(m[3])
	return h*3600 + mi*60 + s, true
}

func maxElapsed(lines []string) (int, bool) {
	best, ok := 0, false
	for _, l := range lines {
		if v, has := elapsedSeconds(l); has && (!ok || v > best) {
			best, ok = v, true
		}
	}
	return best, ok
}

func minElapsed(lines []string) (int, bool) {
	best, ok := 0, false
	for _, l := range lines {
		if v, has := elapsedSeconds(l); has && (!ok || v < best) {
			best, ok = v, true
		}
	}
	return best, ok
}

// depositedWithoutTurn reports that the text is visibly still waiting in the
// input after the submit. Working indicators already on the pre-send screen
// are ignored; a new, unattributable one means "uncertain", not "deposited".
func depositedWithoutTurn(before, after, marker string) bool {
	if marker == "" {
		return false
	}
	seen := map[string]bool{}
	for _, l := range workingLines(before) {
		seen[l] = true
	}
	for _, l := range workingLines(after) {
		if !seen[l] {
			return false
		}
	}
	if composerHolds(after, marker) {
		return true
	}
	// Codex idle composers often lack a prompt glyph; deposited text still
	// sits above the status line with no Working indicator.
	if strings.Contains(after, "[Pasted Content ") && !strings.Contains(before, "[Pasted Content ") {
		return true
	}
	return strings.Count(after, marker) > strings.Count(before, marker)
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
//
// The create path starts in the session's recorded cwd, as Create does. It
// used to ignore cwd, so a session lost to a host reboot came back in $HOME
// and its agent could not be resumed where it ran (madrid, 2026-10-10).
func (p *Persist) AttachCommand(h ports.PersistHandle, cwd string) string {
	q := shellquote.Quote(h.Name)
	startDir := ""
	if cwd != "" {
		if expr, err := shellquote.PathExpr(cwd); err == nil {
			startDir = " -c " + expr
		}
	}
	// The notice is written INSIDE the new session, as its first line, not
	// before it. Printing beforehand does not work: `exec tmux new-session`
	// repaints the screen and erases it. An earlier version paused two seconds
	// so the line could be read, which is still a warning that disappears --
	// the same "looks healthy" failure a moment later. As the session's first
	// scrollback line it stays until the operator scrolls past it.
	banner := fmt.Sprintf(
		`printf '\033[33mrelay: session %%s was not running on %%s; its previous work is gone. This is a NEW session.\033[0m\n' %s $(hostname -s); exec %s`,
		q, ports.HoldingShell)
	return fmt.Sprintf(
		"if tmux has-session -t %s 2>/dev/null; then exec tmux attach -t %s; "+
			"else exec tmux new-session -s %s%s -- bash -lc %s; fi",
		q, q, q, startDir, shellquote.Quote(banner))
}

// AttachExistingCommand attaches only to a live session. A missing session is
// reported (stderr line, exit ports.ExitSessionAbsent) and never created, so a
// presenter reopening a put-away session learns the work is gone instead of
// landing in an empty session of the same name.
func (p *Persist) AttachExistingCommand(h ports.PersistHandle) string {
	q := shellquote.Quote(h.Name)
	return fmt.Sprintf(
		"if tmux has-session -t %s 2>/dev/null; then exec tmux attach -t %s; "+
			"else printf 'relay: tmux session %%s does not exist on %%s\\n' %s $(hostname -s) >&2; exit %d; fi",
		q, q, q, ports.ExitSessionAbsent)
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
