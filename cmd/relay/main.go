package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dostos/relay/internal/cli"
	"github.com/dostos/relay/internal/compat"
	"github.com/dostos/relay/internal/homeservice"
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
	app := cli.New()
	os.Exit(app.Run(args))
}
