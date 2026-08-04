// relayd is a temporary argv-compatibility shim for the primary relay binary.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/dostos/relay/internal/compat"
)

func main() {
	mapped, ok := compat.MapRelayd(os.Args[1:])
	if !ok {
		fmt.Fprintln(os.Stderr, "relayd compatibility command is unavailable; use relay --help")
		os.Exit(2)
	}
	bin := relayBinary()
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	fmt.Fprintln(os.Stderr, "relayd is deprecated; use relay")
	if err := syscall.Exec(bin, append([]string{bin}, mapped...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func relayBinary() string {
	if value := os.Getenv("RELAY_BIN"); value != "" {
		return value
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "relay")
	}
	return "relay"
}
