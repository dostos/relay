package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dostos/relay/internal/cli"
	"github.com/dostos/relay/internal/compat"
	"github.com/dostos/relay/internal/homeservice"
	"github.com/dostos/relay/internal/mcpserver"
	cmuxviz "github.com/dostos/relay/internal/viz/cmux"
	"github.com/dostos/relay/internal/vizbroker"
)

func main() {
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "relayd" {
		mapped, ok := compat.MapRelayd(args)
		if !ok {
			fmt.Fprintln(os.Stderr, "relayd compatibility command is unavailable; use relay --help")
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "relayd is deprecated; use relay")
		args = mapped
	}
	if len(args) > 0 && args[0] == "service" {
		os.Exit(homeservice.Command(args[1:]))
	}
	if len(args) > 0 && args[0] == "mcp" {
		os.Exit(mcpserver.Command(args[1:]))
	}
	if len(args) == 1 && args[0] == "supervise" {
		fmt.Fprintln(os.Stderr, "relay supervise is deprecated; watcher ownership moves to relay service run after unit migration")
		os.Exit(homeservice.Command([]string{"watcher", "run"}))
	}
	if len(args) > 0 && args[0] == "viz-broker" {
		os.Exit(vizbroker.Command(args[1:]))
	}
	if len(args) > 1 && args[0] == "viz" && args[1] == "authorize" {
		os.Exit(vizbroker.AuthorizeCommand(args[2:]))
	}
	if len(args) > 1 && args[0] == "viz" && args[1] == "serve" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: relay viz serve")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := cmuxviz.New().Follow(ctx, true); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	app := cli.New()
	os.Exit(app.Run(args))
}
