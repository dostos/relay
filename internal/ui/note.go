package ui

import (
	"fmt"
	"io"
	"os"
)

// Note writes a quiet one-line human notice to stderr (or w).
// Form: "relay · <msg>"
func Note(msg string) {
	NoteTo(os.Stderr, msg)
}

// NoteTo is Note with an explicit writer (tests).
func NoteTo(w io.Writer, msg string) {
	if w == nil {
		w = os.Stderr
	}
	if f, ok := w.(*os.File); ok && IsTTY(f) {
		fmt.Fprintf(w, "\033[2mrelay · %s\033[0m\n", msg)
		return
	}
	fmt.Fprintf(w, "relay · %s\n", msg)
}

// Warn writes a human warning: "relay ! <msg>"
func Warn(msg string) {
	WarnTo(os.Stderr, msg)
}

// WarnTo is Warn with an explicit writer.
func WarnTo(w io.Writer, msg string) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "relay ! %s\n", msg)
}

// Done writes a success notice: "relay ✓ <msg>" (ASCII fallback: "relay ok")
func Done(msg string) {
	DoneTo(os.Stderr, msg)
}

// DoneTo is Done with an explicit writer.
func DoneTo(w io.Writer, msg string) {
	if w == nil {
		w = os.Stderr
	}
	if f, ok := w.(*os.File); ok && IsTTY(f) {
		fmt.Fprintf(w, "relay \033[32m✓\033[0m %s\n", msg)
		return
	}
	fmt.Fprintf(w, "relay ok %s\n", msg)
}
