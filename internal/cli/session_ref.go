package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

// sessionRefPos is where each session subcommand takes its session
// reference (sess-ID, HOST/NAME or NAME; #72).
var sessionRefPos = map[string]int{
	"get": 1, "readiness": 1, "rename": 1, "bridge": 1, "capture": 1,
	"send": 1, "submit": 1, "wait": 1, "exec": 1, "resize": 1, "attach": 1,
	"destroy": 1, "sensors": 1, "op": 2,
}

// registryOnly subcommands change the registry record itself, which an
// ad-hoc HOST/NAME target does not have.
var registryOnly = map[string]bool{"rename": true, "bridge": true, "destroy": true, "sensors": true}

// resolveSessionArg rewrites the session reference in args to a session id.
// done is true when it already printed a failure.
func (a *App) resolveSessionArg(ctx context.Context, sub string, args []string) (int, bool) {
	pos, ok := sessionRefPos[sub]
	if !ok || pos >= len(args) || len(args[pos]) == 0 || args[pos][0] == '-' {
		return 0, false
	}
	if a.Sessions == nil {
		return 0, false
	}
	ref := args[pos]
	sess, err := a.Sessions.Resolve(ctx, ref)
	if err != nil {
		return a.failResolve(ref, err), true
	}
	if core.IsAdhoc(sess) && registryOnly[sub] {
		a.JSON = true
		return a.failNext(fmt.Errorf("%w: %s is a tmux session relay did not create; register it first", core.ErrNotRegistered, ref), map[string]any{
			"reason": "not_registered",
			"next":   "adopt the tmux session into the registry",
			"argv":   []string{"relay", "session", "adopt", "-H", sess.HostID, "--name", sess.Persist.Name},
		}), true
	}
	args[pos] = sess.ID
	return 0, false
}

func (a *App) failResolve(ref string, err error) int {
	var amb *core.AmbiguousSessionError
	var nf *core.SessionNotFoundError
	switch {
	case errors.As(err, &amb):
		argv := []string{"relay", "session", "list"}
		if len(amb.Candidates) > 0 {
			argv = []string{"relay", "session", "get", amb.Candidates[0].Ref}
		}
		return a.failStructured(err, map[string]any{"reason": "ambiguous_session", "ref": ref, "candidates": amb.Candidates,
			"next": "repeat the command with one of the HOST/NAME candidates", "argv": argv})
	case errors.As(err, &nf):
		extra := map[string]any{"reason": "session_not_found", "ref": ref, "candidates": nf.Candidates,
			"next": "list sessions", "argv": []string{"relay", "session", "list"}}
		if core.DirectMode {
			extra["next"] = "the desktop bridge is down, so only HOST/NAME targets resolve here; address the target as HOST/NAME"
		}
		return a.failStructured(err, extra)
	default:
		return a.fail(err)
	}
}

// failStructured prints {ok:false,error,...extra} in JSON mode, and the error
// plus a next-step line otherwise. Exit 1.
func (a *App) failStructured(err error, extra map[string]any) int {
	if a.JSON {
		return a.failNext(err, extra)
	}
	code := a.fail(err)
	if argv, ok := extra["argv"].([]string); ok && len(argv) > 0 {
		fmt.Fprintf(stderr(), "  next: %s\n", joinArgv(argv))
	}
	return code
}

func (a *App) cmdSessionWait(ctx context.Context, args []string) int {
	usage := "usage: relay session wait ID --idle [--timeout 5m] [--settle 3s] [--interval 1s] [--tail N] [--json]"
	if len(args) < 1 || args[0] == "" || args[0][0] == '-' {
		return a.failStructured(errors.New(usage), map[string]any{"reason": "usage"})
	}
	id := args[0]
	o := core.WaitOptions{Timeout: 5 * time.Minute, Interval: time.Second, Settle: 3 * time.Second}
	idle := false
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		if f == "--idle" {
			idle = true
			continue
		}
		if i+1 >= len(rest) {
			return a.fail(fmt.Errorf("%s requires a value (%s)", f, usage))
		}
		v := rest[i+1]
		i++
		var err error
		switch f {
		case "--timeout":
			o.Timeout, err = time.ParseDuration(v)
		case "--settle":
			o.Settle, err = time.ParseDuration(v)
		case "--interval":
			o.Interval, err = time.ParseDuration(v)
		case "--tail":
			o.TailLines, err = strconv.Atoi(v)
			if err == nil && (o.TailLines < 0 || o.TailLines > 500) {
				err = fmt.Errorf("must be 0-500")
			}
		default:
			return a.fail(rejectUnknownFlag(f))
		}
		if err != nil {
			return a.fail(fmt.Errorf("%s: %v", f, err))
		}
	}
	if !idle {
		return a.fail(fmt.Errorf("--idle is required (the only wait condition today); %s", usage))
	}
	res, err := a.Sessions.WaitIdle(ctx, id, o)
	payload := map[string]any{"ok": err == nil && res.Outcome == core.WaitIdle, "result": res}
	code := 0
	switch {
	case err != nil:
		payload["error"] = err.Error()
		code = 1
	case res.Outcome == core.WaitBlocked:
		payload["error"] = "target is waiting at a gate only a human can answer: " + res.Reason
		payload["next"] = "inspect the gate; relay never answers it"
		payload["argv"] = []string{"relay", "session", "readiness", id}
		code = ports.ExitTargetBlocked
	case res.Outcome == core.WaitGone:
		payload["error"] = "target tmux session no longer exists"
		code = ports.ExitSessionAbsent
	case res.Outcome == core.WaitTimeout:
		payload["error"] = "timed out while target was " + res.Activity
		payload["next"] = "wait again (the target is not touched)"
		payload["argv"] = []string{"relay", "--json", "session", "wait", id, "--idle", "--timeout", o.Timeout.String()}
		code = ports.ExitWaitTimeout
	}
	a.recordUsage(usageEvent{Cmd: "session.wait", Target: res.Target, State: res.Outcome, Evidence: res.Activity, Exit: code, Error: errString(err), DurationMS: res.WaitedMS})
	if a.JSON {
		_ = a.out(payload)
	} else {
		fmt.Printf("%s %s (%s, %d samples, %dms)\n", res.Target, res.Outcome, res.Activity, res.Samples, res.WaitedMS)
		if res.Tail != "" {
			fmt.Println(res.Tail)
		}
		if msg, ok := payload["error"].(string); ok && msg != "" {
			fmt.Fprintln(stderr(), "relay: "+msg)
		}
	}
	return code
}

// directCapable lists the commands that can run on this host when the
// desktop bridge is unreachable: read-only probes and messaging that address
// a pane through tmux/ssh, with no control-host registry write (#73).
func directCapable(args []string) bool {
	w := firstWords(args, 2)
	if len(w) == 0 {
		return false
	}
	switch w[0] {
	case "version", "build", "capabilities", "caps":
		return true
	case "session", "sess":
		if len(w) < 2 {
			return false
		}
		switch w[1] {
		case "send", "submit", "wait", "readiness", "capture":
			return true
		}
	}
	return false
}

// firstWords returns the first n non-flag words of argv.
func firstWords(args []string, n int) []string {
	var out []string
	for _, a := range args {
		if a == "--" {
			break
		}
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		out = append(out, a)
		if len(out) == n {
			break
		}
	}
	return out
}

// wantsHelp reports --help/-h before any `--` separator.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// maxStdinMessage bounds `session send --stdin`.
const maxStdinMessage = 1 << 20

// rewriteStdinSend turns `session send ID [flags] --stdin` into
// `session send ID [flags] -- TEXT` with TEXT read from in.
func rewriteStdinSend(args []string, in io.Reader) ([]string, error) {
	w := firstWords(args, 2)
	if len(w) < 2 || (w[0] != "session" && w[0] != "sess") || w[1] != "send" {
		return args, nil
	}
	idx := -1
	for i, a := range args {
		if a == "--" {
			break
		}
		if a == "--stdin" {
			idx = i
		}
	}
	if idx < 0 {
		return args, nil
	}
	for _, a := range args[idx+1:] {
		if a == "--" {
			return nil, fmt.Errorf("--stdin and -- TEXT are mutually exclusive")
		}
	}
	raw, err := io.ReadAll(io.LimitReader(in, maxStdinMessage+1))
	if err != nil {
		return nil, fmt.Errorf("read message from stdin: %w", err)
	}
	if len(raw) > maxStdinMessage {
		return nil, fmt.Errorf("message on stdin exceeds %d bytes", maxStdinMessage)
	}
	text := strings.TrimRight(string(raw), "\n")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("--stdin: empty message")
	}
	out := append([]string{}, args[:idx]...)
	out = append(out, args[idx+1:]...)
	return append(out, "--", text), nil
}
