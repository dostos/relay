// relayd — per-host events and desktop relay bridge (Unix sockets; no TCP).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/controlbridge"
	"github.com/dostos/relay/internal/controlstate"
	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
	cmuxviz "github.com/dostos/relay/internal/viz/cmux"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(cmdServe())
	case "bridge":
		os.Exit(cmdBridge(os.Args[2:]))
	case "viz":
		os.Exit(cmdViz(os.Args[2:]))
	case "viz-broker":
		os.Exit(cmdVizBroker(os.Args[2:]))
	case "control":
		os.Exit(cmdControl(os.Args[2:]))
	case "ping":
		os.Exit(cmdPing())
	case "status":
		os.Exit(cmdStatus())
	case "emit":
		os.Exit(cmdEmit(os.Args[2:]))
	case "subscribe":
		os.Exit(cmdSubscribe(os.Args[2:]))
	case "version", "-V", "--version":
		fmt.Println("relayd", coord.Version)
	case "build":
		fmt.Println(coord.Build)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "relayd: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`relayd — Unix-socket event bus + desktop bridge (NO TCP)

Usage:
  relayd serve                 Listen on ~/.local/state/relay/relayd.sock
  relayd bridge [--relay-bin PATH]
                               Desktop bridge for remote relay → local cmux
  relayd viz ping
  relayd viz present --session ID --target SSH --tmux NAME
                               Optional visualization service (local policy)
  relayd viz sync             Consume queued requests from the control host
  relayd viz follow           Keep consuming while the Mac is awake
  relayd viz authorize --service NAME --public-key-file FILE
                               Explicitly enroll a restricted Viz SSH key
  relayd viz-broker --service NAME
                               Forced-command endpoint for a Viz-only SSH key
  relayd control export|serve
                               Secure control-state migration over stdin/stdout
  relayd ping
  relayd status
  relayd emit -s SESS --kind KIND [--meta JSON]
  relayd subscribe -s SESS [--from N] [-f]
`)
}

// cmdVizBroker is deliberately tiny enough to use as an authorized_keys
// forced command. The key can only subscribe to its projection stream and
// append acknowledgements to that stream's ack channel; it cannot invoke a
// shell or any other relayd operation.
func cmdVizBroker(args []string) int {
	service := ""
	if len(args) == 2 && args[0] == "--service" {
		service = args[1]
	}
	if service == "" || !strings.HasPrefix(service, "relay-viz-") {
		fmt.Fprintln(os.Stderr, "relayd viz-broker: valid --service required")
		return 2
	}
	fields := strings.Fields(strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND")))
	if len(fields) == 2 && fields[0] == "viz-snapshot-v2" && fields[1] == service {
		snapshot, err := visualizationAuthoritySnapshotV2(service)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(snapshot)
		return 0
	}
	if len(fields) == 3 && fields[0] == "viz-resolve" && fields[1] == service {
		resolution, err := visualizationAuthorityResume(fields[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resolution)
		return 0
	}
	if len(fields) == 2 && fields[0] == "viz-snapshot" && fields[1] == service {
		items, err := visualizationAuthoritySnapshot()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(items)
		return 0
	}
	if len(fields) == 4 && fields[0] == "viz-subscribe" && fields[1] == service {
		from, err := strconv.ParseInt(fields[2], 10, 64)
		follow := fields[3] == "1"
		if err != nil || from < 0 || (fields[3] != "0" && fields[3] != "1") {
			fmt.Fprintln(os.Stderr, "relayd viz-broker: invalid cursor")
			return 2
		}
		if err := relayd.SubscribeLocal(sockPath(), service, from, follow, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(fields) == 3 && fields[0] == "viz-ack" && fields[1] == service {
		raw, err := base64.RawURLEncoding.DecodeString(fields[2])
		if err != nil || len(raw) > 64<<10 {
			fmt.Fprintln(os.Stderr, "relayd viz-broker: invalid acknowledgement")
			return 2
		}
		var meta map[string]any
		if json.Unmarshal(raw, &meta) != nil {
			fmt.Fprintln(os.Stderr, "relayd viz-broker: invalid acknowledgement")
			return 2
		}
		if !validVizAck(meta) {
			fmt.Fprintln(os.Stderr, "relayd viz-broker: acknowledgement schema refused")
			return 2
		}
		resp, err := relayd.EmitLocal(sockPath(), service+"-ack", "viz_ack", meta)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		return 0
	}
	fmt.Fprintln(os.Stderr, "relayd viz-broker: command refused")
	return 126
}

func validVizAck(meta map[string]any) bool {
	for key := range meta {
		switch key {
		case "request_seq", "request_kind", "result", "build", "session_id", "op":
		default:
			return false
		}
	}
	if meta["request_kind"] != "project" || meta["op"] != "upsert" {
		return false
	}
	seq, ok := meta["request_seq"].(float64)
	if !ok || seq < 1 || seq != float64(int64(seq)) {
		return false
	}
	session, ok := meta["session_id"].(string)
	if !ok || len(session) < 1 || len(session) > 128 || strings.ContainsAny(session, "\r\n\x00") {
		return false
	}
	result, ok := meta["result"].(string)
	if !ok || len(result) < 1 || len(result) > 8192 {
		return false
	}
	var receipt struct {
		SessionID string `json:"session_id"`
		Revision  int64  `json:"revision"`
		Surface   string `json:"surface"`
	}
	if json.Unmarshal([]byte(result), &receipt) != nil {
		return false
	}
	return receipt.SessionID == session && receipt.Revision == int64(seq) && strings.HasPrefix(receipt.Surface, "surface:") && len(receipt.Surface) <= 128
}

func cmdControl(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "relayd control: export or import required")
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdControlServe()
	case "export":
		bundle, err := controlstate.Export(&core.Registry{})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := json.NewEncoder(os.Stdout).Encode(bundle); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "relayd control: unknown command %q\n", args[0])
		return 2
	}
}

func cmdControlServe() int {
	if err := core.EnsureStateDirs(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	lock, err := os.OpenFile(filepath.Join(core.StateRoot(), "control-bridge.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "relayd control bridge already running")
		return 1
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	sock := core.DesktopBridgeSocketPath()
	srv := &bridge.Server{SockPath: sock, RelayBin: relayBinary(), Build: coord.Build, Authorize: core.AuthorizeBridgeSource}
	registry := &core.Registry{}
	viz := cmuxviz.New()
	service := &controlbridge.Service{
		Registry: registry, BridgeSocket: sock, Stderr: os.Stderr,
		AckSync: func(ctx context.Context) error { return viz.SyncAcks(ctx, registry) },
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := service.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			cancel()
		}
	}()
	fmt.Fprintf(os.Stderr, "relayd home control bridge listening on unix:%s\n", sock)
	if err := srv.Serve(); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdViz(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "relayd viz: ping or present required")
		return 2
	}
	if args[0] == "authorize" {
		return cmdVizAuthorize(args[1:])
	}
	viz := cmuxviz.New()
	switch args[0] {
	case "ping":
		if !viz.Available(context.Background()) {
			fmt.Fprintln(os.Stderr, "cmux unavailable")
			return 1
		}
		fmt.Println(`{"ok":true}`)
		return 0
	case "present":
		var req ports.Presentation
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--session":
				i++
				if i < len(args) {
					req.SessionID = args[i]
				}
			case "--target":
				i++
				if i < len(args) {
					req.Target = args[i]
				}
			case "--tmux":
				i++
				if i < len(args) {
					req.TmuxName = args[i]
				}
			default:
				fmt.Fprintf(os.Stderr, "relayd viz: unknown argument %q\n", args[i])
				return 2
			}
		}
		surface, err := viz.PresentTarget(context.Background(), req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "surface": surface})
		return 0
	case "sync", "follow":
		if err := viz.Follow(context.Background(), args[0] == "follow"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "relayd viz: unknown command %q\n", args[0])
		return 2
	}
}

func cmdBridge(args []string) int {
	relayBin := relayBinary()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--relay-bin":
			i++
			if i < len(args) {
				relayBin = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "relayd bridge: unknown argument %q\n", args[i])
			return 2
		}
	}
	sock := core.DesktopBridgeSocketPath()
	if v := os.Getenv("RELAY_BRIDGE_LOCAL_SOCK"); v != "" {
		sock = v
	}
	srv := &bridge.Server{SockPath: sock, RelayBin: relayBin, Build: coord.Build, Authorize: core.AuthorizeBridgeSource}
	fmt.Fprintf(os.Stderr, "relayd desktop bridge listening on unix:%s\n", sock)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = srv.Close()
	}()
	if err := srv.Serve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// relayBinary resolves relay beside relayd. core.RelayBin is intentionally for
// callers running inside relay itself, where os.Executable is already relay.
func relayBinary() string {
	if value := os.Getenv("RELAY_BIN"); value != "" {
		return value
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "relay")
	}
	if path, err := exec.LookPath("relay"); err == nil {
		return path
	}
	return "relay"
}

func sockPath() string {
	if v := os.Getenv("RELAYD_SOCK"); v != "" {
		return v
	}
	sock, _, err := relayd.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return sock
}

func cmdServe() int {
	sock, events, err := relayd.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if v := os.Getenv("RELAYD_SOCK"); v != "" {
		sock = v
	}
	store, err := relayd.NewStore(events)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	srv := &relayd.Server{SockPath: sock, Store: store}
	fmt.Fprintf(os.Stderr, "relayd %s starting unix:%s (no TCP; verify with relayd status)\n", coord.Version, sock)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = srv.Close()
		os.Exit(0)
	}()

	if err := srv.Serve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdPing() int {
	resp, err := relayd.PingLocal(sockPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(resp)
	return 0
}

func cmdStatus() int {
	resp, err := relayd.StatusLocal(sockPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(resp)
	return 0
}

func cmdEmit(args []string) int {
	var session, kind, metaRaw string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-s", "--session":
			i++
			if i < len(args) {
				session = args[i]
			}
		case "--kind", "-k":
			i++
			if i < len(args) {
				kind = args[i]
			}
		case "--meta":
			i++
			if i < len(args) {
				metaRaw = args[i]
			}
		}
	}
	var meta map[string]any
	if metaRaw != "" {
		_ = json.Unmarshal([]byte(metaRaw), &meta)
	}
	resp, err := relayd.EmitLocal(sockPath(), session, kind, meta)
	if err != nil {
		// No file fallback — daemon is the sole writer (avoids seq races).
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(resp)
	return 0
}

func cmdSubscribe(args []string) int {
	var session string
	var from int64
	follow := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-s", "--session":
			i++
			if i < len(args) {
				session = args[i]
			}
		case "--from":
			i++
			if i < len(args) {
				from, _ = strconv.ParseInt(args[i], 10, 64)
			}
		case "-f", "--follow":
			follow = true
		}
	}
	if session == "" {
		fmt.Fprintln(os.Stderr, "relayd subscribe: -s SESS required")
		return 2
	}
	if err := relayd.SubscribeLocal(sockPath(), session, from, follow, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
