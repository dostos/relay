// relayd — per-host events and desktop relay bridge (Unix sockets; no TCP).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/dostos/relay/internal/bridge"
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
  relayd ping
  relayd status
  relayd emit -s SESS --kind KIND [--meta JSON]
  relayd subscribe -s SESS [--from N] [-f]
`)
}

func cmdViz(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "relayd viz: ping or present required")
		return 2
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
	relayBin := core.RelayBin()
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
