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
	"github.com/dostos/relay/internal/clientfleet"
	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/coord/sshcoord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/persist/tmux"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
	localtransport "github.com/dostos/relay/internal/transport/local"
	sshtransport "github.com/dostos/relay/internal/transport/ssh"
	"github.com/dostos/relay/internal/ui"
)

// App wires adapters and runs CLI commands.
type App struct {
	Sessions    *core.SessionService
	Profiles    *core.ProfileService
	Auth        *core.AuthService
	Bootstrap   *core.BootstrapService
	Discover    *core.DiscoverService
	Containers  *core.ContainerService
	Ensure      *core.EnsureService
	Reg         *core.Registry
	Coord       ports.Coord
	Maint       *core.MaintenanceService
	JSON        bool
	CompactJSON bool
	// Stdin is the pane text source for `pane classify`; nil means os.Stdin.
	Stdin io.Reader
	tf    core.TransportFactory
}

// New constructs the default App (SSH + tmux + relayd coord).
func New() *App {
	reg := &core.Registry{}
	persist := tmux.New()
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
	accounts := core.DefaultAccountsRunner()
	profiles := &core.ProfileService{NewTransport: tf, Accounts: accounts}
	sessions := &core.SessionService{
		Reg:          reg,
		Profiles:     profiles,
		NewTransport: tf,
		Persist:      persist,
		Coord:        coord,
	}
	boot := &core.BootstrapService{NewTransport: tf}
	auth := &core.AuthService{
		Profiles:     profiles,
		Sessions:     sessions,
		NewTransport: tf,
		Accounts:     accounts,
	}
	return &App{
		Sessions:  sessions,
		Profiles:  profiles,
		Auth:      auth,
		Bootstrap: boot,
		Discover: &core.DiscoverService{
			NewTransport: tf,
			Coord:        coord,
			Profiles:     profiles,
		},
		Ensure: &core.EnsureService{
			NewTransport: tf,
			Profiles:     profiles,
			Accounts:     accounts,
		},
		Containers: &core.ContainerService{
			NewTransport: tf,
			Profiles:     profiles,
		},
		Reg:   reg,
		Coord: coord,
		Maint: &core.MaintenanceService{Sessions: sessions, Reg: reg, NewTransport: tf},
		tf:    tf,
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
		// `pane classify` reads its input from STDIN, which the bridge request
		// (argv + source only) cannot carry: forwarded, it would classify
		// nothing. It is a pure local function and always runs here.
		if arg == "signal" || arg == "hook" || arg == "ask" || arg == "pane" {
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
	if sock == "" && !commandNeedsLocalTTY(args) {
		if identity, err := core.LoadHomeClientIdentity(); err == nil {
			sock = core.DesktopBridgeSocketPath()
			source = identity
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

func commandNeedsLocalTTY(args []string) bool {
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != "--json" {
			filtered = append(filtered, arg)
		}
	}
	if len(filtered) >= 2 && filtered[0] == "session" && filtered[1] == "attach" {
		return true
	}
	// `pane classify` reads the pane text from the caller's stdin; the bridge
	// carries argv, not stdin, so it must run in this process.
	if len(filtered) >= 2 && filtered[0] == "pane" && filtered[1] == "classify" {
		return true
	}
	return len(filtered) > 0 && filtered[0] == "resume" && (len(filtered) == 1 || (filtered[1] != "list" && filtered[1] != "reap" && filtered[1] != "prune"))
}

func ensureDesktopBridge(ctx context.Context) (string, error) {
	sock := core.DesktopBridgeSocketPath()
	client := bridge.Client{SockPath: sock}
	if client.Ping(ctx) == nil {
		return sock, nil
	}
	return "", fmt.Errorf("relay home command boundary is unavailable at %s; start or repair relay service run", sock)
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
	case "container":
		return a.cmdContainer(ctx, filtered[1:])
	case "auth":
		return a.cmdAuth(ctx, filtered[1:])
	case "targets":
		return a.cmdTargets(ctx, filtered[1:])
	case "session", "sess":
		return a.cmdSession(ctx, filtered[1:])
	case "pane":
		return a.cmdPane(ctx, filtered[1:])
	case "client":
		return a.cmdClient(filtered[1:])
	case "resume":
		return a.cmdResume(ctx, filtered[1:])
	case "doctor":
		return a.cmdDoctor(ctx, filtered[1:])
	default:
		// `relay HOST NAME` is the only two-word form, and HOST has to be a
		// real ssh alias: a retired verb typed from habit (`relay parent list`)
		// must fail as an unknown command, not try to open a tmux on a host
		// called "parent".
		if len(filtered) == 2 && core.ValidateTargetAlias(filtered[0]) == nil {
			return a.cmdNamed(ctx, filtered[0], filtered[1])
		}
		return a.fail(fmt.Errorf("unknown command %q (relay --help)", filtered[0]))
	}
}

func (a *App) cmdClient(args []string) int {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Println("usage: relay client list|update [--client ID]|update-status")
		return 0
	}
	root := core.StateRoot()
	switch args[0] {
	case "list":
		clients, err := clientfleet.List(root)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "clients": clientfleet.Summaries(clients)}))
	case "update":
		selected := ""
		if len(args) == 3 && args[1] == "--client" {
			selected = args[2]
		} else if len(args) != 1 {
			return a.fail(fmt.Errorf("usage: relay client update [--client ID]"))
		}
		queued, err := clientfleet.QueueUpdate(root, filepath.Join(root, "relayd.sock"), selected)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		ok := true
		for _, result := range queued {
			if result.State != "queued" {
				ok = false
			}
		}
		if outErr := a.out(map[string]any{"ok": ok, "kind": "update_relayd", "clients": queued}); outErr != nil {
			return a.errOut(outErr)
		}
		if !ok {
			return 1
		}
		return 0
	case "update-status", "status":
		statuses, err := clientfleet.Status(root)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "clients": statuses}))
	default:
		return a.fail(fmt.Errorf("unknown client subcommand %q", args[0]))
	}
}

func (a *App) cmdHelp() int {
	fmt.Print(`relay — durable remote sessions (SSH + tmux; Forge or any terminal attaches with relay resume)

Usage:
  relay [--json] <command> ...
  relay HOST NAME                    Open/create named tmux on HOST and attach here

New machine (ssh config → discover → init):
  relay targets                       List Host aliases from ~/.ssh/config (+ Include)
  relay host discover -H HOST         Inventory + proposed host.yaml (no writes)
  relay host init -H HOST [--apply] [--force]
                                      Install relay + compatibility shim; write proposal with --apply
  relay host ensure -H HOST [--apply] Deps + propose/merge the logins agent-accounts lists as
                                      ccs:*/codex:* agents + their auth state

Dev containers (declared under containers: in the remote host.yaml):
  relay container up -H HOST --container NAME [--recreate] [--reprovision]
                                      devcontainer up + relay named volumes; provisions the toolkit
  relay container status|down -H HOST --container NAME
  relay session create -H HOST --container NAME [--ephemeral] [--volume V:/p] [--bind /h:/p] [--gpus 0,1] [--network N] [-- ARGV]
                                      An image-backed container gets one instance per session;
                                      the trailing command is what the pane runs.
  relay container stop|start -H HOST --container NAME [--name INSTANCE]

Host profiles (authoritative on each remote ~/.config/relay/host.yaml):
  relay host show -H HOST
  relay host fetch -H HOST
  relay host probe -H HOST
  relay host cache -H HOST
  relay host map -H HOST --match NAME --remote-cwd DIR
                                      Record where a local repo lives on HOST
  relay host ls -H HOST [--path DIR]  List remote directories (for a folder picker)
  relay host clone -H HOST --repo OWNER/NAME [--parent DIR]
                                      Clone with the HOST's gh; reports if it has a devcontainer
  relay host container -H HOST --name NAME --workspace-folder DIR
                                      Declare a devcontainer so relay container up can launch it
  relay host example -H HOST          Print starter host.yaml
  relay host bootstrap -H HOST        Install host-local event service (unix socket; one quiet SSH)

Agent auth (claude / cursor-agent / codex / ccs:<profile> / …):
  relay auth status -H HOST [--agent NAME]
                                      Every login's auth health, as agent-accounts reports it
                                      (confidence: live-probe | declared | local-expiry | none).
                                      Needs the agent-accounts CLI ($RELAY_AGENT_ACCOUNTS or PATH).
  relay auth login -H HOST --agent NAME
                                      Remote login session + reassemble wrapped OAuth URL + open locally
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
  relay session destroy ID [--keep-remote]
  relay session sensors ID [--silence SEC]   Reinstall quiet idle/exit hooks

Attach (what a Forge tab, or any terminal, runs):
  relay resume --session NAME [--host HOST] [--cwd DIR] [--no-reconnect]
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
	case "map":
		// Records where a local repo lives on this host, in the host's own
		// host.yaml. A client that offers a folder picker edits this; it does
		// not keep its own copy, or relay and that client would answer the same
		// question differently the first time either changed.
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		match, remoteCWD := "", ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--match":
				i++
				if i < len(rest) {
					match = rest[i]
				}
			case "--remote-cwd":
				i++
				if i < len(rest) {
					remoteCWD = rest[i]
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if match == "" || remoteCWD == "" {
			return a.fail(fmt.Errorf("usage: relay host map -H HOST --match NAME --remote-cwd DIR"))
		}
		t, err := a.tf(host)
		if err != nil {
			return a.fail(err)
		}
		profile, err := a.Profiles.MapRepo(ctx, t, host, match, remoteCWD)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{
			"ok": true, "host_id": host, "match": match, "remote_cwd": remoteCWD,
			"path_map": profile.PathMap,
		}))
	case "container":
		// Declares a devcontainer in the host's profile so `relay container up`
		// can launch it. Only the two fields that verb needs are written:
		// toolkit and home volumes are decisions about what an agent requires,
		// and inventing them would put volumes on a shared host nobody asked
		// for.
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		name, folder := "", ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--name":
				i++
				if i < len(rest) {
					name = rest[i]
				}
			case "--workspace-folder":
				i++
				if i < len(rest) {
					folder = rest[i]
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if name == "" || folder == "" {
			return a.fail(fmt.Errorf("usage: relay host container -H HOST --name NAME --workspace-folder DIR"))
		}
		t, err := a.tf(host)
		if err != nil {
			return a.fail(err)
		}
		profile, err := a.Profiles.DeclareContainer(ctx, t, host, name, folder)
		if err != nil {
			return a.fail(err)
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{
			"ok": true, "host_id": host, "name": name,
			"workspace_folder": folder, "containers": profile.Containers,
		}))
	case "clone":
		// Clones with the HOST's gh, so the host's credentials apply and no
		// token from this machine is forwarded to a shared box.
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		repo, parent := "", "~"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--repo":
				i++
				if i < len(rest) {
					repo = rest[i]
				}
			case "--parent":
				i++
				if i < len(rest) {
					parent = rest[i]
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if repo == "" {
			return a.fail(fmt.Errorf("usage: relay host clone -H HOST --repo OWNER/NAME [--parent DIR]"))
		}
		cmd, err := core.CloneRepoCommand(repo, parent)
		if err != nil {
			return a.fail(err)
		}
		t, err := a.tf(host)
		if err != nil {
			return a.fail(err)
		}
		out, errOut, runErr := t.Run(ctx, "", cmd)
		state, path, dev, parseErr := core.ParseCloneResult(out)
		if runErr != nil || parseErr != nil {
			detail := strings.TrimSpace(errOut)
			if detail == "" {
				detail = strings.TrimSpace(out)
			}
			return a.fail(fmt.Errorf("clone %s on %s: %s", repo, host, detail))
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{
			"ok": true, "host_id": host, "repo": repo,
			"state": state, "path": path, "devcontainer": dev,
		}))
	case "ls":
		// One level of directories, so a client can offer a remote folder
		// picker without speaking SSH itself.
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		dir := "~"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--path":
				i++
				if i < len(rest) {
					dir = rest[i]
				}
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		cmd, err := core.RemoteDirsCommand(dir)
		if err != nil {
			return a.fail(err)
		}
		t, err := a.tf(host)
		if err != nil {
			return a.fail(err)
		}
		out, _, runErr := t.Run(ctx, "", cmd)
		if runErr != nil {
			return a.fail(fmt.Errorf("list %s on %s: %w", dir, host, runErr))
		}
		dirs := make([]string, 0, 32)
		for _, line := range strings.Split(out, "\n") {
			name := strings.TrimSuffix(strings.TrimSpace(line), "/")
			if name != "" {
				dirs = append(dirs, name)
			}
		}
		a.JSON = true
		return a.errOut(a.out(map[string]any{"ok": true, "host_id": host, "path": dir, "dirs": dirs}))
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
	case "ensure":
		if host == "" {
			return a.fail(fmt.Errorf("-H HOST required"))
		}
		opts := core.EnsureOptions{}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--apply":
				opts.Apply = true
			default:
				return a.fail(rejectUnknownFlag(rest[i]))
			}
		}
		if a.Ensure == nil {
			return a.fail(fmt.Errorf("ensure service unavailable"))
		}
		res, err := a.Ensure.Ensure(ctx, host, opts)
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
		for _, d := range res.Deps {
			mark := "missing"
			if d.Present {
				mark = "ok"
			}
			line := fmt.Sprintf("  dep %-28s %s", d.Name, mark)
			if d.Hint != "" && !d.Present {
				line += " — " + d.Hint
			}
			fmt.Println(line)
		}
		if len(res.ProposedAgents) > 0 {
			ui.Note("proposed account agents (not written until --apply)")
			for _, ag := range res.ProposedAgents {
				fmt.Printf("  + %s\n", ag.Name)
			}
		}
		if len(res.SkippedAgents) > 0 {
			fmt.Printf("  skipped     %d already present\n", len(res.SkippedAgents))
		}
		for _, row := range res.Auth {
			auth := "unauthed"
			if row.Authed {
				auth = "authed"
			}
			if !row.Present {
				auth = "missing"
			}
			fmt.Printf("  auth %-24s %s\n", row.Agent, auth)
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

// parseReadinessFlags reads the optional `--readiness [--readiness-lines N]`
// pair that `session list` and `session get` accept. Anything else is an
// unknown flag, so the no-flag output stays byte-identical to before.
func parseReadinessFlags(rest []string) (withReadiness bool, lines int, err error) {
	lines = core.DefaultReadinessLines
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--readiness":
			withReadiness = true
		case "--readiness-lines":
			if i+1 >= len(rest) {
				return false, 0, fmt.Errorf("%s requires a value", rest[i])
			}
			n, convErr := strconv.Atoi(rest[i+1])
			if convErr != nil || n <= 0 {
				return false, 0, fmt.Errorf("--readiness-lines must be a positive integer, got %q", rest[i+1])
			}
			lines = n
			withReadiness = true
			i++
		default:
			return false, 0, rejectUnknownFlag(rest[i])
		}
	}
	return withReadiness, lines, nil
}

// cmdPane is the presenter's local half of readiness: `relay pane classify`
// runs the classifier over pane text on stdin — no ssh, no registry, no
// state — so Forge can classify a surface it is attached to without a second
// copy of the classifier. Every other `pane` verb retired with the presenter
// and stays an unknown command.
func (a *App) cmdPane(_ context.Context, args []string) int {
	if len(args) == 0 || args[0] != "classify" {
		name := "pane"
		if len(args) > 0 {
			name = "pane " + args[0]
		}
		return a.fail(fmt.Errorf("unknown command %q (relay --help)", name))
	}
	a.JSON = true
	lines := core.DefaultReadinessLines
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-n", "--lines":
			if i+1 >= len(args) {
				return a.fail(fmt.Errorf("%s requires a value", args[i]))
			}
			lines, _ = strconv.Atoi(args[i+1])
			i++
		default:
			return a.fail(rejectUnknownFlag(args[i]))
		}
	}
	text, err := io.ReadAll(a.stdin())
	if err != nil {
		return a.fail(fmt.Errorf("read stdin: %w", err))
	}
	rep := core.ClassifyText(string(text), lines)
	return a.errOut(a.out(rep))
}

// stdin is what `pane classify` reads; tests substitute it.
func (a *App) stdin() io.Reader {
	if a.Stdin != nil {
		return a.Stdin
	}
	return os.Stdin
}

// parseSessionCreateArgs reads `session create` flags after -H. A trailing
// `-- ARGV…` is the pane's inner command — for a container session it runs
// inside the container, so a worker's entrypoint is what the pane shows —
// and the container extras (--volume, --bind, --gpus, --network) are refused
// by core unless the container is image-backed.
func parseSessionCreateArgs(host string, rest []string) (core.CreateOpts, error) {
	opts := core.CreateOpts{HostID: host}
	need := func(i int, flag string) (string, error) {
		if i+1 >= len(rest) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return rest[i+1], nil
	}
	for i := 0; i < len(rest); i++ {
		var v string
		var err error
		switch rest[i] {
		case "--repo":
			v, err = need(i, rest[i])
			opts.RepoRef = v
			i++
		case "--cwd", "-R":
			v, err = need(i, rest[i])
			opts.RemoteCWD = v
			i++
		case "--container":
			v, err = need(i, rest[i])
			opts.Container = v
			i++
		case "--ephemeral":
			opts.ContainerEphemeral = true
		case "--name", "-s":
			v, err = need(i, rest[i])
			opts.Name = v
			i++
		case "--volume", "-v":
			v, err = need(i, rest[i])
			opts.ContainerVolumes = append(opts.ContainerVolumes, v)
			i++
		case "--bind":
			v, err = need(i, rest[i])
			opts.ContainerBinds = append(opts.ContainerBinds, v)
			i++
		case "--gpus":
			v, err = need(i, rest[i])
			opts.ContainerGPUs = v
			i++
		case "--network":
			v, err = need(i, rest[i])
			opts.ContainerNetwork = v
			i++
		case "--":
			if i+1 >= len(rest) {
				return opts, fmt.Errorf("-- must be followed by the command to run")
			}
			// Each word quoted on its own, so the pane runs exactly the argv
			// given, not a re-parse of it by the remote shell.
			words := make([]string, 0, len(rest)-i-1)
			for _, w := range rest[i+1:] {
				words = append(words, shellquote.Quote(w))
			}
			opts.Command = strings.Join(words, " ")
			i = len(rest)
		default:
			return opts, rejectUnknownFlag(rest[i])
		}
		if err != nil {
			return opts, err
		}
	}
	return opts, nil
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
	return a.openNamedAndAttach(ctx, opts)
}

// openNamedAndAttach opens (or adopts) the named session and attaches to it in
// this terminal — the same attach `relay resume` performs, so a human running
// `relay HOST NAME` in Forge or any terminal lands in the pane. Invoked through
// the bridge from inside a remote pane there is no terminal to attach, so the
// session is reported with the command that attaches it.
func (a *App) openNamedAndAttach(ctx context.Context, opts core.CreateOpts) int {
	host := opts.HostID
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
	if sess.Labels["adopted"] == "existing" {
		ui.Warn("existing tmux adopted without relay bridge identity; use a new NAME for remote-to-remote relay commands")
	}
	attach := []string{"relay", "resume", "--session", sess.Persist.Name, "--host", host}
	if os.Getenv(bridge.LocalInvokeEnv) == "1" || a.JSON {
		a.JSON = true
		return a.errOut(a.out(map[string]any{
			"ok": true, "created": created, "session": sess, "attach": attach,
		}))
	}
	return a.cmdResume(ctx, []string{"--session", sess.Persist.Name, "--host", host})
}

func (a *App) cmdSession(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(fmt.Errorf("session subcommand required"))
	}
	sub := args[0]
	switch sub {
	case "list":
		withReadiness, lines, err := parseReadinessFlags(args[1:])
		if err != nil {
			return a.fail(err)
		}
		if withReadiness {
			list, err := a.Sessions.ListWithReadiness(ctx, lines)
			if err != nil {
				return a.fail(err)
			}
			a.JSON = true
			return a.errOut(a.out(list))
		}
		list, err := a.Sessions.List()
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(list))
	case "get":
		if len(args) < 2 {
			return a.fail(fmt.Errorf("session id required"))
		}
		withReadiness, lines, err := parseReadinessFlags(args[2:])
		if err != nil {
			return a.fail(err)
		}
		s, err := a.Sessions.Get(args[1])
		if err != nil {
			return a.fail(err)
		}
		if withReadiness {
			_, rep, _ := a.Sessions.Readiness(ctx, s.ID, lines)
			a.JSON = true
			return a.errOut(a.out(core.SessionReadiness{Session: s, Readiness: rep}))
		}
		return a.errOut(a.out(s))
	case "readiness":
		// One session, captured and classified, for a presenter that does not
		// hold the pane text itself (a put-away session). Always JSON.
		if len(args) < 2 {
			return a.fail(fmt.Errorf("usage: relay session readiness ID [--lines N]"))
		}
		a.JSON = true
		lines := core.DefaultReadinessLines
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "-n", "--lines":
				if i+1 >= len(args) {
					return a.fail(fmt.Errorf("%s requires a value", args[i]))
				}
				lines, _ = strconv.Atoi(args[i+1])
				i++
			case "--readiness":
				// tolerated: `session readiness ID` is already the readiness verb
			default:
				return a.fail(rejectUnknownFlag(args[i]))
			}
		}
		sess, rep, err := a.Sessions.Readiness(ctx, args[1], lines)
		if err != nil {
			if sess == nil {
				return a.fail(err)
			}
			_ = a.out(map[string]any{"ok": false, "error": err.Error(), "session_id": sess.ID, "host_id": sess.HostID, "state": core.AgentUnknown, "lines": lines})
			return 1
		}
		return a.errOut(a.out(map[string]any{
			"ok": true, "session_id": sess.ID, "host_id": sess.HostID,
			"state": rep.State, "reason": rep.Reason, "gate": rep.Gate,
			"lines": rep.Lines, "sampled_at": rep.SampledAt,
		}))
	case "rename":
		if len(args) != 3 {
			return a.fail(fmt.Errorf("usage: relay session rename ID NAME"))
		}
		sess, err := a.Sessions.Rename(ctx, args[1], args[2])
		if err != nil {
			return a.fail(err)
		}
		return a.errOut(a.out(map[string]any{
			"ok": true, "session_id": sess.ID, "persist_name": sess.Persist.Name,
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
		opts, err := parseSessionCreateArgs(host, rest)
		if err != nil {
			return a.fail(err)
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
		for _, x := range args[2:] {
			switch x {
			case "--keep-remote":
				keep = true
			default:
				return a.fail(rejectUnknownFlag(x))
			}
		}
		if err := a.Sessions.Destroy(ctx, args[1], keep); err != nil {
			return a.fail(err)
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
		if err := a.Sessions.ReinstallSensors(ctx, id, silence); err != nil {
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
	var session, cwd, targetHost string
	opts := core.ResumeOpts{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session", "-s":
			i++
			if i < len(args) {
				session = args[i]
			}
		case "--cwd":
			i++
			if i < len(args) {
				cwd = args[i]
			}
		case "--host":
			i++
			if i < len(args) {
				targetHost = args[i]
				opts.TargetHost = targetHost
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
		return a.fail(fmt.Errorf("usage: relay resume --session NAME [--host HOST] [--cwd DIR] [--no-reconnect]  |  relay resume list|reap|prune"))
	}
	bridgeSessionID := ""
	if sess, findErr := a.Reg.FindByPersistName(session, cwd); findErr == nil {
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
	// The bridge result must reflect the ping. It used to be initialised true
	// and only ever re-set true, so a dead bridge — which strands every remote
	// agent's control path — still reported ok.
	bridgeOK := false
	bridgeDetail := "not running; remote agents cannot reach this control plane"
	status, bridgeErr := (bridge.Client{SockPath: core.DesktopBridgeSocketPath()}).Status(ctx)
	if bridgeErr == nil {
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

	var serviceHealth struct {
		Build      string `json:"build"`
		PID        int    `json:"pid"`
		Ready      bool   `json:"ready"`
		Live       bool   `json:"live"`
		Stopping   bool   `json:"stopping"`
		UpdatedAt  string `json:"updated_at"`
		Components map[string]struct {
			Build          string `json:"build"`
			Ready          bool   `json:"ready"`
			Live           bool   `json:"live"`
			DurableEffects bool   `json:"durable_effects"`
			Error          string `json:"error"`
		} `json:"components"`
	}
	healthRaw, healthErr := os.ReadFile(core.HomeServiceHealthPath())
	remoteHealth := false
	if healthErr == nil {
		healthErr = json.Unmarshal(healthRaw, &serviceHealth)
	}
	processAlive := healthErr == nil && serviceHealth.PID > 0 && (remoteHealth || syscall.Kill(serviceHealth.PID, 0) == nil)
	healthUpdated, _ := time.Parse(time.RFC3339Nano, serviceHealth.UpdatedAt)
	healthFresh := !healthUpdated.IsZero() && time.Since(healthUpdated) < 15*time.Second
	healthOK := healthErr == nil && processAlive && healthFresh && serviceHealth.Live && serviceHealth.Ready && !serviceHealth.Stopping && serviceHealth.Build == coord.Build
	healthDetail := "unified home service health unavailable"
	if healthErr != nil {
		healthDetail = healthErr.Error()
	} else {
		healthDetail = fmt.Sprintf("pid=%d build=%s components=%d ready=%t", serviceHealth.PID, serviceHealth.Build, len(serviceHealth.Components), serviceHealth.Ready)
		for name, component := range serviceHealth.Components {
			if component.Build != serviceHealth.Build || !component.Ready || !component.Live || !component.DurableEffects {
				healthOK = false
				healthDetail += fmt.Sprintf("; %s unhealthy (%s)", name, component.Error)
			}
		}
	}
	checks = append(checks, check{"home_service", healthOK, healthDetail})

	// A migrated home has exactly one authority owner. Old event/control/
	// supervisor processes may be healthy individually while still splitting
	// sockets, builds, and policy, so surface them explicitly.
	legacyDetail := "none"
	legacyProcesses, legacyErr := legacyAuthorityProcesses()
	legacyOK := legacyErr == nil && len(legacyProcesses) == 0
	if legacyErr != nil {
		legacyDetail = legacyErr.Error()
	} else if len(legacyProcesses) > 0 {
		legacyDetail = strings.Join(legacyProcesses, "; ")
	}
	checks = append(checks, check{"legacy_authority_processes", legacyOK, legacyDetail})

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
						fmt.Sprintf("%s runs relayd build %s; local is %s — run: relay host bootstrap -H %s",
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
			"transport": "ssh", "persistence": "tmux", "coord": "relayd",
		}})
	return code
}

func legacyAuthorityProcesses() ([]string, error) {
	out, err := exec.Command("ps", "-axo", "pid=,comm=,args=").Output()
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		command := filepath.Base(fields[1])
		argv := fields[2:]
		if isLegacyAuthorityProcess(command, argv) {
			matches = append(matches, strings.TrimSpace(line))
		}
	}
	return matches, nil
}

func isLegacyAuthorityProcess(command string, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	// argv[0] is the executable as rendered by ps. Matching the executable's
	// comm field and exact argument positions avoids concurrent doctor/pgrep
	// command lines being misreported as live legacy services.
	if command == "relayd" {
		return len(argv) >= 2 && argv[1] == "serve" || len(argv) >= 3 && argv[1] == "control" && argv[2] == "serve"
	}
	return command == "relay" && len(argv) >= 2 && argv[1] == "supervise"
}

// cmdContainer manages relay-declared containers on a host: bringing a Dev
// Containers workspace up with relay's named volumes injected, tearing it
// down, and reporting whether it is running.
func (a *App) cmdContainer(ctx context.Context, args []string) int {
	usage := "usage: relay container up|down|status|stop|start -H HOST --container NAME [--name INSTANCE] [--recreate] [--reprovision]"
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(usage)
		return 0
	}
	sub := args[0]
	host, name, tmuxName := "", "", ""
	recreate, reprovision := false, false
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-H", "--host":
			i++
			if i < len(rest) {
				host = rest[i]
			}
		case "--container", "-c":
			i++
			if i < len(rest) {
				name = rest[i]
			}
		case "--name", "-s":
			i++
			if i < len(rest) {
				tmuxName = rest[i]
			}
		case "--recreate":
			recreate = true
		case "--reprovision":
			reprovision = true
		default:
			return a.fail(rejectUnknownFlag(rest[i]))
		}
	}
	if host == "" {
		return a.fail(fmt.Errorf("-H HOST required"))
	}
	if name == "" {
		return a.fail(fmt.Errorf("--container NAME required"))
	}
	if a.Containers == nil {
		return a.fail(fmt.Errorf("container service unavailable"))
	}
	var (
		st  *core.ContainerStatus
		err error
	)
	switch sub {
	case "up":
		st, err = a.Containers.UpInstance(ctx, host, name, tmuxName, recreate, reprovision)
	case "down":
		st, err = a.Containers.DownInstance(ctx, host, name, tmuxName)
	case "status":
		st, err = a.Containers.StatusInstance(ctx, host, name, tmuxName)
	case "stop":
		st, err = a.Containers.Stop(ctx, host, name, tmuxName)
	case "start":
		st, err = a.Containers.Start(ctx, host, name, tmuxName)
	default:
		return a.fail(fmt.Errorf("unknown container subcommand %q\n%s", sub, usage))
	}
	if err != nil {
		return a.fail(err)
	}
	if a.JSON {
		return a.errOut(a.out(st))
	}
	state := "stopped"
	if st.Running {
		state = "running"
	} else if !st.Present {
		state = "absent"
	}
	fmt.Printf("%s on %s: %s\n", st.Name, st.HostID, state)
	if st.Instance != "" {
		fmt.Printf("  instance   %s\n", st.Instance)
	}
	if st.ContainerID != "" {
		fmt.Printf("  container  %s (%s)\n", st.ContainerID, st.IDLabel)
	}
	if !st.Running && st.Present && st.Instance != "" {
		fmt.Printf("  exit code  %d\n", st.ExitCode)
	}
	if st.Toolkit != "" {
		fmt.Printf("  toolkit    volume %s\n", st.Toolkit)
	}
	if st.Home != "" {
		fmt.Printf("  home       volume %s\n", st.Home)
	}
	if st.Detail != "" {
		fmt.Printf("  detail     %s\n", st.Detail)
	}
	return 0
}
