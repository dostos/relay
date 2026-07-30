package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dostos/relay/internal/coord/sshcoord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/persist/tmux"
	"github.com/dostos/relay/internal/ports"
	sshtransport "github.com/dostos/relay/internal/transport/ssh"
	"github.com/dostos/relay/internal/ui"
	"github.com/dostos/relay/internal/viz/cmux"
)

// App wires adapters and runs CLI commands.
type App struct {
	Sessions  *core.SessionService
	Handoffs  *core.HandoffService
	Profiles  *core.ProfileService
	Auth      *core.AuthService
	Bootstrap *core.BootstrapService
	Discover  *core.DiscoverService
	Reg       *core.Registry
	Coord     ports.Coord
	Msg       *core.MsgService
	Maint     *core.MaintenanceService
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
	boot := &core.BootstrapService{NewTransport: tf}
	auth := &core.AuthService{
		Profiles:     profiles,
		Sessions:     sessions,
		Viz:          viz,
		NewTransport: tf,
	}
	return &App{
		Sessions:  sessions,
		Handoffs:  handoffs,
		Profiles:  profiles,
		Auth:      auth,
		Bootstrap: boot,
		Discover: &core.DiscoverService{
			NewTransport: tf,
			Coord:        coord,
			Profiles:     profiles,
		},
		Reg:   reg,
		Coord: coord,
		Msg:   &core.MsgService{Coord: coord, NewTransport: tf},
		Maint: &core.MaintenanceService{Sessions: sessions, Reg: reg, Viz: viz, NewTransport: tf},
		Viz:   viz,
		tf:    tf,
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
	if a.JSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	ui.Warn(err.Error())
	return 1
}

// failNext prints a structured failure carrying a self-heal hint (a `next`
// label + ready-to-run `argv`) and returns exit 1, so a blind caller can
// recover in one step without parsing prose.
func (a *App) failNext(err error, extra map[string]any) int {
	payload := map[string]any{"ok": false, "error": err.Error()}
	for k, v := range extra {
		payload[k] = v
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
	return 1
}

func rejectUnknownFlag(arg string) error {
	if strings.HasPrefix(arg, "-") {
		return fmt.Errorf("unknown flag %q", arg)
	}
	return fmt.Errorf("unexpected argument %q", arg)
}

// Run dispatches argv (without program name).
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		return a.cmdHelp()
	}
	// global flags (only --json is global; other dashed tokens belong to subcommands)
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
			if len(filtered) == 0 && strings.HasPrefix(args[i], "-") {
				return a.fail(fmt.Errorf("unknown flag %q", args[i]))
			}
			filtered = append(filtered, args[i])
		}
	}
	if len(filtered) == 0 {
		return a.cmdHelp()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch filtered[0] {
	case "help", "-h", "--help":
		return a.cmdHelp()
	case "version", "-V", "--version":
		fmt.Println("relay 0.1.0")
		return 0
	case "host":
		return a.cmdHost(ctx, filtered[1:])
	case "auth":
		return a.cmdAuth(ctx, filtered[1:])
	case "targets":
		return a.cmdTargets(ctx, filtered[1:])
	case "session", "sess":
		return a.cmdSession(ctx, filtered[1:])
	case "handoff":
		return a.cmdHandoff(ctx, filtered[1:])
	case "agent":
		return a.cmdAgent(ctx, filtered[1:])
	case "msg":
		return a.cmdMsg(ctx, filtered[1:])
	case "gc":
		return a.cmdGC(ctx, filtered[1:])
	case "events":
		return a.cmdEvents(ctx, filtered[1:])
	case "viz", "pane":
		return a.cmdViz(ctx, filtered[1:])
	case "resume":
		return a.cmdResume(ctx, filtered[1:])
	case "install-cmux-restore":
		return a.cmdInstallCmuxRestore()
	case "doctor":
		return a.cmdDoctor(ctx, filtered[1:])
	default:
		ui.Warn(fmt.Sprintf("unknown command %q", filtered[0]))
		return a.cmdHelp()
	}
}

func (a *App) cmdHelp() int {
	fmt.Print(`relay — session + handoff control plane (SSH/tmux/cmux are default adapters)

Usage:
  relay [--json] <command> ...

New machine (ssh config → discover → init):
  relay targets                       List Host aliases from ~/.ssh/config (+ Include)
  relay host discover -H HOST         Inventory + proposed host.yaml (no writes)
  relay host init -H HOST [--apply] [--force]
                                      Bootstrap relayd; write proposal with --apply

Host profiles (authoritative on each remote ~/.config/relay/host.yaml):
  relay host show -H HOST
  relay host fetch -H HOST
  relay host probe -H HOST
  relay host cache -H HOST
  relay host example -H HOST          Print starter host.yaml
  relay host bootstrap -H HOST        Install always-on relayd (unix socket; one quiet SSH)

Agent auth (claude / cursor-agent / codex / ccs:<profile> / …):
  relay auth status -H HOST [--agent NAME]
  relay auth login -H HOST --agent NAME
                                      Pane + reassemble wrapped OAuth URL + open locally
  relay auth url --session ID         Re-extract/open auth URL if the pane cropped it
  relay auth copy --from HOST --to HOST --agent NAME
                                      Copy known cred files between Linux hosts (when supported)

Sessions (explicit id; no guesswork):
  relay session create -H HOST [--repo DIR] [--cwd REMOTE] [--name NAME]
  relay session adopt -H HOST --name TMUX [--cwd REMOTE] [--repo DIR]
                                      Register an already-running remote tmux
  relay session list
  relay session get ID
  relay session capture ID [-n LINES]
  relay session send ID -- TEXT
  relay session exec ID -- CMD
  relay session resize ID
  relay session attach ID             Interactive (humans only)
  relay session destroy ID [--keep-remote] [--keep-viz]
                                      Also closes the presented cmux pane; --keep-viz leaves it.
  relay session sensors ID [--silence SEC]   Reinstall quiet idle/exit hooks

Handoffs (goal-based / long-running):
  relay handoff -H HOST --agent NAME --goal TEXT [--repo DIR] [--workspace WS] [--no-pane]
  relay handoff -H HOST --cmd "make train" [--no-pane]
  relay handoff list
  relay handoff get ID
  relay handoff finalize ID [--outcome done|failed|abandoned] [--keep-session]
  relay handoff reconcile

Agent surface (token-efficient; always JSON; NO poll loops):
  relay agent start -H HOST --agent NAME --goal TEXT | --cmd CMD [--workspace WS] [--no-pane]
  relay agent pick -H HOST                                     # suggest agent by weekly headroom (advisory)
  relay agent wait --handoff ID [--from SEQ] [--timeout SEC]   # blocks once
  relay agent send --handoff ID -- TEXT
  relay agent capture --handoff ID [-n LINES]
  relay agent done --handoff ID [--outcome done|failed|abandoned] [--keep-session]
  relay agent status --handoff ID
  # Follow response.next / response.argv. Never events tail -f in a loop.
  # Agents may also DECLARE state instead of going idle: emit kind
  # ask|note|progress|result (with meta.q/text) and 'agent wait' surfaces it.

Agent-to-agent messages (relayd channels; any channel name):
  relay msg send -H HOST -c CHANNEL [--kind K] [--from ID] [--text ... | -- ...] [--meta JSON]
  relay msg read -H HOST -c CHANNEL [--from SEQ] [--follow] [--timeout SEC]
  relay msg wait -H HOST -c CHANNEL[:SEQ] [-c CHANNEL2[:SEQ] …] [--timeout SEC]   # fan-in; first wins
  relay msg rm   -H HOST -c CHANNEL [-c CHANNEL2 …]                                # drop a channel when done
  # Thread the returned next_from per channel; NAME:SEQ gives each its own cursor.

Cleanup (one pass; reap dead sessions + prune tombstones + drop stale panes + GC channels):
  relay gc [-H HOST] [--dry-run] [--channel-ttl DAYS | --no-channel-ttl]
  # Default sweeps every registry host; one probe SSH per host. Unreachable hosts skipped.

Events (via always-on relayd on the host):
  relay events tail [-f] --handoff ID [--from SEQ]
  relay events emit --handoff ID --kind KIND

Visualization (optional cmux adapter):
  relay viz present SESSION_ID [--workspace WS] [--pane PANE] [--tab]
                                      Default: side-by-side split. --tab stacks in PANE.
  relay viz brand                     Refresh ◆ RELAY · <project> tabs + workspace pills
  relay viz focus SESSION_ID
  relay viz close SESSION_ID          Retire just the pane (session destroy does this automatically)
  relay viz layout
  relay viz save                      Snapshot live relay panes for cmux restart
  relay viz restore                   Re-attach saved panes after cmux restart

cmux session restore (survive cmux quit / Mac reboot):
  relay install-cmux-restore          Register vault agent (run by install.sh)
  relay resume [--session NAME] [--cwd DIR] [--no-reconnect]
                                      Bare form uses this cmux pane's history.
                                      Re-attach; waits/retries on SSH drop (session frozen).
  relay resume list [--probe]               live | disconnected | cleaned
                                      --probe adds real remote tmux liveness
  relay resume reap [--dry-run]             Clean entries whose remote tmux is gone
  relay resume prune [--cleaned|--all] [--days N]
                                      Drop registry tombstones (default: cleaned)

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

func (a *App) cmdTargets(ctx context.Context, args []string) int {
	_ = ctx
	if err := requireNoExtra(args); err != nil {
		return a.fail(err)
	}
	list, err := core.ListTargets()
	if err != nil {
		return a.fail(err)
	}
	if a.JSON {
		return a.errOut(a.out(map[string]any{"ok": true, "targets": list}))
	}
	fmt.Print(core.FormatTargetsText(list))
	return 0
}

func (a *App) cmdAuth(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay auth status|login|copy …"))
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "status":
		host, rest := flagHost(rest)
		agent := ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--agent", "-a":
				i++
				if i < len(rest) {
					agent = rest[i]
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		rows, err := a.Auth.Status(ctx, host, agent)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "host_id": host, "agents": rows}))
	case "login":
		host, rest := flagHost(rest)
		agent := ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--agent", "-a":
				i++
				if i < len(rest) {
					agent = rest[i]
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if host == "" || agent == "" {
			return a.fail(fmt.Errorf("usage: relay auth login -H HOST --agent NAME"))
		}
		res, err := a.Auth.Login(ctx, host, agent)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(res))
	case "url":
		sessionID := ""
		doOpen := true
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--session", "-s":
				i++
				if i < len(rest) {
					sessionID = rest[i]
				}
			case "--no-open":
				doOpen = false
			default:
				if sessionID == "" && !strings.HasPrefix(rest[i], "-") {
					sessionID = rest[i]
					continue
				}
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if sessionID == "" {
			return a.fail(fmt.Errorf("usage: relay auth url --session ID [--no-open]"))
		}
		u, err := a.Auth.ExtractAuthURL(ctx, sessionID)
		if err != nil {
			return a.fail(err)
		}
		opened := false
		if doOpen && os.Getenv("RELAY_NO_OPEN") != "1" {
			opened = openAuthURL(u)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "auth_url": u, "opened": opened, "session_id": sessionID}))
	case "copy":
		var from, to, agent string
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--from":
				i++
				if i < len(rest) {
					from = rest[i]
				}
			case "--to":
				i++
				if i < len(rest) {
					to = rest[i]
				}
			case "--agent", "-a":
				i++
				if i < len(rest) {
					agent = rest[i]
				}
			case "-H", "--host":
				return a.fail(fmt.Errorf("auth copy uses --from / --to, not -H"))
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if from == "" || to == "" || agent == "" {
			return a.fail(fmt.Errorf("usage: relay auth copy --from HOST --to HOST --agent NAME"))
		}
		res, err := a.Auth.Copy(ctx, from, to, agent)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(res))
	default:
		return a.fail(fmt.Errorf("unknown auth subcommand %q", sub))
	}
}

func openAuthURL(u string) bool {
	if u == "" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	default:
		return false
	}
	return cmd.Start() == nil
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
	case "show", "fetch": // fetch is alias of show (remote-authoritative pull)
		if err := requireNoExtra(rest); err != nil {
			return a.fail(err)
		}
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		p, err := a.Profiles.Fetch(ctx, host)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(p))
	case "cache":
		if err := requireNoExtra(rest); err != nil {
			return a.fail(err)
		}
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		c, err := a.Profiles.Cache(host)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(c))
	case "probe":
		if err := requireNoExtra(rest); err != nil {
			return a.fail(err)
		}
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
		if err := requireNoExtra(rest); err != nil {
			return a.fail(err)
		}
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
	case "discover":
		if err := requireNoExtra(rest); err != nil {
			return a.fail(err)
		}
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		if a.Discover == nil {
			return a.fail(fmt.Errorf("discover service unavailable"))
		}
		card, err := a.Discover.Discover(ctx, host)
		if err != nil {
			return a.fail(err)
		}
		if a.JSON {
			return a.errOut(a.out(card))
		}
		fmt.Print(core.FormatDiscoverText(card))
		if card.ProposalYAML != "" && card.HostYAML == "missing" {
			ui.Note(fmt.Sprintf("proposal (not written) — relay host init -H %s --apply", host))
			fmt.Print(card.ProposalYAML)
		}
		return 0
	case "init":
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		opts := core.InitOptions{}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--apply":
				opts.Apply = true
			case "--force":
				opts.Force = true
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if a.Discover == nil {
			return a.fail(fmt.Errorf("discover service unavailable"))
		}
		res, err := a.Discover.Init(ctx, host, opts, a.Bootstrap)
		if err != nil {
			return a.fail(err)
		}
		if a.JSON {
			return a.errOut(a.out(res))
		}
		fmt.Printf("host %s\n", res.HostID)
		fmt.Printf("  dry_run     %v\n", res.DryRun)
		fmt.Printf("  applied     %v\n", res.Applied)
		fmt.Printf("  profile     %v\n", res.WroteProfile)
		if res.Detail != "" {
			fmt.Printf("  detail      %s\n", res.Detail)
		}
		if res.Discover != nil {
			fmt.Print(core.FormatDiscoverText(res.Discover))
		}
		if res.DryRun && res.Discover != nil && res.Discover.ProposalYAML != "" {
			ui.Note("proposal → " + core.RemoteHostProfilePath())
			fmt.Print(res.Discover.ProposalYAML)
		}
		if res.Next != "" {
			fmt.Printf("  next        %s\n", res.Next)
		}
		if !res.OK {
			return 1
		}
		return 0
	default:
		_ = rest
		return a.fail(fmt.Errorf("unknown host subcommand %q", sub))
	}
}

func requireNoExtra(rest []string) error {
	if len(rest) == 0 {
		return nil
	}
	return rejectUnknownFlag(rest[0])
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
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
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
			if errors.Is(err, core.ErrMissingProfile) {
				return a.failNext(err, map[string]any{
					"reason":  "missing_host_profile",
					"host_id": host,
					"next":    "host init",
					"argv":    []string{"relay", "host", "init", "-H", host, "--apply"},
				})
			}
			return a.fail(err)
		}
		return a.errOut(a.out(s))
	case "adopt":
		host, rest := flagHost(args[1:])
		opts := core.CreateOpts{HostID: host, Labels: map[string]string{"adopted": "existing"}}
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
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if opts.Name == "" {
			return a.fail(fmt.Errorf("usage: relay session adopt -H HOST --name TMUX [--cwd REMOTE] [--repo DIR]"))
		}
		if opts.RepoRef != "" && !filepath.IsAbs(opts.RepoRef) {
			abs, _ := filepath.Abs(opts.RepoRef)
			opts.RepoRef = abs
		}
		s, err := a.Sessions.Adopt(ctx, opts)
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
			switch args[i] {
			case "-n", "--lines":
				if i+1 >= len(args) {
					return a.fail(fmt.Errorf("%s requires a value", args[i]))
				}
				n, _ = strconv.Atoi(args[i+1])
				i++
			default:
				return a.fail(rejectUnknownFlag(args[i]))
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
		closeViz := true
		for _, x := range args[2:] {
			switch x {
			case "--keep-remote":
				keep = true
			case "--keep-viz":
				closeViz = false
			default:
				return a.fail(rejectUnknownFlag(x))
			}
		}
		if err := a.Sessions.Destroy(ctx, args[1], keep); err != nil {
			return a.fail(err)
		}
		// Retiring the session retires its presented pane too (exact bound
		// surface, keyed by session_id — never any other pane). Best-effort:
		// a headless/unbound session simply has no binding to close.
		if closeViz && a.Viz != nil {
			_ = a.Viz.Close(ctx, args[1])
		}
		return 0
	case "sensors":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		id := args[1]
		silence := 0
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--silence":
				i++
				if i < len(args) {
					silence, _ = strconv.Atoi(args[i])
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if err := a.Handoffs.ReinstallSensors(ctx, id, silence); err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "session_id": id, "sensors": "reinstalled"}))
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
			default:
				return a.fail(rejectUnknownFlag(args[i]))
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
		case "--workspace":
			i++
			if i < len(rest) {
				opts.Workspace = rest[i]
			}
		case "--no-pane":
			opts.NoPane = true
		case "--silence":
			i++
			if i < len(rest) {
				opts.Silence, _ = strconv.Atoi(rest[i])
			}
		default:
			return a.fail(rejectUnknownFlag(rest[i]))
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

// cmdGC is the one-shot "clean up when done" sweep: reap dead sessions, prune
// tombstones, drop stale pane-state, and GC stale/empty message channels — one
// probe SSH per host. Default sweeps every registry host; -H scopes to one.
//
//	relay gc [-H HOST] [--dry-run] [--channel-ttl DAYS | --no-channel-ttl]
func (a *App) cmdGC(ctx context.Context, args []string) int {
	host, rest := flagHost(args)
	dryRun := false
	channelTTLDays := 7
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--dry-run", "-n":
			dryRun = true
		case "--channel-ttl":
			i++
			if i < len(rest) {
				channelTTLDays, _ = strconv.Atoi(rest[i])
			}
		case "--no-channel-ttl":
			channelTTLDays = 0
		case "--all":
			// default already sweeps all registry hosts when -H is absent
		default:
			return a.fail(rejectUnknownFlag(rest[i]))
		}
	}
	if channelTTLDays < 0 {
		channelTTLDays = 0
	}
	var hosts []string
	if host != "" {
		hosts = []string{host}
	}
	rep, err := a.Maint.GC(ctx, hosts, time.Duration(channelTTLDays)*24*time.Hour, false, dryRun)
	if err != nil {
		return a.fail(err)
	}
	a.JSON = true
	return a.errOut(a.out(map[string]any{"ok": true, "gc": rep}))
}

// cmdMsg is the agent-to-agent message bus over relayd channels.
//
//	relay msg send -H HOST -c CHANNEL [--kind K] [--from ID] [--text ... | -- ...] [--meta JSON]
//	relay msg read -H HOST -c CHANNEL [--from SEQ] [--follow] [--timeout S]
//	relay msg wait -H HOST -c CHANNEL [-c CHANNEL2 …] [--from SEQ] [--timeout S]   (fan-in)
func (a *App) cmdMsg(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay msg send|read|wait …"))
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "send":
		host, rest := flagHost(rest)
		var channel, kind, from, text, metaJSON string
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--channel", "-c":
				i++
				if i < len(rest) {
					channel = rest[i]
				}
			case "--kind":
				i++
				if i < len(rest) {
					kind = rest[i]
				}
			case "--from":
				i++
				if i < len(rest) {
					from = rest[i]
				}
			case "--text":
				i++
				if i < len(rest) {
					text = rest[i]
				}
			case "--meta":
				i++
				if i < len(rest) {
					metaJSON = rest[i]
				}
			case "--":
				text = strings.Join(rest[i+1:], " ")
				i = len(rest)
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if host == "" || channel == "" {
			return a.fail(fmt.Errorf("usage: relay msg send -H HOST --channel NAME [--kind K] [--from ID] [--text ...] [--meta JSON]"))
		}
		var meta map[string]any
		if metaJSON != "" {
			if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
				return a.fail(fmt.Errorf("--meta not valid JSON: %w", err))
			}
		}
		seq, err := a.Msg.Send(ctx, host, channel, kind, from, text, meta)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "channel": channel, "seq": seq}))
	case "rm":
		host, rest := flagHost(rest)
		var channels []string
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--channel", "-c":
				i++
				if i < len(rest) {
					channels = append(channels, rest[i])
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if host == "" || len(channels) == 0 {
			return a.fail(fmt.Errorf("usage: relay msg rm -H HOST --channel NAME [--channel NAME2 …]"))
		}
		removed, err := a.Msg.RemoveChannels(ctx, host, channels)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "removed": removed}))
	case "read":
		host, rest := flagHost(rest)
		var channel string
		var from int64
		follow := false
		timeout := 0
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--channel", "-c":
				i++
				if i < len(rest) {
					channel = rest[i]
				}
			case "--from":
				i++
				if i < len(rest) {
					from, _ = strconv.ParseInt(rest[i], 10, 64)
				}
			case "--follow", "-f":
				follow = true
			case "--timeout":
				i++
				if i < len(rest) {
					timeout, _ = strconv.Atoi(rest[i])
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if host == "" || channel == "" {
			return a.fail(fmt.Errorf("usage: relay msg read -H HOST --channel NAME [--from SEQ] [--follow] [--timeout S]"))
		}
		msgs, last, err := a.Msg.Read(ctx, host, channel, from, follow, time.Duration(timeout)*time.Second)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "channel": channel, "messages": msgs, "count": len(msgs), "next_from": last}))
	case "wait":
		host, rest := flagHost(rest)
		var channels []string
		var from int64
		timeout := 0
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--channel", "-c":
				i++
				if i < len(rest) {
					channels = append(channels, rest[i])
				}
			case "--from":
				i++
				if i < len(rest) {
					from, _ = strconv.ParseInt(rest[i], 10, 64)
				}
			case "--timeout":
				i++
				if i < len(rest) {
					timeout, _ = strconv.Atoi(rest[i])
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if host == "" || len(channels) == 0 {
			return a.fail(fmt.Errorf("usage: relay msg wait -H HOST --channel NAME[:SEQ] [--channel NAME2[:SEQ] …] [--from SEQ] [--timeout S]"))
		}
		// Each channel has its OWN seq space, so a single --from is a footgun for
		// fan-in. Accept a per-channel cursor as NAME:SEQ; fall back to --from.
		fromMap := map[string]int64{}
		names := make([]string, 0, len(channels))
		for _, ch := range channels {
			name := ch
			cur := from
			if i := strings.LastIndex(ch, ":"); i > 0 {
				if v, err := strconv.ParseInt(ch[i+1:], 10, 64); err == nil {
					name = ch[:i]
					cur = v
				}
			}
			names = append(names, name)
			fromMap[name] = cur
		}
		channels = names
		m, timedOut, err := a.Msg.WaitOne(ctx, host, channels, fromMap, time.Duration(timeout)*time.Second)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		if timedOut {
			return a.errOut(a.out(map[string]any{"ok": true, "timed_out": true, "next": "wait", "hint": "no message before timeout; call wait again on a new turn (do not spin)"}))
		}
		return a.errOut(a.out(map[string]any{"ok": true, "timed_out": false, "message": m, "next_from": m.Seq}))
	default:
		return a.fail(fmt.Errorf("unknown msg subcommand %q", sub))
	}
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
			default:
				return a.fail(rejectUnknownFlag(args[i]))
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
			default:
				return a.fail(rejectUnknownFlag(args[i]))
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

func (a *App) cmdAgent(ctx context.Context, args []string) int {
	// Agent surface is always JSON (token-efficient machine contract).
	a.JSON = true
	if len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay agent start|pick|wait|send|capture|done|status …"))
	}
	switch args[0] {
	case "pick":
		host, _ := flagHost(args[1:])
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		profile, err := a.Profiles.Get(ctx, host, true)
		if err != nil {
			return a.fail(err)
		}
		picked, ranking := core.Suggest(ctx, profile)
		return a.errOut(a.out(map[string]any{
			"ok": true, "host_id": host, "picked": picked, "ranking": ranking,
		}))
	case "start":
		host, rest := flagHost(args[1:])
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
			case "--workspace":
				i++
				if i < len(rest) {
					opts.Workspace = rest[i]
				}
			case "--container":
				i++
				if i < len(rest) {
					opts.Container = rest[i]
				}
			case "--no-pane":
				opts.NoPane = true
			case "--silence":
				i++
				if i < len(rest) {
					opts.Silence, _ = strconv.Atoi(rest[i])
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if opts.RepoRef == "" && opts.RemoteCWD == "" {
			if root, err := findGitRoot(""); err == nil {
				opts.RepoRef = root
			}
		}
		resp, err := a.Handoffs.AgentStart(ctx, opts)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		return 0
	case "wait":
		var handoffID string
		var from int64
		timeoutSec := 120
		for i := 1; i < len(args); i++ {
			switch args[i] {
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
			case "--timeout":
				i++
				if i < len(args) {
					timeoutSec, _ = strconv.Atoi(args[i])
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if handoffID == "" {
			return a.fail(fmt.Errorf("--handoff ID required"))
		}
		resp, err := a.Handoffs.AgentWait(ctx, handoffID, from, time.Duration(timeoutSec)*time.Second)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil && (resp == nil || !resp.OK) {
			return 1
		}
		return 0
	case "send":
		var handoffID string
		var rest []string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--handoff":
				i++
				if i < len(args) {
					handoffID = args[i]
				}
			case "--":
				rest = args[i+1:]
				i = len(args)
			default:
				if strings.HasPrefix(args[i], "-") {
					return a.fail(rejectUnknownFlag(args[i]))
				}
				rest = args[i:]
				i = len(args)
			}
		}
		text := strings.Join(rest, " ")
		if handoffID == "" || text == "" {
			return a.fail(fmt.Errorf("usage: relay agent send --handoff ID -- TEXT"))
		}
		resp, err := a.Handoffs.AgentSend(ctx, handoffID, text)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		return 0
	case "capture":
		var handoffID string
		n := 80
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--handoff":
				i++
				if i < len(args) {
					handoffID = args[i]
				}
			case "-n", "--lines":
				i++
				if i < len(args) {
					n, _ = strconv.Atoi(args[i])
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if handoffID == "" {
			return a.fail(fmt.Errorf("--handoff ID required"))
		}
		resp, err := a.Handoffs.AgentCapture(ctx, handoffID, n)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		return 0
	case "done":
		var handoffID string
		outcome := core.OutcomeDone
		keep := false
		closeViz := true
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--handoff":
				i++
				if i < len(args) {
					handoffID = args[i]
				}
			case "--outcome":
				i++
				if i < len(args) {
					outcome = core.FinalizeOutcome(args[i])
				}
			case "--keep-session":
				keep = true
			case "--keep-viz":
				closeViz = false
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if handoffID == "" {
			return a.fail(fmt.Errorf("--handoff ID required"))
		}
		resp, err := a.Handoffs.AgentDone(ctx, handoffID, outcome, keep, closeViz)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		return 0
	case "status":
		var handoffID string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--handoff":
				i++
				if i < len(args) {
					handoffID = args[i]
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if handoffID == "" {
			return a.fail(fmt.Errorf("--handoff ID required"))
		}
		resp, err := a.Handoffs.AgentStatus(ctx, handoffID)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		return 0
	default:
		return a.fail(fmt.Errorf("unknown agent subcommand %q", args[0]))
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
		sess, err := a.Sessions.Get(args[1])
		if err != nil {
			return a.fail(err)
		}
		layout := ports.Layout{Mode: "remote"}
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--workspace":
				if i+1 >= len(args) {
					return a.fail(fmt.Errorf("--workspace requires a value"))
				}
				layout.Workspace = args[i+1]
				i++
			case "--pane":
				if i+1 >= len(args) {
					return a.fail(fmt.Errorf("--pane requires a value"))
				}
				layout.Pane = args[i+1]
				i++
			case "--tab":
				layout.Tab = true
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if layout.Tab && layout.Pane == "" {
			return a.fail(fmt.Errorf("--tab requires --pane"))
		}
		launch := core.ResumeLaunchCmd(sess.Persist.Name)
		ref, err := a.Viz.Present(ctx, args[1], launch, layout)
		if err != nil {
			return a.fail(err)
		}
		sess.VizSurfaceRef = ref
		_ = a.Reg.PutSession(sess)
		core.RememberResume(sess)
		core.RememberPane(ref, sess, true)
		_ = a.applySessionChrome(ctx, sess)
		_ = a.brandAll(ctx)
		return a.errOut(a.out(map[string]string{
			"session_id": args[1],
			"surface":    ref,
			"launch":     launch,
			"brand":      core.BrandTitle(sess.Persist.Name),
		}))
	case "brand":
		if err := a.brandAll(ctx); err != nil {
			return a.fail(err)
		}
		list, _ := a.Sessions.List()
		for _, s := range list {
			_ = a.applySessionChrome(ctx, s)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "sessions": len(list)}))
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
	case "save":
		n, err := a.Viz.SaveRestorable(ctx)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "saved": n}))
	case "restore":
		n, err := a.Viz.RestoreSaved(ctx)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "restored": n}))
	default:
		return a.fail(fmt.Errorf("unknown viz subcommand %q", args[0]))
	}
}

func (a *App) cmdResume(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "list" {
		probe := false
		for _, x := range args[1:] {
			switch x {
			case "--probe":
				probe = true
			default:
				return a.fail(rejectUnknownFlag(x))
			}
		}
		var list []core.ResumeInfo
		var err error
		if probe {
			list, err = a.Sessions.ListResumeStatusProbed(ctx)
		} else {
			list, err = a.Sessions.ListResumeStatus()
		}
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "probed": probe, "sessions": list}))
	}
	if len(args) > 0 && args[0] == "reap" {
		dryRun := false
		for _, x := range args[1:] {
			switch x {
			case "--dry-run", "-n":
				dryRun = true
			default:
				return a.fail(rejectUnknownFlag(x))
			}
		}
		// Sessions-only sweep — shares MaintenanceService.GC (single reap impl).
		rep, err := a.Maint.GC(ctx, nil, 0, true, dryRun)
		if err != nil {
			return a.fail(err)
		}
		var reaped, skippedHosts []string
		kept := 0
		for _, h := range rep.Hosts {
			if !h.Reachable {
				skippedHosts = append(skippedHosts, h.Host)
				continue
			}
			reaped = append(reaped, h.ReapedSessions...)
			kept += h.KeptSessions
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "reap": map[string]any{
			"reaped": reaped, "kept": kept, "skipped_hosts": skippedHosts, "dry_run": dryRun,
		}}))
	}
	if len(args) > 0 && args[0] == "prune" {
		cleanedOnly := true
		days := 0
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--all":
				cleanedOnly = false
			case "--cleaned":
				cleanedOnly = true
			case "--days":
				i++
				if i < len(args) {
					days, _ = strconv.Atoi(args[i])
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		// Clamp so time.Duration (int64 ns) can't overflow and wrap into a
		// future cutoff that would delete everything.
		if days < 0 {
			days = 0
		}
		if days > 36500 {
			days = 36500
		}
		removed, err := core.PruneResume(cleanedOnly, time.Duration(days)*24*time.Hour)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "removed": removed, "count": len(removed)}))
	}
	var session, cwd string
	opts := core.ResumeOpts{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session", "-s":
			i++
			if i < len(args) {
				session = args[i]
				opts.Explicit = true
			}
		case "--cwd":
			i++
			if i < len(args) {
				cwd = args[i]
			}
		case "--no-reconnect":
			opts.NoReconnect = true
		case "list":
			return a.cmdResume(ctx, []string{"list"})
		default:
			return a.fail(rejectUnknownFlag(args[i]))
		}
	}
	if session == "" {
		name, paneCWD, surface, err := core.ResolveResumeFromPane()
		if err != nil {
			return a.fail(fmt.Errorf("%w\nusage: relay resume [--session NAME] [--cwd DIR] [--no-reconnect]  |  relay resume list", err))
		}
		session = name
		opts.Surface = surface
		if cwd == "" {
			cwd = paneCWD
		}
		ui.Note(fmt.Sprintf("pane %s → %s", surface, session))
	}
	if err := a.Sessions.ResumeOpts(ctx, session, cwd, opts); err != nil {
		msg := core.FormatResumeError(err)
		ui.Warn(msg)
		// Cleaned = intentional; never open a fake shell.
		if errors.Is(err, core.ErrResumeCleaned) {
			return 1
		}
		// Unknown/missing binding only — not SSH drops (those retry inside Resume).
		if isUnknownResumeBinding(err) {
			ui.Note(fmt.Sprintf("no binding for %q — opening local shell", session))
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/bash"
			}
			cmd := exec.Command(shell, "-l")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			_ = cmd.Run()
		}
		return 1
	}
	return 0
}

func isUnknownResumeBinding(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not in resume registry") ||
		strings.Contains(msg, "not live and not in resume") ||
		strings.Contains(msg, "unknown session")
}

func (a *App) cmdInstallCmuxRestore() int {
	path := cmux.DefaultCmuxJSONPath()
	if err := cmux.InstallVaultAgent(path); err != nil {
		return a.fail(err)
	}
	return a.errOut(a.out(map[string]any{
		"ok":     true,
		"config": path,
		"agent":  "relay",
		"hint":   "approve 'relay' under cmux Settings → Terminal → Resume Commands for auto-restore; or use relay viz save/restore",
	}))
}

func (a *App) cmdDoctor(ctx context.Context, args []string) int {
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
	}
	host, rest := flagHost(args)
	if err := requireNoExtra(rest); err != nil {
		return a.fail(err)
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
	if host == "" {
		checks = append(checks, check{"coord", false, "pass -H HOST to probe remote relayd"})
	} else if a.Coord != nil {
		t, err := a.tf(host)
		if err != nil {
			checks = append(checks, check{"coord", false, err.Error()})
		} else if err := a.Coord.Ensure(ctx, t); err != nil {
			checks = append(checks, check{"coord", false, err.Error()})
		} else {
			checks = append(checks, check{"coord", true, "relayd ok on " + host})
		}
	}
	_ = core.EnsureStateDirs()
	return a.errOut(a.out(map[string]any{"checks": checks, "adapters": map[string]string{
		"transport": "ssh", "persistence": "tmux", "viz": "cmux", "coord": "relayd",
	}}))
}

func (a *App) brandAll(ctx context.Context) error {
	if a.Viz == nil {
		return nil
	}
	list, err := a.Sessions.List()
	if err != nil {
		return err
	}
	labels := make(map[string]string, len(list))
	for _, s := range list {
		labels[s.ID] = core.ProjectLabel(s.Persist.Name)
	}
	return a.Viz.BrandLabels(ctx, labels)
}

func (a *App) applySessionChrome(ctx context.Context, sess *core.Session) error {
	if sess == nil || a.tf == nil {
		return nil
	}
	t, err := a.tf(sess.HostID)
	if err != nil {
		return err
	}
	return tmux.ApplyChrome(ctx, t, sess.Persist)
}
