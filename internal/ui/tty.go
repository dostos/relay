// Package ui provides human-facing terminal status helpers for relay.
// JSON / --json paths must not use these; they are for interactive stderr only.
package ui

import (
	"os"

	"golang.org/x/term"
)

// IsTTY reports whether f is an interactive terminal (not merely a char device).
func IsTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
