package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/coord/sshcoord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/persist/tmux"
	"github.com/dostos/relay/internal/ports"
	localtransport "github.com/dostos/relay/internal/transport/local"
	sshtransport "github.com/dostos/relay/internal/transport/ssh"
	"github.com/dostos/relay/internal/ui"
	"github.com/dostos/relay/internal/viz/cmux"
)

// App wires adapters and runs CLI commands.
type App struct {
	Sessions    *core.SessionService
	Handoffs    *core.HandoffService
	Profiles    *core.ProfileService
	Auth        *core.AuthService
	Bootstrap   *core.BootstrapService
	Discover    *core.DiscoverService
	Reg         *core.Registry
	Coord       ports.Coord
	Msg         *core.MsgService
	Boards      *core.BoardService
	Roots       *core.RootService
	Maint       *core.MaintenanceService
	Parents     *core.ParentService
	Policies    *core.PolicyService
	Viz         ports.Viz
	JSON        bool
	CompactJSON bool
	tf          core.TransportFactory
}

// New constructs the default App (SSH + tmux + cmux + relayd coord).
func New() *App {
	reg := &core.Registry{}
	persist := tmux.New()
	viz := cmux.New()
	coord := sshcoord.New()
	localHostID := core.LocalHostIDFromProfile()
	tf := func(hostID string) (ports.Transport, error) {
		if hostID == "" {
			return nil, fmt.Errorf("host required")
		}
		// "local" is an identity, not a resolvable hostname. Sending it to ssh
		// made every local session — including root manager panes — fail with
		// "could not resolve hostname local".
		if hostID == core.LocalHostID || hostID == "self" || (localHostID != "" && hostID == localHostID) {
			return localtransport.New(), nil
		}
		return sshtransport.New(hostID), nil
	}
	profiles := &core.ProfileService{NewTransport: tf}
	sessions := &core.SessionService{
		Reg:          reg,
		Profiles:     profiles,
		NewTransport: tf,
		Persist:      persist,
		Viz:          viz,
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
	msgs := &core.MsgService{Coord: coord, NewTransport: tf}
	policies := &core.PolicyService{}
	parents := &core.ParentService{Reg: reg, Sessions: sessions, Coord: coord, Viz: viz, Notifier: viz, Policies: policies, NewTransport: tf}
	handoffs.ParentRouter = parents
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
		Reg:      reg,
		Coord:    coord,
		Msg:      msgs,
		Boards:   &core.BoardService{Reg: reg, Msg: msgs},
		Roots:    &core.RootService{Reg: reg, Sessions: sessions},
		Maint:    &core.MaintenanceService{Sessions: sessions, Reg: reg, Viz: viz, NewTransport: tf},
		Parents:  parents,
		Policies: policies,
		Viz:      viz,
		tf:       tf,
	}
}

func (a *App) out(v any) error {
	if a.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if !a.CompactJSON {
			enc.SetIndent("", "  ")
		}
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
	enc.SetEscapeHTML(false)
	if !a.CompactJSON {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(payload)
	return 1
}

func rejectUnknownFlag(arg string) error {
	if strings.HasPrefix(arg, "-") {
		return fmt.Errorf("unknown flag %q", arg)
	}
	return fmt.Errorf("unexpected argument %q", arg)
}

func (a *App) forwardThroughDesktopBridge(args []string) (int, bool) {
	for _, arg := range args {
		if arg == "--json" {
			continue
		}
		// Signals are host-local hook events. Sending them through the desktop
		// bridge would add latency and break if the pane's attach is reconnecting.
		if arg == "signal" || arg == "hook" || arg == "ask" {
			return 0, false
		}
		break
	}
	sock := strings.TrimSpace(os.Getenv(bridge.SocketEnv))
	source := bridge.Source{
		SessionID:   os.Getenv("RELAY_SESSION_ID"),
		HostID:      os.Getenv("RELAY_SESSION_HOST"),
		PersistName: os.Getenv("RELAY_SESSION_NAME"),
		Token:       os.Getenv(bridge.SourceTokenEnv),
	}
	if sock == "" {
		if identity, err := core.LoadBridgeIdentityForCurrentPane(); err == nil {
			sock = identity.Socket
			source = bridge.Source{SessionID: identity.SessionID, HostID: identity.HostID, PersistName: identity.PersistName, Token: identity.Token}
		}
	}
	if sock == "" || os.Getenv(bridge.LocalInvokeEnv) == "1" {
		return 0, false
	}
	resp, err := (bridge.Client{SockPath: sock}).Invoke(context.Background(), args, source)
	if err != nil {
		return a.fail(err), true
	}
	if resp.Build == "" {
		ui.Warn("desktop bridge build is unknown; upgrade/restart the control-plane bridge")
	} else if resp.Build != coord.Build {
		ui.Warn(fmt.Sprintf("desktop bridge build drift: bridge=%s client=%s; upgrade/restart the control-plane bridge", resp.Build, coord.Build))
	}
	if resp.Stdout != "" {
		fmt.Fprint(os.Stdout, resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if resp.Error != "" && resp.Stderr == "" {
		ui.Warn(resp.Error)
	}
	return resp.ExitCode, true
}

func ensureDesktopBridge(ctx context.Context) (string, error) {
	sock := core.DesktopBridgeSocketPath()
	client := bridge.Client{SockPath: sock}
	if client.Ping(ctx) == nil {
		return sock, nil
	}
	relayBin := core.RelayBin()
	relaydBin := filepath.Join(filepath.Dir(relayBin), "relayd")
	if _, err := os.Stat(relaydBin); err != nil {
		var lookupErr error
		relaydBin, lookupErr = exec.LookPath("relayd")
		if lookupErr != nil {
			return "", fmt.Errorf("relayd not installed beside relay")
		}
	}
	if err := core.EnsureStateDirs(); err != nil {
		return "", err
	}
	logPath := filepath.Join(core.StateRoot(), "desktop-bridge.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(relaydBin, "bridge", "--relay-bin", relayBin)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", err
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()
	for i := 0; i < 40; i++ {
		if client.Ping(ctx) == nil {
			return sock, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("desktop relay bridge did not start; see %s", logPath)
}

func sourceFromEnvironment(reg *core.Registry) (sessionID, hostID, persistName, repoRef string) {
	sessionID = strings.TrimSpace(os.Getenv(bridge.SourceSessionEnv))
	hostID = strings.TrimSpace(os.Getenv(bridge.SourceHostEnv))
	persistName = strings.TrimSpace(os.Getenv(bridge.SourcePersistEnv))
	if sessionID != "" && reg != nil {
		if sess, err := reg.GetSession(sessionID); err == nil {
			// The authenticated session id selects the local record. Do not trust
			// host/name snapshots supplied by the remote process for lineage.
			hostID = sess.HostID
			persistName = sess.Persist.Name
			repoRef = sess.RepoRef
		}
	}
	return
}

func (a *App) applyHandoffSource(ctx context.Context, opts *core.HandoffOpts) (string, error) {
	if opts == nil {
		return "", nil
	}
	sessionID, hostID, persistName, repoRef := sourceFromEnvironment(a.Reg)
	// A bridge-authenticated child may create only a direct child of itself.
	// It cannot name a grandparent/root and bypass its immediate manager.
	if sessionID != "" && opts.SourceSessionID != "" && opts.SourceSessionID != sessionID {
		return "", fmt.Errorf("handoff parent %s bypasses authenticated manager %s", opts.SourceSessionID, sessionID)
	}
	if sessionID == "" && opts.SourceSessionID != "" {
		sessionID = opts.SourceSessionID
		if sess, err := a.Reg.GetSession(sessionID); err == nil {
			hostID, persistName, repoRef = sess.HostID, sess.Persist.Name, sess.RepoRef
		} else {
			return "", err
		}
	}
	if sessionID == "" && a.Parents != nil {
		surface, ok := core.SurfaceFromEnvironment()
		if !ok && strings.TrimSpace(os.Getenv("CMUX_SURFACE_ID")) != "" {
			if resolved, err := core.CurrentSurface(); err == nil {
				surface, ok = resolved, true
			}
		}
		if ok {
			repos := []string{}
			if root, err := findGitRoot(""); err == nil {
				repos = append(repos, root)
			}
			parent, _, err := a.Parents.RegisterLocal(ctx, core.RegisterParentOpts{Surface: surface, RepoRefs: repos, WakeMode: "inject"})
			if err != nil {
				return "", err
			}
			sessionID, hostID, persistName, repoRef = parent.ID, parent.HostID, parent.Persist.Name, parent.RepoRef
		}
	}
	opts.SourceSessionID = sessionID
	opts.SourceHostID = hostID
	opts.SourcePersistName = persistName
	if opts.Workspace == "" || opts.Pane == "" {
		workspace, pane := a.locationForSource(sessionID)
		if opts.Workspace == "" {
			opts.Workspace = workspace
		}
		if opts.Pane == "" {
			opts.Pane = pane
		}
	}
	return repoRef, nil
}

func (a *App) locationForSource(sessionID string) (workspace, pane string) {
	if sessionID == "" || a.Reg == nil || a.Viz == nil {
		return "", ""
	}
	sess, err := a.Reg.GetSession(sessionID)
	if err != nil || sess.VizSurfaceRef == "" {
		return "", ""
	}
	if resolver, ok := a.Viz.(interface {
		LocationForSurface(context.Context, string) (string, string)
	}); ok {
		return resolver.LocationForSurface(context.Background(), sess.VizSurfaceRef)
	}
	return "", ""
}

// Run dispatches argv (without program name).
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		return a.cmdHelp()
	}
	if code, forwarded := a.forwardThroughDesktopBridge(args); forwarded {
		return code
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
	case "build":
		fmt.Println(coord.Build)
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
	case "parent":
		return a.cmdParent(ctx, filtered[1:])
	case "resolve":
		return a.cmdResolve(ctx, filtered[1:])
	case "log":
		return a.cmdCommunicationLog(filtered[1:])
	case "policy":
		return a.cmdPolicy(filtered[1:])
	case "signal", "hook":
		return a.cmdSignal(ctx, filtered[0], filtered[1:])
	case "ask":
		return a.cmdAsk(ctx, filtered[1:])
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
	case "history":
		return a.cmdHistory(filtered[1:])
	case "board":
		return a.cmdBoard(ctx, filtered[1:])
	case "root":
		return a.cmdRoot(ctx, filtered[1:])
	case "supervise":
		return a.cmdSupervise(ctx, filtered[1:])
	default:
		if len(filtered) == 2 {
			return a.cmdNamed(ctx, filtered[0], filtered[1])
		}
		ui.Warn(fmt.Sprintf("unknown command %q", filtered[0]))
		return a.cmdHelp()
	}
}

func (a *App) cmdHelp() int {
	fmt.Print(`relay — session + handoff control plane (SSH/tmux/cmux are default adapters)

Usage:
  relay [--json] <command> ...
  relay HOST NAME                    Open/create named tmux on HOST in this pane

New machine (ssh config → discover → init):
  relay targets                       List Host aliases from ~/.ssh/config (+ Include)
  relay host discover -H HOST         Inventory + proposed host.yaml (no writes)
  relay host init -H HOST [--apply] [--force]
                                      Install relay + relayd; write proposal with --apply

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
  relay session rename ID NAME        Rename tmux + reconnect/checkpoint identity in place
  relay session bridge ID             Repair adopted pane bridge identity without restart
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
  relay handoff -H HOST --agent NAME --goal TEXT [--repo DIR] [--parent SESSION] [--workspace WS] [--pane PANE] [--no-pane]
  relay handoff -H HOST --cmd "make train" [--no-pane]
  relay handoff list
  relay handoff get ID
  relay handoff finalize ID [--outcome done|failed|abandoned] [--keep-session]
  relay handoff reconcile
  relay history                      Show durable relay/handoff lineage

Agent surface (token-efficient; always JSON; NO poll loops):
  relay agent [protocol]                                    # print compact orchestration contract
  relay agent start HOST AGENT [options] -- GOAL
  relay agent restart HANDOFF [--repo DIR] [--cwd REMOTE] [--name NAME] [--no-pane]
  relay agent start HOST --cmd CMD [options]
  relay agent pick HOST                                        # suggest agent by weekly headroom (advisory)
  relay agent wait ID [--from SEQ] [--timeout SEC]              # blocks once
  relay agent send ID [--] TEXT
  relay agent capture ID [-n LINES]
  relay agent done ID [--outcome done|failed|abandoned] [--keep-session]
  relay agent status ID
  relay ask [--] QUESTION                                  # declare blocked input explicitly
  relay resolve MESSAGE [--] DECISION                        # the only response handshake
  relay log [CURSOR]                                         # new compact manager context
  # Managed starts have no follow-up; execute response.argv only when present.
  # Agents may also DECLARE state instead of going idle: emit kind
  # ask|note|progress|result (with meta.q/text); the parent watcher surfaces it.

Long-lived goal orchestration (durable compact inbox + guarded local-pane cleanup):
  relay parent register [--surface REF] [--name NAME] [--repo DIR ...] [--wake inject|notify]
  relay parent bind PARENT [--surface REF]     # preserve identity after cmux restart
  relay parent link PARENT HANDOFF             # adopt an already-running goal
  relay parent move PARENT HANDOFF             # explicitly repair a wrong parent edge
  relay parent list
  relay parent state PARENT active|idle|complete
  relay parent status ID
  relay parent retire ID [--dry-run]

Automatic handling policies (desktop-local; unmatched/errors go to manager):
  relay policy list
  relay policy check --kind KIND [--source RAW_KIND] [--agent NAME] [--host HOST] [--text TEXT] [--command CMD]
  relay policy add ID --kind KIND [--source RAW_KIND] [--agent NAME] [--host HOST] [--contains TEXT ...] (--reply TEXT | --ack)
  relay policy remove ID

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
  relay pane list                     Session-keyed pane, workspace, parent, and liveness inventory
  relay pane rename SESSION_ID NAME   Set a durable display alias; tmux identity is unchanged
  relay viz present SESSION_ID [--workspace WS] [--pane PANE] [--tab]
                                      First child splits right; later siblings stack downward.
                                      --tab stacks in PANE; explicit placement overrides defaults.
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

func (a *App) cmdNamed(ctx context.Context, host, name string) int {
	if strings.HasPrefix(host, "-") || strings.HasPrefix(name, "-") {
		return a.fail(fmt.Errorf("usage: relay HOST NAME"))
	}
	sourceID, sourceHost, sourcePersist, sourceRepo := sourceFromEnvironment(a.Reg)
	opts := core.CreateOpts{
		HostID:            host,
		Name:              name,
		Labels:            map[string]string{"role": "interactive", "agent": "human"},
		SourceSessionID:   sourceID,
		SourceHostID:      sourceHost,
		SourcePersistName: sourcePersist,
	}
	if sourceRepo != "" {
		opts.RepoRef = sourceRepo
	} else if root, err := findGitRoot(""); err == nil {
		opts.RepoRef = root
	} else {
		opts.RemoteCWD = "~"
	}
	// Remember the registered identity before OpenNamed probes tmux. If the
	// remote session died, OpenNamed replaces that registry record; its old
	// cmux binding must not remain as a duplicate of the replacement pane.
	var previousIDs []string
	if sessions, listErr := a.Reg.ListSessions(); listErr == nil {
		for _, candidate := range sessions {
			if candidate.HostID == host && candidate.Persist.Name == name {
				previousIDs = append(previousIDs, candidate.ID)
			}
		}
	}
	sess, created, err := a.Sessions.OpenNamed(ctx, opts)
	if err != nil {
		if errors.Is(err, core.ErrMissingProfile) {
			return a.failNext(err, map[string]any{
				"reason": "missing_host_profile", "host_id": host,
				"next": "host init", "argv": []string{"relay", "host", "init", "-H", host, "--apply"},
			})
		}
		return a.fail(err)
	}
	if forgetter, ok := a.Viz.(interface{ ForgetBinding(string) error }); ok {
		for _, previousID := range previousIDs {
			if previousID != sess.ID {
				_ = forgetter.ForgetBinding(previousID)
			}
		}
	}
	if sourceID != "" && sourceID != sess.ID && !created {
		_ = core.AppendRelayEdge(sourceID, sess.ID)
	}
	if sess.Labels["adopted"] == "existing" {
		ui.Warn("existing tmux adopted without relay bridge identity; use a new NAME for remote-to-remote relay commands")
	}
	launch := core.ResumeLaunchCmd(sess.Persist.Name)
	if os.Getenv(bridge.LocalInvokeEnv) == "1" {
		if a.Viz == nil || !a.Viz.Available(ctx) {
			return a.fail(fmt.Errorf("cmux unavailable on desktop bridge"))
		}
		workspace, pane := a.locationForSource(sourceID)
		ref, err := core.PresentSession(ctx, a.Viz, sess, launch, ports.Layout{
			Mode: "remote", Workspace: workspace, Pane: pane, SourceSessionID: sourceID,
		})
		if err != nil {
			return a.fail(err)
		}
		sess.VizSurfaceRef = ref
		_ = a.Reg.PutSession(sess)
		core.RememberResume(sess)
		core.RememberPane(ref, sess, true)
		_ = a.applySessionChrome(ctx, sess)
		_ = a.brandAll(ctx)
		a.JSON = true
		return a.errOut(a.out(map[string]any{
			"ok": true, "created": created, "session": sess, "surface": ref,
			"source_session_id": sourceID,
		}))
	}
	if binder, ok := a.Viz.(interface {
		BindCurrent(context.Context, string, string) (string, error)
	}); ok && a.Viz.Available(ctx) {
		if ref, bindErr := binder.BindCurrent(ctx, sess.ID, launch); bindErr == nil {
			sess.VizSurfaceRef = ref
			_ = a.Reg.PutSession(sess)
			core.RememberPane(ref, sess, true)
		} else if bindErr != nil {
			ui.Warn("cmux current-pane binding failed: " + bindErr.Error())
		}
	}
	_ = a.applySessionChrome(ctx, sess)
	_ = a.brandAll(ctx)
	return a.cmdResume(ctx, []string{"--session", sess.Persist.Name})
}

func (a *App) cmdHistory(args []string) int {
	if err := requireNoExtra(args); err != nil {
		return a.fail(err)
	}
	graph, err := core.LoadHistory()
	if err != nil {
		return a.fail(err)
	}
	if a.JSON {
		return a.errOut(a.out(map[string]any{"ok": true, "history": graph}))
	}
	fmt.Print(core.FormatHistory(graph))
	return 0
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
	case "rename":
		if len(args) != 3 {
			return a.fail(fmt.Errorf("usage: relay session rename ID NAME"))
		}
		sess, err := a.Sessions.Rename(ctx, args[1], args[2])
		if err != nil {
			return a.fail(err)
		}
		rebound := false
		if a.Viz != nil && a.Viz.Available(ctx) && sess.VizSurfaceRef != "" {
			if rebinder, ok := a.Viz.(interface {
				RebindRenamedSession(context.Context, *core.Session, string) error
			}); ok {
				if err := rebinder.RebindRenamedSession(ctx, sess, core.ResumeLaunchCmd(sess.Persist.Name)); err != nil {
					return a.fail(fmt.Errorf("tmux renamed to %q, but cmux rebind failed: %w", sess.Persist.Name, err))
				}
				rebound = true
			}
		}
		if err := a.brandAll(ctx); err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{
			"ok": true, "session_id": sess.ID, "persist_name": sess.Persist.Name,
			"display_name": core.SessionDisplayName(sess), "cmux_rebound": rebound,
		}))
	case "bridge":
		if len(args) != 2 {
			return a.fail(fmt.Errorf("usage: relay session bridge ID"))
		}
		sess, err := a.Sessions.ProvisionBridge(ctx, args[1])
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "session_id": sess.ID, "host_id": sess.HostID, "persist_name": sess.Persist.Name, "bridge": "provisioned"}))
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
				opts.ExplicitPlace = true
			}
		case "--pane":
			i++
			if i < len(rest) {
				opts.Pane = rest[i]
				opts.ExplicitPlace = true
			}
		case "--parent":
			i++
			if i < len(rest) {
				opts.SourceSessionID = rest[i]
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
	sourceRepo, sourceErr := a.applyHandoffSource(ctx, &opts)
	if sourceErr != nil {
		return a.fail(sourceErr)
	}
	if opts.RepoRef == "" && opts.RemoteCWD == "" {
		if sourceRepo != "" {
			opts.RepoRef = sourceRepo
		} else if root, err := findGitRoot(""); err == nil {
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
	if b != nil && b.SourceSessionID != "" {
		a.startParentWatcher(b.HandoffID)
	}
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
	a.CompactJSON = true
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
			return a.errOut(a.out(map[string]any{"ok": true, "timed_out": true, "next": "wait"}))
		}
		return a.errOut(a.out(map[string]any{"ok": true, "timed_out": false, "message": m, "next_from": m.Seq}))
	default:
		return a.fail(fmt.Errorf("unknown msg subcommand %q", sub))
	}
}

// cmdParent owns local-parent registration, the durable compact inbox, and
// guarded retirement. It is JSON-first so hooks and agents never parse prose.
func (a *App) cmdParent(ctx context.Context, args []string) int {
	a.JSON = true
	a.CompactJSON = true
	if a.Parents == nil || len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay parent register|link|list|inbox|sweep|reply|ack|state|status|retire …"))
	}
	switch args[0] {
	case "register":
		var opts core.RegisterParentOpts
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--surface":
				i++
				if i < len(args) {
					opts.Surface = args[i]
				}
			case "--name":
				i++
				if i < len(args) {
					opts.Name = args[i]
				}
			case "--repo":
				i++
				if i < len(args) {
					opts.RepoRefs = append(opts.RepoRefs, args[i])
				}
			case "--wake":
				i++
				if i < len(args) {
					opts.WakeMode = args[i]
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		sess, created, err := a.Parents.RegisterLocal(ctx, opts)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "created": created, "session": sess}))
	case "bind":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay parent bind PARENT [--surface REF]"))
		}
		parentID, surface := args[1], ""
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--surface":
				i++
				if i < len(args) {
					surface = args[i]
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		sess, err := a.Parents.BindLocal(ctx, parentID, surface)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "parent_session_id": sess.ID, "surface": sess.VizSurfaceRef, "state": sess.Labels["parent_state"]}))
	case "link":
		if len(args) != 3 {
			return a.fail(fmt.Errorf("usage: relay parent link PARENT HANDOFF"))
		}
		parentID, handoffID := args[1], args[2]
		ho, err := a.Parents.LinkChild(parentID, handoffID)
		if err != nil {
			return a.fail(err)
		}
		a.startParentWatcher(ho.ID)
		return a.errOut(a.out(map[string]any{
			"ok": true, "parent_session_id": parentID,
			"handoff_id": ho.ID, "child_session_id": ho.SessionID,
		}))
	case "move", "reparent":
		if len(args) != 3 {
			return a.fail(fmt.Errorf("usage: relay parent move PARENT HANDOFF"))
		}
		parentID, handoffID := args[1], args[2]
		ho, oldParentID, err := a.Parents.ReparentChild(parentID, handoffID)
		if err != nil {
			return a.fail(err)
		}
		a.ensureParentWatcher(ho.ID)
		return a.errOut(a.out(map[string]any{"ok": true, "handoff_id": ho.ID, "child_session_id": ho.SessionID, "old_parent_session_id": oldParentID, "parent_session_id": parentID}))
	case "list":
		list, err := a.Reg.ListSessions()
		if err != nil {
			return a.fail(err)
		}
		out := make([]*core.Session, 0)
		for _, sess := range list {
			if sess.HostID == core.LocalHostID && sess.Persist.Kind == core.LocalPersistKind && sess.Labels["role"] == core.ParentRole {
				out = append(out, sess)
			}
		}
		return a.errOut(a.out(map[string]any{"ok": true, "parents": out}))
	case "inbox":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay parent inbox PARENT [--all]"))
		}
		parentID := args[1]
		all := false
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--all":
				all = true
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if err := authorizeParentCaller(parentID); err != nil {
			return a.fail(err)
		}
		msgs, err := a.Parents.ListMessages(parentID, !all)
		if err != nil {
			return a.fail(err)
		}
		items := make([]core.ParentInboxItem, 0, len(msgs))
		for _, msg := range msgs {
			items = append(items, core.CompactParentMessage(msg, all))
		}
		return a.errOut(a.out(map[string]any{"ok": true, "parent_session_id": parentID, "messages": items, "count": len(items)}))
	case "log":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay parent log PARENT [--after CURSOR] [--limit N] [--handoff ID]"))
		}
		parentID, handoffID := args[1], ""
		var after int64
		limit := 20
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--after":
				i++
				if i >= len(args) {
					return a.fail(fmt.Errorf("--after requires a cursor"))
				}
				var err error
				after, err = strconv.ParseInt(args[i], 10, 64)
				if err != nil || after < 0 {
					return a.fail(fmt.Errorf("invalid --after cursor %q", args[i]))
				}
			case "--limit":
				i++
				if i >= len(args) {
					return a.fail(fmt.Errorf("--limit requires a number"))
				}
				var err error
				limit, err = strconv.Atoi(args[i])
				if err != nil || limit < 1 || limit > 100 {
					return a.fail(fmt.Errorf("--limit must be between 1 and 100"))
				}
			case "--handoff":
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "-") {
					return a.fail(fmt.Errorf("--handoff requires an ID"))
				}
				handoffID = args[i]
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if err := authorizeParentCaller(parentID); err != nil {
			return a.fail(err)
		}
		page, err := core.LoadCommunicationPage(parentID, handoffID, after, limit)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "parent_session_id": parentID, "log": page}))
	case "sweep":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay parent sweep PARENT"))
		}
		parentID := args[1]
		if err := authorizeParentCaller(parentID); err != nil {
			return a.fail(err)
		}
		acked, byHandoff, err := a.Parents.SweepTerminal(parentID)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "parent_session_id": parentID, "acked": acked, "by_handoff": byHandoff}))
	case "reply":
		messageID, text := parentMessageArgs(args[1:])
		if messageID == "" || text == "" {
			return a.fail(fmt.Errorf("usage: relay parent reply MESSAGE [--] TEXT"))
		}
		candidate, err := a.Parents.FindMessage(messageID)
		if err != nil {
			return a.fail(err)
		}
		if err := authorizeParentCaller(candidate.ParentSessionID); err != nil {
			return a.fail(err)
		}
		msg, err := a.Parents.Reply(ctx, messageID, text)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "message_id": msg.ID, "state": msg.State, "correlation_id": msg.CorrelationID}))
	case "ack":
		messageID, _ := parentMessageArgs(args[1:])
		if messageID == "" {
			return a.fail(fmt.Errorf("usage: relay parent ack MESSAGE"))
		}
		if len(args) != 2 {
			return a.fail(fmt.Errorf("usage: relay parent ack MESSAGE"))
		}
		candidate, err := a.Parents.FindMessage(messageID)
		if err != nil {
			return a.fail(err)
		}
		if err := authorizeParentCaller(candidate.ParentSessionID); err != nil {
			return a.fail(err)
		}
		msg, err := a.Parents.Ack(messageID)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "message_id": msg.ID, "state": msg.State}))
	case "state", "active", "idle", "complete":
		state := args[0]
		if state == "state" {
			if len(args) != 3 {
				return a.fail(fmt.Errorf("usage: relay parent state PARENT active|idle|complete"))
			}
			state = args[2]
		} else if len(args) != 2 {
			return a.fail(fmt.Errorf("usage: relay parent %s PARENT", state))
		}
		sessionID := args[1]
		sess, err := a.Parents.SetState(sessionID, state)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "session_id": sess.ID, "state": state}))
	case "status", "retire":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay parent %s PARENT [--dry-run]", args[0]))
		}
		sessionID := args[1]
		dryRun := args[0] == "status"
		force, keepViz := false, false
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--dry-run":
				dryRun = true
			case "--force":
				force = true
			case "--keep-viz":
				keepViz = true
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if args[0] == "status" {
			if err := authorizeParentCaller(sessionID); err != nil {
				return a.fail(err)
			}
		}
		gate, err := a.Parents.Retire(ctx, sessionID, dryRun, force, keepViz)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "retirement": gate}))
	case "watch":
		if len(args) < 2 || len(args) > 3 || strings.HasPrefix(args[1], "-") || (len(args) == 3 && args[2] != "--detach") {
			return a.fail(fmt.Errorf("usage: relay parent watch HANDOFF [--detach]"))
		}
		handoffID := args[1]
		if len(args) == 3 {
			a.startParentWatcher(handoffID)
			return a.errOut(a.out(map[string]any{"ok": true, "handoff_id": handoffID, "watcher": "started"}))
		}
		if err := a.Parents.Watch(ctx, handoffID); err != nil && ctx.Err() == nil {
			return a.fail(err)
		}
		return 0
	default:
		return a.fail(fmt.Errorf("unknown parent subcommand %q", args[0]))
	}
}

// cmdResolve is the sole agent-facing response operation. A question needs
// one decision; informational child events acknowledge themselves on delivery.
func (a *App) cmdResolve(ctx context.Context, args []string) int {
	a.JSON = true
	a.CompactJSON = true
	messageID, decision := parentMessageArgs(args)
	if messageID == "" || decision == "" {
		return a.fail(fmt.Errorf("usage: relay resolve MESSAGE [--] DECISION"))
	}
	msg, err := a.Parents.FindMessage(messageID)
	if err != nil {
		return a.fail(err)
	}
	if err := authorizeParentCaller(msg.ParentSessionID); err != nil {
		return a.fail(err)
	}
	resolved, err := a.Parents.Reply(ctx, messageID, decision)
	if err != nil {
		return a.fail(err)
	}
	return a.errOut(a.out(map[string]any{"ok": true, "state": resolved.State}))
}

// cmdCommunicationLog infers the manager identity from the authenticated
// bridge source. Agents carry only a numeric cursor, never a parent session ID.
func (a *App) cmdCommunicationLog(args []string) int {
	a.JSON = true
	a.CompactJSON = true
	if len(args) > 1 {
		return a.fail(fmt.Errorf("usage: relay log [CURSOR]"))
	}
	var after int64
	if len(args) == 1 {
		var err error
		after, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil || after < 0 {
			return a.fail(fmt.Errorf("invalid cursor %q", args[0]))
		}
	}
	parentID, err := a.currentParentID()
	if err != nil {
		return a.fail(err)
	}
	page, err := core.LoadCommunicationPage(parentID, "", after, 20)
	if err != nil {
		return a.fail(err)
	}
	return a.errOut(a.out(map[string]any{"events": page.Entries, "next": page.NextAfter, "more": page.HasMore}))
}

func (a *App) currentParentID() (string, error) {
	for _, key := range []string{bridge.SourceSessionEnv, "RELAY_SESSION_ID"} {
		if id := strings.TrimSpace(os.Getenv(key)); id != "" {
			if err := authorizeParentCaller(id); err != nil {
				return "", err
			}
			return id, nil
		}
	}
	surface, err := core.CurrentSurface()
	if err != nil {
		return "", fmt.Errorf("cannot infer manager session: %w", err)
	}
	sessions, err := a.Reg.ListSessions()
	if err != nil {
		return "", err
	}
	for _, session := range sessions {
		if session.VizSurfaceRef == surface && session.Labels["role"] == core.ParentRole {
			return session.ID, nil
		}
	}
	return "", fmt.Errorf("current pane %s is not a Relay manager", surface)
}

func (a *App) cmdPolicy(args []string) int {
	a.JSON = true
	a.CompactJSON = true
	if a.Policies == nil {
		return a.fail(fmt.Errorf("policy service unavailable"))
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return a.fail(fmt.Errorf("usage: relay policy list"))
		}
		path, builtins, cfg, err := a.Policies.Describe()
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "path": path, "builtins": builtins, "rules": cfg.Rules}))
	case "remove", "rm":
		if len(args) != 2 {
			return a.fail(fmt.Errorf("usage: relay policy remove ID"))
		}
		if err := a.Policies.Remove(args[1]); err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "removed": args[1]}))
	case "add":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay policy add ID --kind KIND [guards] (--reply TEXT | --ack)"))
		}
		rule := core.PolicyRule{ID: args[1]}
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--kind":
				i++
				if i < len(args) {
					rule.Kind = args[i]
				}
			case "--source":
				i++
				if i < len(args) {
					rule.SourceKind = args[i]
				}
			case "--agent":
				i++
				if i < len(args) {
					rule.Agent = args[i]
				}
			case "--host":
				i++
				if i < len(args) {
					rule.Host = args[i]
				}
			case "--contains":
				i++
				if i < len(args) {
					rule.Contains = append(rule.Contains, args[i])
				}
			case "--seen":
				i++
				if i < len(args) {
					rule.SeenKind = args[i]
				}
			case "--pending":
				i++
				if i < len(args) {
					rule.PendingKind = args[i]
				}
			case "--reply":
				i++
				if i < len(args) {
					rule.Action, rule.Reply = "reply", args[i]
				}
			case "--ack":
				rule.Action = "ack"
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if err := a.Policies.Add(rule); err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "rule": rule}))
	case "check":
		ctx := core.PolicyContext{SeenKinds: map[string]bool{}, PendingKinds: map[string]bool{}}
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--kind":
				i++
				if i < len(args) {
					ctx.Kind = args[i]
				}
			case "--source":
				i++
				if i < len(args) {
					ctx.SourceKind = args[i]
				}
			case "--agent":
				i++
				if i < len(args) {
					ctx.Agent = args[i]
				}
			case "--host":
				i++
				if i < len(args) {
					ctx.Host = args[i]
				}
			case "--text":
				i++
				if i < len(args) {
					ctx.Text = args[i]
				}
			case "--command":
				i++
				if i < len(args) {
					ctx.Command = args[i]
				}
			case "--seen":
				i++
				if i < len(args) {
					ctx.SeenKinds[args[i]] = true
				}
			case "--pending":
				i++
				if i < len(args) {
					ctx.PendingKinds[args[i]] = true
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if ctx.Kind == "" {
			return a.fail(fmt.Errorf("--kind required"))
		}
		decision, err := a.Policies.Decide(ctx)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "decision": decision}))
	default:
		return a.fail(fmt.Errorf("usage: relay policy list|check|add|remove"))
	}
}

// A bridge caller may act only as its authenticated source session. This lets
// a remote parent orchestrate its own long-lived goal tree without exposing a
// different local parent's durable inbox by guessed identifiers.
func authorizeParentCaller(parentID string) error {
	caller := strings.TrimSpace(os.Getenv(bridge.SourceSessionEnv))
	if caller != "" && caller != parentID {
		return fmt.Errorf("parent session %s is outside authenticated caller scope", parentID)
	}
	return nil
}

// cmdRoot owns the apex lifecycle: which session governs, which roots are
// enrolled under it, where their rules live, and what it decided while the
// human was away. Relay stays model-free — the judgment lives in the apex
// agent (share/roles/relay-conductor.md), not here.
func (a *App) cmdRoot(ctx context.Context, args []string) int {
	a.JSON = true
	a.CompactJSON = true
	if len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay root adopt|release|enroll|unenroll|status|rules|digest …"))
	}
	// The digest is the human's decision queue and names every governed
	// subtree, so it must never be readable by an arbitrary session. The bridge
	// allowlist already keeps `root` desktop-only; this is the second lock, so
	// the guarantee does not depend on that list staying correct.
	if caller := strings.TrimSpace(os.Getenv(bridge.SourceSessionEnv)); caller != "" {
		apex, err := a.Roots.Apex()
		if err != nil || apex.ID != caller {
			return a.fail(fmt.Errorf("relay root is outside authenticated caller scope"))
		}
	}
	sub, rest := args[0], args[1:]
	positional := ""
	after := int64(0)
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--after":
			i++
			if i < len(rest) {
				after, _ = strconv.ParseInt(rest[i], 10, 64)
			}
		default:
			if strings.HasPrefix(rest[i], "-") {
				return a.fail(fmt.Errorf("unknown flag %q", rest[i]))
			}
			positional = rest[i]
		}
	}
	switch sub {
	case "adopt":
		if positional == "" {
			return a.fail(fmt.Errorf("usage: relay root adopt SESSION"))
		}
		sess, err := a.Roots.Adopt(positional)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "apex": sess.ID}))
	case "release":
		if positional == "" {
			return a.fail(fmt.Errorf("usage: relay root release SESSION"))
		}
		sess, err := a.Roots.Release(positional)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "released": sess.ID}))
	case "enroll", "unenroll":
		if positional == "" {
			return a.fail(fmt.Errorf("usage: relay root %s SESSION", sub))
		}
		act := a.Roots.Enroll
		if sub == "unenroll" {
			act = a.Roots.Unenroll
		}
		sess, err := act(positional)
		if err != nil {
			return a.fail(err)
		}
		out := map[string]any{"ok": true, "session_id": sess.ID, "governed": sub == "enroll"}
		if sub == "enroll" {
			// Never let an enroll imply autonomy the deployment cannot deliver.
			cp := core.DescribeControlPlane()
			out["control_plane"] = cp
			if cp.Warning != "" {
				out["warning"] = cp.Warning
			}
		}
		return a.errOut(a.out(out))
	case "status":
		apex, err := a.Roots.Apex()
		if err != nil {
			return a.fail(err)
		}
		governed, err := a.Roots.Governed()
		if err != nil {
			return a.fail(err)
		}
		ids := make([]string, 0, len(governed))
		for _, sess := range governed {
			ids = append(ids, sess.ID)
		}
		// Report whether the apex agent is actually working. A configured but
		// inert apex is indistinguishable from a healthy one otherwise.
		readiness := a.Roots.AgentReadinessFor(ctx, a.Sessions, apex.ID)
		out := map[string]any{
			"ok": true, "apex": apex.ID, "governed": ids,
			"agent":         readiness,
			"control_plane": core.DescribeControlPlane(),
		}
		if readiness.State != core.AgentReady {
			out["ok"] = false
		}
		return a.errOut(a.out(out))
	case "rules":
		if positional == "" {
			return a.fail(fmt.Errorf("usage: relay root rules PROJECT"))
		}
		path, err := core.RulesPath(positional)
		if err != nil {
			return a.fail(err)
		}
		_, statErr := os.Stat(path)
		return a.errOut(a.out(map[string]any{"ok": true, "path": path, "exists": statErr == nil}))
	case "digest":
		digest, err := a.Roots.Digest(a.Parents, after)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(digest))
	default:
		return a.fail(fmt.Errorf("usage: relay root adopt|release|enroll|unenroll|status|rules|digest …"))
	}
}

// boardCaller resolves which session is acting. A bridge-authenticated agent
// always acts as itself: the identity comes from its authenticated envelope,
// never from an argument, so a peer cannot post or query as someone else.
// --session is accepted only outside a bridge context, for local operators.
func boardCaller(explicit string) (string, error) {
	authenticated := strings.TrimSpace(os.Getenv(bridge.SourceSessionEnv))
	if authenticated != "" {
		if explicit != "" && explicit != authenticated {
			return "", fmt.Errorf("session %s is outside authenticated caller scope", explicit)
		}
		return authenticated, nil
	}
	if explicit == "" {
		return "", fmt.Errorf("--session required outside a relay-managed pane")
	}
	return explicit, nil
}

// cmdBoard exposes the manager-scoped peer board. Output is compact machine
// JSON: a query returns current state only, never history.
func (a *App) cmdBoard(ctx context.Context, args []string) int {
	a.JSON = true
	a.CompactJSON = true
	if len(args) == 0 {
		return a.fail(fmt.Errorf("usage: relay board post|query|watch …"))
	}
	sub, rest := args[0], args[1:]
	var session, category, key, text string
	var fromSeq int64
	var fromSet bool
	var subtree bool
	timeoutSec := 120
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--subtree":
			subtree = true
		case "--session", "-s":
			i++
			if i < len(rest) {
				session = rest[i]
			}
		case "--category", "-c":
			i++
			if i < len(rest) {
				category = rest[i]
			}
		case "--key", "-k":
			i++
			if i < len(rest) {
				key = rest[i]
			}
		case "--from":
			i++
			if i < len(rest) {
				fromSeq, _ = strconv.ParseInt(rest[i], 10, 64)
				fromSet = true
			}
		case "--timeout":
			i++
			if i < len(rest) {
				timeoutSec, _ = strconv.Atoi(rest[i])
			}
		case "--":
			text = strings.Join(rest[i+1:], " ")
			i = len(rest)
		default:
			if strings.HasPrefix(rest[i], "-") {
				return a.fail(fmt.Errorf("unknown flag %q", rest[i]))
			}
		}
	}
	caller, err := boardCaller(session)
	if err != nil {
		return a.fail(err)
	}
	switch sub {
	case "post":
		if text == "" {
			return a.fail(fmt.Errorf("usage: relay board post -c CATEGORY [-k KEY] -- TEXT"))
		}
		seq, err := a.Boards.Post(ctx, caller, category, key, text)
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "seq": seq}))
	case "query":
		query := func() ([]core.BoardEntry, error) {
			if subtree {
				// One call for the whole subtree instead of one per level.
				return a.Boards.QuerySubtree(ctx, caller, category, key)
			}
			return a.Boards.Query(ctx, caller, category, key, true)
		}
		entries, err := query()
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{"ok": true, "entries": entries, "count": len(entries)}))
	case "watch":
		if !fromSet {
			fromSeq, err = a.Boards.CurrentSeq(ctx, caller, category)
			if err != nil {
				return a.fail(err)
			}
		}
		entry, timedOut, err := a.Boards.Watch(ctx, caller, category, fromSeq, time.Duration(timeoutSec)*time.Second)
		if err != nil {
			return a.fail(err)
		}
		if timedOut {
			return a.errOut(a.out(map[string]any{"ok": true, "timeout": true}))
		}
		return a.errOut(a.out(map[string]any{"ok": true, "entry": entry}))
	default:
		return a.fail(fmt.Errorf("usage: relay board post|query|watch …"))
	}
}

func parentMessageArgs(args []string) (messageID, text string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", ""
	}
	messageID = args[0]
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return messageID, strings.Join(rest, " ")
}

// cmdSupervise keeps exactly one watcher running per live handoff.
//
// Watcher lifecycle used to belong to install.sh, which meant a watcher that
// died for any other reason stayed dead and its handoff silently stopped
// routing escalations. This makes it relay's job.
func (a *App) cmdSupervise(ctx context.Context, args []string) int {
	a.JSON = true
	a.CompactJSON = true
	check := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check":
			check = true
		default:
			return a.fail(rejectUnknownFlag(args[i]))
		}
	}
	sup := &core.SupervisorService{
		Reg:     a.Reg,
		Parents: a.Parents,
		OnEvent: func(event, handoffID string, err error) {
			payload := map[string]any{"event": event}
			if handoffID != "" {
				payload["handoff_id"] = handoffID
			}
			if err != nil {
				payload["error"] = err.Error()
			}
			_ = a.out(payload)
		},
	}
	if check {
		// Report only. Reconcile would start goroutines this process then kills
		// on exit, which looks like success and supervises nothing.
		sup.OnEvent = nil
		unwatched, err := sup.Unwatched()
		if err != nil {
			return a.fail(err)
		}
		ids := make([]string, 0, len(unwatched))
		for _, ho := range unwatched {
			ids = append(ids, ho.ID)
		}
		live, _ := sup.NeedsWatch()
		code := 0
		if len(ids) > 0 {
			code = 1
		}
		_ = a.out(map[string]any{
			"ok": len(ids) == 0, "live": len(live), "unwatched": ids,
		})
		return code
	}
	if err := sup.Run(ctx); err != nil && ctx.Err() == nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) startParentWatcher(handoffID string) {
	if handoffID == "" || os.Getenv("RELAY_NO_PARENT_WATCH") == "1" {
		return
	}
	bin, err := os.Executable()
	if err != nil {
		return
	}
	if err := core.EnsureStateDirs(); err != nil {
		return
	}
	logPath := filepath.Join(core.ParentWatchDir(), handoffID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	cmd := exec.Command(bin, "--json", "parent", "watch", handoffID)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
	_ = logFile.Close()
}

func (a *App) ensureParentWatcher(handoffID string) {
	raw, err := os.ReadFile(core.ParentWatchLockPath(handoffID))
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err == nil && parseErr == nil && pid > 1 && pid != os.Getpid() {
		command, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		cmdline := string(command)
		if strings.Contains(cmdline, "relay") && strings.Contains(cmdline, "parent watch") && strings.Contains(cmdline, handoffID) {
			// Watchers reload the handoff record before routing each event, so
			// an existing watcher automatically observes a repaired parent edge.
			return
		}
	}
	a.startParentWatcher(handoffID)
}

// cmdSignal is the agent-neutral hook surface. It intentionally executes on
// the child host instead of crossing the desktop bridge.
func (a *App) cmdSignal(ctx context.Context, mode string, args []string) int {
	a.JSON = true
	a.CompactJSON = true
	var kind, text, correlation string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--kind":
			i++
			if i < len(args) {
				kind = args[i]
			}
		case "--text":
			i++
			if i < len(args) {
				text = args[i]
			}
		case "--correlation", "--request":
			i++
			if i < len(args) {
				correlation = args[i]
			}
		default:
			if kind == "" && !strings.HasPrefix(args[i], "-") {
				kind = args[i]
			} else {
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
	}
	meta := map[string]any{}
	if mode == "hook" {
		if stat, err := os.Stdin.Stat(); err == nil && stat.Mode()&os.ModeCharDevice == 0 {
			raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
			var payload map[string]any
			if json.Unmarshal(raw, &payload) == nil {
				for _, key := range []string{"reason", "tool_name", "permission_mode", "cwd"} {
					if value, ok := payload[key].(string); ok && value != "" {
						meta[key] = compactHookField(value, 512)
					}
				}
				if text == "" {
					for _, key := range []string{"message", "reason", "last_assistant_message"} {
						if value, ok := payload[key].(string); ok && value != "" {
							text = compactHookField(value, 640)
							break
						}
					}
				}
				command, _ := payload["command"].(string)
				if command == "" {
					if toolInput, ok := payload["tool_input"].(map[string]any); ok {
						command, _ = toolInput["command"].(string)
					}
				}
				if command != "" {
					meta["command"] = compactHookField(command, 2048)
				}
			}
		}
	}
	if kind == "" {
		return a.fail(fmt.Errorf("signal kind required"))
	}
	session := strings.TrimSpace(os.Getenv("RELAY_SESSION_NAME"))
	if session == "" {
		return a.fail(fmt.Errorf("RELAY_SESSION_NAME is not set"))
	}
	if text != "" {
		meta["text"] = compactHookField(text, 640)
	}
	if correlation != "" {
		meta["correlation_id"] = correlation
	}
	metaRaw, _ := json.Marshal(meta)
	relaydBin := filepath.Join(filepath.Dir(core.RelayBin()), "relayd")
	if _, err := os.Stat(relaydBin); err != nil {
		relaydBin, err = exec.LookPath("relayd")
		if err != nil {
			return a.fail(fmt.Errorf("relayd not installed"))
		}
	}
	cmdArgs := []string{"emit", "-s", session, "--kind", kind}
	if len(meta) > 0 {
		cmdArgs = append(cmdArgs, "--meta", string(metaRaw))
	}
	out, err := exec.CommandContext(ctx, relaydBin, cmdArgs...).Output()
	if err != nil {
		return a.fail(err)
	}
	var response map[string]any
	if json.Unmarshal(out, &response) != nil {
		return a.fail(fmt.Errorf("invalid relayd response"))
	}
	if mode == "hook" {
		return 0
	}
	response["kind"], response["session"] = kind, session
	return a.errOut(a.out(response))
}

func (a *App) cmdAsk(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	question := strings.TrimSpace(strings.Join(args, " "))
	if question == "" {
		return a.fail(fmt.Errorf("usage: relay ask [--] QUESTION"))
	}
	return a.cmdSignal(ctx, "signal", []string{"ask", "--text", question})
}

func compactHookField(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		value = string(runes[:limit-1]) + "…"
	}
	return value
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
	a.CompactJSON = true
	if len(args) == 0 {
		args = []string{"protocol"}
	}
	switch args[0] {
	case "protocol", "help", "--help", "-h":
		return a.errOut(a.out(map[string]any{
			"ok":      true,
			"v":       2,
			"purpose": "long-lived goal handoff and orchestration",
			"start":   []string{"relay", "agent", "start", "HOST", "AGENT", "--", "GOAL"},
			"resolve": []string{"relay", "resolve", "MESSAGE", "--", "DECISION"},
			"log":     []string{"relay", "log", "CURSOR"},
			"board":   []string{"relay", "board", "query", "-c", "CATEGORY"},
			"rules": []string{
				"managed start has no follow-up; hooks wake manager",
				"run argv only when returned; wait timeout means stop",
				"dead parent->ancestor; root->human",
				"blocked: relay ask QUESTION; security gates stop",
				"log is optional cursor delta; no transcripts/polling",
				"board=peer state; post -k KEY -- TEXT; query folds latest",
			},
		}))
	case "pick":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent pick HOST"))
		}
		host := args[1]
		profile, err := a.Profiles.Get(ctx, host, true)
		if err != nil {
			return a.fail(err)
		}
		picked, ranking := core.Suggest(ctx, profile)
		return a.errOut(a.out(map[string]any{
			"ok": true, "host_id": host, "picked": picked, "ranking": ranking,
		}))
	case "start":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent start HOST AGENT [options] -- GOAL | HOST --cmd CMD [options]"))
		}
		host, rest := args[1], args[2:]
		opts := core.HandoffOpts{HostID: host}
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			opts.Agent = rest[0]
			rest = rest[1:]
		}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
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
					opts.ExplicitPlace = true
				}
			case "--pane":
				i++
				if i < len(rest) {
					opts.Pane = rest[i]
					opts.ExplicitPlace = true
				}
			case "--container":
				i++
				if i < len(rest) {
					opts.Container = rest[i]
				}
			case "--parent":
				i++
				if i < len(rest) {
					opts.SourceSessionID = rest[i]
				}
			case "--no-pane":
				opts.NoPane = true
			case "--silence":
				i++
				if i < len(rest) {
					opts.Silence, _ = strconv.Atoi(rest[i])
				}
			case "--":
				opts.Goal = strings.Join(rest[i+1:], " ")
				i = len(rest)
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if opts.Command == "" && (opts.Agent == "" || opts.Goal == "") {
			return a.fail(fmt.Errorf("usage: relay agent start HOST AGENT [options] -- GOAL | HOST --cmd CMD [options]"))
		}
		if opts.Command != "" && (opts.Agent != "" || opts.Goal != "") {
			return a.fail(fmt.Errorf("choose an agent goal or --cmd, not both"))
		}
		sourceRepo, sourceErr := a.applyHandoffSource(ctx, &opts)
		if sourceErr != nil {
			return a.fail(sourceErr)
		}
		if opts.RepoRef == "" && opts.RemoteCWD == "" {
			if sourceRepo != "" {
				opts.RepoRef = sourceRepo
			} else if root, err := findGitRoot(""); err == nil {
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
		if resp != nil && opts.SourceSessionID != "" {
			a.startParentWatcher(resp.HandoffID)
		}
		return 0
	case "restart":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent restart HANDOFF [--repo DIR] [--cwd REMOTE] [--name NAME] [--no-pane]"))
		}
		if err := a.authorizeAgentHandoff(ctx, args[1]); err != nil {
			return a.fail(err)
		}
		opts, err := a.Handoffs.AgentRestartOptions(args[1])
		if err != nil {
			return a.fail(err)
		}
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--repo":
				i++
				if i < len(args) {
					opts.RepoRef, opts.RemoteCWD = args[i], ""
				}
			case "--cwd", "-R":
				i++
				if i < len(args) {
					opts.RemoteCWD, opts.RepoRef = args[i], ""
				}
			case "--name", "-s":
				i++
				if i < len(args) {
					opts.Name = args[i]
				}
			case "--no-pane":
				opts.NoPane = true
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		if _, err := a.applyHandoffSource(ctx, &opts); err != nil {
			return a.fail(err)
		}
		resp, err := a.Handoffs.AgentStart(ctx, opts)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		if resp != nil && opts.SourceSessionID != "" {
			a.startParentWatcher(resp.HandoffID)
		}
		return 0
	case "wait":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent wait HANDOFF [--from SEQ] [--timeout SEC]"))
		}
		handoffID := args[1]
		if err := a.authorizeAgentHandoff(ctx, handoffID); err != nil {
			return a.fail(err)
		}
		var from int64
		timeoutSec := 120
		for i := 2; i < len(args); i++ {
			switch args[i] {
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
		resp, err := a.Handoffs.AgentWait(ctx, handoffID, from, time.Duration(timeoutSec)*time.Second)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil && (resp == nil || !resp.OK) {
			return 1
		}
		return 0
	case "send":
		if len(args) < 3 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent send HANDOFF [--] TEXT"))
		}
		handoffID, rest := args[1], args[2:]
		if err := a.authorizeAgentHandoff(ctx, handoffID); err != nil {
			return a.fail(err)
		}
		if rest[0] == "--" {
			rest = rest[1:]
		}
		text := strings.Join(rest, " ")
		if text == "" {
			return a.fail(fmt.Errorf("usage: relay agent send HANDOFF [--] TEXT"))
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
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent capture HANDOFF [-n LINES]"))
		}
		handoffID := args[1]
		if err := a.authorizeAgentHandoff(ctx, handoffID); err != nil {
			return a.fail(err)
		}
		n := 80
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "-n", "--lines":
				i++
				if i < len(args) {
					n, _ = strconv.Atoi(args[i])
				}
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
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
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent done HANDOFF [--outcome done|failed|abandoned]"))
		}
		handoffID := args[1]
		if err := a.authorizeAgentHandoff(ctx, handoffID); err != nil {
			return a.fail(err)
		}
		outcome := core.OutcomeDone
		keep := false
		closeViz := true
		for i := 2; i < len(args); i++ {
			switch args[i] {
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
		resp, err := a.Handoffs.AgentDone(ctx, handoffID, outcome, keep, closeViz)
		if resp != nil {
			_ = a.out(resp)
		}
		if err != nil {
			return 1
		}
		return 0
	case "status":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return a.fail(fmt.Errorf("usage: relay agent status HANDOFF"))
		}
		handoffID := args[1]
		if err := a.authorizeAgentHandoff(ctx, handoffID); err != nil {
			return a.fail(err)
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

func (a *App) authorizeAgentHandoff(ctx context.Context, handoffID string) error {
	caller := strings.TrimSpace(os.Getenv(bridge.SourceSessionEnv))
	if caller == "" {
		return nil // local human/control-plane invocation
	}
	_, err := core.AuthorizeHandoffManager(a.Reg, handoffID, caller, func(sessionID string) (bool, error) {
		return a.Sessions.Exists(ctx, sessionID)
	})
	return err
}

func (a *App) cmdViz(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("viz subcommand required"))
	}
	if a.Viz == nil {
		return a.fail(fmt.Errorf("viz adapter unavailable"))
	}
	// Pane inventory remains useful when cmux is stopped: persisted bindings
	// are reported as disconnected instead of hidden.
	if args[0] != "list" && !a.Viz.Available(ctx) {
		return a.fail(fmt.Errorf("viz adapter unavailable (is cmux running?)"))
	}
	switch args[0] {
	case "migrate-control":
		migrator, ok := a.Viz.(interface{ QueueControlMigration() (int64, error) })
		if !ok {
			return a.fail(fmt.Errorf("viz adapter does not expose control migration"))
		}
		seq, err := migrator.QueueControlMigration()
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "seq": seq, "kind": "migrate_control"}))
	case "retire-control":
		retirer, ok := a.Viz.(interface{ QueueControlRetirement() (int64, error) })
		if !ok {
			return a.fail(fmt.Errorf("viz adapter does not expose control retirement"))
		}
		seq, err := retirer.QueueControlRetirement()
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "seq": seq, "kind": "retire_control"}))
	case "update":
		updater, ok := a.Viz.(interface{ QueueUpdate() (int64, error) })
		if !ok {
			return a.fail(fmt.Errorf("viz adapter does not expose update signaling"))
		}
		seq, err := updater.QueueUpdate()
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "seq": seq, "kind": "update_relayd"}))
	case "list":
		manager, ok := a.Viz.(interface {
			ManagedPanes(context.Context) ([]cmux.ManagedPane, error)
		})
		if !ok {
			return a.fail(fmt.Errorf("viz adapter does not expose managed panes"))
		}
		panes, err := manager.ManagedPanes(ctx)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "panes": panes}))
	case "rename":
		if len(args) != 3 {
			return a.fail(fmt.Errorf("usage: relay pane rename SESSION_ID NAME"))
		}
		displayName := strings.TrimSpace(args[2])
		if displayName == "" || len(displayName) > 64 || strings.ContainsAny(displayName, "\r\n\t") {
			return a.fail(fmt.Errorf("invalid pane display name %q", args[2]))
		}
		sess, err := a.Reg.GetSession(args[1])
		if err != nil {
			return a.fail(err)
		}
		if sess.Labels == nil {
			sess.Labels = map[string]string{}
		}
		sess.Labels[core.DisplayNameLabel] = displayName
		if err := a.Reg.PutSession(sess); err != nil {
			return a.fail(err)
		}
		if err := a.brandAll(ctx); err != nil {
			return a.fail(err)
		}
		if _, err := a.Viz.SaveRestorable(ctx); err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{
			"ok": true, "session_id": sess.ID, "display_name": displayName,
			"persist_name": sess.Persist.Name,
		}))
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
		ref, err := core.PresentSession(ctx, a.Viz, sess, launch, layout)
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
	if opts.Surface == "" {
		opts.Surface, _ = core.CurrentSurface()
	}
	var resumeSession *core.Session
	if opts.Surface != "" {
		if sess, findErr := a.Reg.FindByPersistName(session, cwd); findErr == nil {
			resumeSession = sess
			if binder, ok := a.Viz.(interface {
				BindSurface(context.Context, string, string, string) (string, error)
			}); ok && a.Viz.Available(ctx) {
				if ref, bindErr := binder.BindSurface(ctx, sess.ID, core.ResumeLaunchCmd(sess.Persist.Name), opts.Surface); bindErr == nil {
					opts.Surface = ref
				} else {
					ui.Warn("cmux pane rebind failed: " + bindErr.Error())
				}
			}
			sess.VizSurfaceRef = opts.Surface
			_ = a.Reg.PutSession(sess)
			core.RememberPane(opts.Surface, sess, true)
		}
	}
	bridgeSessionID := ""
	if sess, findErr := a.Reg.FindByPersistName(session, cwd); findErr == nil {
		resumeSession = sess
		bridgeSessionID = sess.ID
	} else if entry, lookupErr := core.LookupResume(session); lookupErr == nil {
		bridgeSessionID = entry.SessionID
	}
	if bridgeSessionID != "" {
		localSocket, bridgeErr := ensureDesktopBridge(ctx)
		if bridgeErr != nil {
			return a.fail(bridgeErr)
		}
		opts.BridgeLocalSocket = localSocket
		opts.BridgeRemoteSocket = core.BridgeRemoteSocket(bridgeSessionID)
	}
	if resumeSession != nil {
		_ = a.applySessionChrome(ctx, resumeSession)
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
	// The bridge result must reflect the ping. It used to be initialised true
	// and only ever re-set true, so a dead bridge — which strands every remote
	// agent's control path — still reported ok.
	bridgeOK := false
	bridgeDetail := "not running; remote agents cannot reach this control plane"
	if status, err := (bridge.Client{SockPath: core.DesktopBridgeSocketPath()}).Status(ctx); err == nil {
		if status.Build == coord.Build {
			bridgeOK = true
			bridgeDetail = "running build " + status.Build
		} else {
			bridgeDetail = fmt.Sprintf("build drift: bridge=%s client=%s", status.Build, coord.Build)
		}
	}
	checks = append(checks, check{"desktop_bridge", bridgeOK, bridgeDetail})

	// state_dir was a literal true while the only thing that could falsify it
	// ran afterwards with its error discarded.
	stateOK, stateDetail := true, core.StateRoot()
	if err := core.EnsureStateDirs(); err != nil {
		stateOK, stateDetail = false, err.Error()
	}
	checks = append(checks, check{"state_dir", stateOK, stateDetail})

	// Checks for the failure class that cost hours today: things that look
	// healthy while doing nothing. Each of these was invisible before.
	if a.Roots != nil && a.Reg != nil {
		sup := &core.SupervisorService{Reg: a.Reg, Parents: a.Parents}
		if unwatched, err := sup.Unwatched(); err == nil {
			ids := make([]string, 0, len(unwatched))
			for _, ho := range unwatched {
				ids = append(ids, ho.ID)
			}
			if len(ids) == 0 {
				checks = append(checks, check{"handoff_watchers", true, "all live handoffs watched"})
			} else {
				checks = append(checks, check{"handoff_watchers", false,
					"NOT routing escalations: " + strings.Join(ids, ", ")})
			}
		}
		if apex, err := a.Roots.Apex(); err == nil {
			ready := a.Roots.AgentReadinessFor(ctx, a.Sessions, apex.ID)
			checks = append(checks, check{"apex_agent", ready.State == core.AgentReady,
				string(ready.State) + " " + ready.Reason})
		}
		if a.Parents != nil {
			if stale, err := a.Parents.FindStaleEscalations(core.EscalationMaxHold(), time.Now().UTC()); err == nil {
				if len(stale) == 0 {
					checks = append(checks, check{"pending_decisions", true, "none stalled"})
				} else {
					oldest := stale[0]
					for _, s := range stale {
						if s.HeldFor > oldest.HeldFor {
							oldest = s
						}
					}
					checks = append(checks, check{"pending_decisions", false,
						fmt.Sprintf("%d unanswered, oldest %s waiting %dm",
							len(stale), oldest.Message.ID, int(oldest.HeldFor.Minutes()))})
				}
			}
		}
	}
	if host == "" {
		// Not probed is not the same as broken. Failing here would make doctor
		// always exit non-zero, which trains the reader to ignore it — and a
		// diagnostic nobody trusts is worse than none.
		checks = append(checks, check{"coord", true, "not probed; pass -H HOST for a remote relayd check"})
	} else if a.Coord != nil {
		t, err := a.tf(host)
		if err != nil {
			checks = append(checks, check{"coord", false, err.Error()})
		} else if err := a.Coord.Ensure(ctx, t); err != nil {
			checks = append(checks, check{"coord", false, err.Error()})
		} else {
			checks = append(checks, check{"coord", true, "relayd ok on " + host})
			// Ensure() only proves relayd answers, not that it is the relayd
			// this relay was built against. Drift is silent otherwise.
			if reporter, ok := a.Coord.(interface {
				RemoteBuild(context.Context, ports.Transport) (string, error)
			}); ok {
				remote, err := reporter.RemoteBuild(ctx, t)
				switch {
				case err != nil:
					checks = append(checks, check{"coord_build", false, err.Error()})
				case remote != coord.Build:
					checks = append(checks, check{"coord_build", false,
						fmt.Sprintf("%s runs relayd build %s; local is %s — run: relay host init -H %s --apply",
							host, remote, coord.Build, host)})
				default:
					checks = append(checks, check{"coord_build", true, "matches local build " + remote})
				}
			}
		}
	}
	// A diagnostic that always exits 0 cannot be used in a script or a health
	// loop — the caller has to parse prose to learn anything went wrong.
	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	code := 0
	if failed > 0 {
		code = 1
	}
	_ = a.out(map[string]any{
		"ok": failed == 0, "failed": failed, "checks": checks,
		"adapters": map[string]string{
			"transport": "ssh", "persistence": "tmux", "viz": "cmux", "coord": "relayd",
		}})
	return code
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
		labels[s.ID] = core.SessionDisplayName(s)
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
