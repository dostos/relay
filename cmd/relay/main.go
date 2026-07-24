package main

import (
	"os"

	"github.com/dostos/relay/internal/cli"
)

func main() {
	app := cli.New()
	os.Exit(app.Run(os.Args[1:]))
}
