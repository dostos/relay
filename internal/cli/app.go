package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dostos/relay/internal/coord/sshcoord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/persist/tmux"
	"github.com/dostos/relay/internal/ports"
	sshtransport "github.com/dostos/relay/internal/transport/ssh"
	"github.com/dostos/relay/internal/viz/cmux"
)

// App wires adapters and runs CLI commands.
type App struct {
	Sessions  *core.SessionService
	Handoffs  *core.HandoffService
	Profiles  *core.ProfileService
	Bootstrap *core.BootstrapService
	Reg       *core.Registry
	Coord     ports.Coord
	Viz       ports.Viz
	JSON      bool
	tf        core.TransportFactory
}

// New constructs the default App (SSH + tmux + cmux + relayd coord).
func New() *App {
	reg := &core.Registry{}
	persist := tmux.New()
	viz := cmux.New()
	coord := sshcoord.New()
	tf := func(hostID string) (ports.Transport, error) {
		if hostID == "" {
			return nil, fmt.Errorf("host required")
		}
		return sshtransport.New(hostID), nil
	}
	profiles := &core.ProfileService{NewTransport: tf}
	sessions := &core.SessionService{
		Reg:          reg,
		Profiles:     profiles,
		NewTransport: tf,
		Persist:      persist,
	}
	handoffs := &core.HandoffService{
		Sessions:     sessions,
		Reg:          reg,
		Profiles:     profiles,
		Persist:      persist,
		Coord:        coord,
		Viz:          viz,
		NewTransport: tf,
	}
	return &App{
		Sessions:  sessions,
		Handoffs:  handoffs,
		Profiles:  profiles,
		Bootstrap: &core.BootstrapService{NewTransport: tf},
		Reg:       reg,
		Coord:     coord,
		Viz:       viz,
		tf:        tf,
	}
}

func (a *App) out(v any) error {
	if a.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	switch t := v.(type) {
	case string:
		fmt.Println(t)
	default:
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
	}
	return nil
}

func (a *App) fail(err error) int {
	fmt.Fprintf(os.Stderr, "relay: %v\n", err)
	return 1
}

// Run dispatches argv (without program name).
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		return a.cmdHelp()
	}
	// global flags
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			a.JSON = true
		case "-h", "--help", "help":
			if len(filtered) == 0 && i == 0 {
				return a.cmdHelp()
			}
			filtered = append(filtered, args[i])
		default:
			filtered = append(filtered, args[i])
		}
	}
	if len(filtered) == 0 {
		return a.cmdHelp()
	}
	ctx := context.Background()
	switch filtered[0] {
	case "help", "-h", "--help":
		return a.cmdHelp()
	case "version", "-V", "--version":
		fmt.Println("relay 0.1.0")
		return 0
	case "host":
		return a.cmdHost(ctx, filtered[1:])
	case "session", "sess":
		return a.cmdSession(ctx, filtered[1:])
	case "handoff":
		return a.cmdHandoff(ctx, filtered[1:])
	case "events":
		return a.cmdEvents(ctx, filtered[1:])
	case "viz", "pane":
		return a.cmdViz(ctx, filtered[1:])
	case "doctor":
		return a.cmdDoctor(ctx)
	default:
		fmt.Fprintf(os.Stderr, "relay: unknown command %q\n", filtered[0])
		return a.cmdHelp()
	}
}

func (a *App) cmdHelp() int {
	fmt.Print(`relay — session + handoff control plane (SSH/tmux/cmux are default adapters)

Usage:
  relay [--json] <command> ...

Host profiles (authoritative on each remote ~/.config/relay/host.yaml):
  relay host show -H HOST
  relay host fetch -H HOST
  relay host probe -H HOST
  relay host cache -H HOST
  relay host example -H HOST          Print starter host.yaml
  relay host bootstrap -H HOST        Install always-on relayd (unix socket; one quiet SSH)

Sessions (explicit id; no guesswork):
  relay session create -H HOST [--repo DIR] [--cwd REMOTE] [--name NAME]
  relay session list
  relay session get ID
  relay session capture ID [-n LINES]
  relay session send ID -- TEXT
  relay session exec ID -- CMD
  relay session resize ID
  relay session attach ID             Interactive (humans only)
  relay session destroy ID [--keep-remote]

Handoffs (goal-based / long-running):
  relay handoff -H HOST --agent NAME --goal TEXT [--repo DIR] [--no-pane]
  relay handoff -H HOST --cmd "make train" [--no-pane]
  relay handoff list
  relay handoff get ID
  relay handoff finalize ID [--outcome done|failed|abandoned] [--keep-session]
  relay handoff reconcile

Events (via always-on relayd on the host):
  relay events tail [-f] --handoff ID [--from SEQ]
  relay events emit --handoff ID --kind KIND

Visualization (optional cmux adapter):
  relay viz present SESSION_ID
  relay viz focus SESSION_ID
  relay viz close SESSION_ID
  relay viz layout

  relay doctor
`)
	return 0
}

func flagHost(args []string) (host string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-H" || args[i] == "--host" {
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
			continue
		}
		rest = append(rest, args[i])
	}
	return host, rest
}

func (a *App) cmdHost(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("host subcommand required"))
	}
	sub := args[0]
	host, rest := flagHost(args[1:])
	switch sub {
	case "example":
		if host == "" {
			host = "HOST"
		}
		fmt.Print(core.ExampleHostProfileYAML(host))
		return 0
	case "show", "fetch":
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		p, err := a.Profiles.Fetch(ctx, host)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(p))
	case "cache":
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		c, err := a.Profiles.Cache(host)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(c))
	case "probe":
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		p, err := a.Profiles.Probe(ctx, host)
		if err != nil {
			return a.fail(err)
		}
		// Also check relayd (serial; one ping — no burst)
		if a.Coord != nil {
			if t, err := a.tf(host); err == nil {
				if err := a.Coord.Ensure(ctx, t); err != nil {
					p.Meta = map[string]any{"relayd": err.Error()}
				} else {
					p.Meta = map[string]any{"relayd": "ok"}
				}
			}
		}
		return a.errOut(a.out(p))
	case "bootstrap":
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		res, err := a.Bootstrap.Bootstrap(ctx, host)
		if res != nil {
			_ = a.out(res)
		}
		if err != nil {
			return a.fail(err)
		}
		return 0
	default:
		_ = rest
		return a.fail(fmt.Errorf("unknown host subcommand %q", sub))
	}
}

func (a *App) errOut(err error) int {
	if err != nil {
		return a.fail(err)
	}
	return 0
}

func findGitRoot(dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repo: %s", dir)
	}
	return strings.TrimSpace(string(b)), nil
}

func (a *App) cmdSession(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("session subcommand required"))
	}
	sub := args[0]
	switch sub {
	case "list":
		list, err := a.Sessions.List()
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(list))
	case "get":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		s, err := a.Sessions.Get(args[1])
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(s))
	case "create":
		host, rest := flagHost(args[1:])
		opts := core.CreateOpts{HostID: host}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--repo":
				i++
				if i < len(rest) {
					opts.RepoRef = rest[i]
				}
			case "--cwd", "-R":
				i++
				if i < len(rest) {
					opts.RemoteCWD = rest[i]
				}
			case "--name", "-s":
				i++
				if i < len(rest) {
					opts.Name = rest[i]
				}
			}
		}
		if opts.RepoRef == "" && opts.RemoteCWD == "" {
			root, err := findGitRoot("")
			if err == nil {
				opts.RepoRef = root
			}
		}
		if opts.RepoRef != "" && !filepath.IsAbs(opts.RepoRef) {
			abs, _ := filepath.Abs(opts.RepoRef)
			opts.RepoRef = abs
		}
		s, err := a.Sessions.Create(ctx, opts)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(s))
	case "capture":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		id := args[1]
		n := 50
		for i := 2; i < len(args); i++ {
			if (args[i] == "-n" || args[i] == "--lines") && i+1 < len(args) {
				n, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
		text, err := a.Sessions.Capture(ctx, id, n)
		if err != nil {
			return a.fail(err)
		}
		if a.JSON {
			return a.errOut(a.out(map[string]string{"id": id, "text": text}))
		}
		fmt.Print(text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Println()
		}
		return 0
	case "send":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		id := args[1]
		text, ok := afterDashDash(args[2:])
		if !ok || text == "" {
			return a.fail(fmt.Errorf("usage: relay session send ID -- TEXT"))
		}
		if err := a.Sessions.Send(ctx, id, text, true); err != nil {
			return a.fail(err)
		}
		return 0
	case "exec":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		id := args[1]
		cmd, ok := afterDashDash(args[2:])
		if !ok || cmd == "" {
			return a.fail(fmt.Errorf("usage: relay session exec ID -- CMD"))
		}
		stdout, stderr, err := a.Sessions.Exec(ctx, id, cmd)
		if a.JSON {
			return a.errOut(a.out(map[string]any{"stdout": stdout, "stderr": stderr, "error": errString(err)}))
		}
		fmt.Print(stdout)
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		if err != nil {
			return 1
		}
		return 0
	case "resize":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		if err := a.Sessions.Resize(ctx, args[1]); err != nil {
			return a.fail(err)
		}
		return 0
	case "attach":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		if err := a.Sessions.Attach(ctx, args[1]); err != nil {
			return a.fail(err)
		}
		return 0
	case "destroy":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		keep := false
		for _, x := range args[2:] {
			if x == "--keep-remote" {
				keep = true
			}
		}
		if err := a.Sessions.Destroy(ctx, args[1], keep); err != nil {
			return a.fail(err)
		}
		return 0
	default:
		return a.fail(fmt.Errorf("unknown session subcommand %q", sub))
	}
}

func afterDashDash(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" {
			return strings.Join(args[i+1:], " "), true
		}
	}
	if len(args) > 0 {
		return strings.Join(args, " "), true
	}
	return "", false
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *App) cmdHandoff(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("handoff args required"))
	}
	switch args[0] {
	case "list":
		list, err := a.Reg.ListHandoffs()
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(list))
	case "get":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("handoff id required"))
		}
		h, err := a.Reg.GetHandoff(args[1])
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(h))
	case "finalize":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("handoff id required"))
		}
		id := args[1]
		var outcome core.FinalizeOutcome
		keep := false
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--outcome":
				i++
				if i < len(args) {
					outcome = core.FinalizeOutcome(args[i])
				}
			case "--keep-session":
				keep = true
			}
		}
		h, err := a.Handoffs.Finalize(ctx, id, outcome, keep)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(h))
	case "reconcile":
		n, err := a.Handoffs.Reconcile(ctx)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]int{"finalized": n}))
	}

	// launch form: relay handoff -H HOST --agent/--goal or --cmd
	host, rest := flagHost(args)
	opts := core.HandoffOpts{HostID: host}
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--agent":
			i++
			if i < len(rest) {
				opts.Agent = rest[i]
			}
		case "--goal":
			i++
			if i < len(rest) {
				opts.Goal = rest[i]
			}
		case "--cmd", "--command":
			i++
			if i < len(rest) {
				opts.Command = rest[i]
			}
		case "--repo":
			i++
			if i < len(rest) {
				opts.RepoRef = rest[i]
			}
		case "--cwd", "-R":
			i++
			if i < len(rest) {
				opts.RemoteCWD = rest[i]
			}
		case "--name", "-s":
			i++
			if i < len(rest) {
				opts.Name = rest[i]
			}
		case "--no-pane":
			opts.NoPane = true
		case "--silence":
			i++
			if i < len(rest) {
				opts.Silence, _ = strconv.Atoi(rest[i])
			}
		}
	}
	if opts.RepoRef == "" && opts.RemoteCWD == "" {
		if root, err := findGitRoot(""); err == nil {
			opts.RepoRef = root
		}
	}
	b, _, err := a.Handoffs.Launch(ctx, opts)
	if err != nil {
		return a.fail(err)
	}
	// Always print binding as JSON (agent contract).
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(b)
	return 0
}

func (a *App) cmdEvents(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay events tail|emit …"))
	}
	switch args[0] {
	case "emit":
		var handoffID, kind string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--handoff":
				i++
				if i < len(args) {
					handoffID = args[i]
				}
			case "--kind", "-k":
				i++
				if i < len(args) {
					kind = args[i]
				}
			}
		}
		if handoffID == "" || kind == "" {
			return a.fail(fmt.Errorf("usage: relay events emit --handoff ID --kind KIND"))
		}
		seq, err := a.Handoffs.EmitEvent(ctx, handoffID, kind, nil)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "seq": seq, "kind": kind}))
	case "tail":
		follow := false
		var handoffID string
		var from int64
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "-f", "--follow":
				follow = true
			case "--handoff":
				i++
				if i < len(args) {
					handoffID = args[i]
				}
			case "--from":
				i++
				if i < len(args) {
					from, _ = strconv.ParseInt(args[i], 10, 64)
				}
			}
		}
		if handoffID == "" {
			return a.fail(fmt.Errorf("--handoff ID required"))
		}
		if err := a.Handoffs.TailEvents(ctx, handoffID, from, follow, os.Stdout); err != nil {
			if ctx.Err() != nil {
				return 0
			}
			return a.fail(err)
		}
		return 0
	default:
		return a.fail(fmt.Errorf("usage: relay events tail|emit …"))
	}
}

func (a *App) cmdViz(ctx context.Context, args []string) int {
	if a.Viz == nil || !a.Viz.Available(ctx) {
		return a.fail(fmt.Errorf("viz adapter unavailable (is cmux running?)"))
	}
	if len(args) == 0 {
		return a.fail(fmt.Errorf("viz subcommand required"))
	}
	switch args[0] {
	case "layout":
		out, err := a.Viz.Layout(ctx)
		if err != nil {
			return a.fail(err)
		}
		fmt.Print(out)
		return 0
	case "present":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		cmd, err := a.Sessions.AttachCommand(args[1])
		if err != nil {
			return a.fail(err)
		}
		sess, err := a.Sessions.Get(args[1])
		if err != nil {
			return a.fail(err)
		}
		full := fmt.Sprintf("ssh -t %s -- %s", sess.HostID, cmd)
		ref, err := a.Viz.Present(ctx, args[1], full, ports.Layout{Mode: "remote"})
		if err != nil {
			return a.fail(err)
		}
		sess.VizSurfaceRef = ref
		_ = a.Reg.PutSession(sess)
		return a.errOut(a.out(map[string]string{"session_id": args[1], "surface": ref}))
	case "focus":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		if err := a.Viz.Focus(ctx, args[1]); err != nil {
			return a.fail(err)
		}
		return 0
	case "close":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		if err := a.Viz.Close(ctx, args[1]); err != nil {
			return a.fail(err)
		}
		return 0
	default:
		return a.fail(fmt.Errorf("unknown viz subcommand %q", args[0]))
	}
}

func (a *App) cmdDoctor(ctx context.Context) int {
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
	}
	var checks []check
	if _, err := exec.LookPath("ssh"); err != nil {
		checks = append(checks, check{"ssh", false, err.Error()})
	} else {
		checks = append(checks, check{"ssh", true, ""})
	}
	if _, err := exec.LookPath("git"); err != nil {
		checks = append(checks, check{"git", false, err.Error()})
	} else {
		checks = append(checks, check{"git", true, ""})
	}
	cmuxOK := a.Viz != nil && a.Viz.Available(ctx)
	detail := "optional"
	if cmuxOK {
		detail = "available"
	}
	checks = append(checks, check{"cmux_viz", cmuxOK, detail})
	checks = append(checks, check{"state_dir", true, core.StateRoot()})
	checks = append(checks, check{"coord", true, "relayd (unix socket on remote; no TCP)"})
	_ = core.EnsureStateDirs()
	return a.errOut(a.out(map[string]any{"checks": checks, "adapters": map[string]string{
		"transport": "ssh", "persistence": "tmux", "viz": "cmux", "coord": "relayd",
	}}))
}
