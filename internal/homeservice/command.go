package homeservice

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
)

func Command(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relay service run|status|event")
		return 2
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: relay service run")
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := New().Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: relay service status")
			return 2
		}
		health, err := ReadHealth()
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
			return 1
		}
		alive := syscall.Kill(health.PID, 0) == nil
		updated, _ := time.Parse(time.RFC3339Nano, health.UpdatedAt)
		fresh := !updated.IsZero() && time.Since(updated) < 15*time.Second
		result := map[string]any{"ok": health.Ready && health.Live && alive && fresh && !health.Stopping, "health": health, "process_alive": alive, "health_fresh": fresh}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		if !result["ok"].(bool) {
			return 1
		}
		return 0
	case "event":
		return eventCommand(args[1:])
	case "boundary":
		return compatibilityComponentCommand("boundary", args[1:], runCommandBoundary)
	case "watcher":
		return compatibilityComponentCommand("watcher", args[1:], runWatcherReconciler)
	default:
		fmt.Fprintf(os.Stderr, "relay service: unknown command %q\n", args[0])
		return 2
	}
}

func compatibilityComponentCommand(name string, args []string, run func(context.Context, func(bool)) error) int {
	if len(args) != 1 || args[0] != "run" {
		fmt.Fprintf(os.Stderr, "usage: relay service %s run\n", name)
		return 2
	}
	fmt.Fprintf(os.Stderr, "relay service %s run is a temporary legacy-unit compatibility route; migrate to relay service run\n", name)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, func(bool) {}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func eventCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relay service event ping|status|emit|subscribe")
		return 2
	}
	sock, err := eventSocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: relay service event run")
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runEventCoordinator(ctx, func(bool) {}); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "ping":
		resp, err := coordrelayd.PingLocal(sock)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		return 0
	case "status":
		resp, err := coordrelayd.StatusLocal(sock)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		return 0
	case "emit":
		var session, kind, metaRaw string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "-s", "--session":
				i++
				if i < len(args) {
					session = args[i]
				}
			case "-k", "--kind":
				i++
				if i < len(args) {
					kind = args[i]
				}
			case "--meta":
				i++
				if i < len(args) {
					metaRaw = args[i]
				}
			default:
				fmt.Fprintf(os.Stderr, "relay service event emit: unknown argument %q\n", args[i])
				return 2
			}
		}
		var meta map[string]any
		if metaRaw != "" {
			if err := json.Unmarshal([]byte(metaRaw), &meta); err != nil {
				fmt.Fprintln(os.Stderr, "relay service event emit: invalid --meta JSON")
				return 2
			}
		}
		resp, err := coordrelayd.EmitLocal(sock, session, kind, meta)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		return 0
	case "subscribe":
		var session string
		var from int64
		follow := false
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "-s", "--session":
				i++
				if i < len(args) {
					session = args[i]
				}
			case "--from":
				i++
				if i < len(args) {
					from, err = strconv.ParseInt(args[i], 10, 64)
					if err != nil || from < 0 {
						fmt.Fprintln(os.Stderr, "relay service event subscribe: invalid --from")
						return 2
					}
				}
			case "-f", "--follow":
				follow = true
			default:
				fmt.Fprintf(os.Stderr, "relay service event subscribe: unknown argument %q\n", args[i])
				return 2
			}
		}
		if session == "" {
			fmt.Fprintln(os.Stderr, "relay service event subscribe: -s SESSION required")
			return 2
		}
		if err := coordrelayd.SubscribeLocal(sock, session, from, follow, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "relay service event: unknown command %q\n", args[0])
		return 2
	}
}
