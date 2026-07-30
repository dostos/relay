package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Status is a single-line animated status for TTY stderr.
// Non-TTY: Wait emits one line for the whole reconnect phase (no per-tick spam).
type Status struct {
	w      io.Writer
	tty    bool
	fd     int
	mu     sync.Mutex
	frame  int
	active bool
	last   string
}

// NewStatus writes animated status to stderr (TTY) or plain lines (pipe).
func NewStatus() *Status {
	return NewStatusTo(os.Stderr)
}

// NewStatusTo is NewStatus with an explicit writer.
func NewStatusTo(w io.Writer) *Status {
	if w == nil {
		w = os.Stderr
	}
	fd := -1
	tty := false
	if f, ok := w.(*os.File); ok {
		fd = int(f.Fd())
		tty = IsTTY(f)
	}
	return &Status{w: w, tty: tty, fd: fd}
}

// Render updates the status line. On TTY it rewrites in place (width-capped).
// On non-TTY it writes one newline per distinct text.
func (s *Status) Render(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	spin := spinnerFrames[s.frame%len(spinnerFrames)]
	s.frame++
	line := fmt.Sprintf("relay %s  %s", spin, text)
	if !s.tty {
		if text == s.last {
			return
		}
		fmt.Fprintln(s.w, line)
		s.last = text
		return
	}
	if cols := s.termCols(); cols > 0 {
		line = Truncate(line, cols)
	}
	fmt.Fprintf(s.w, "\r\033[2K%s", line)
	s.last = text
}

// Clear erases the in-place status line (TTY only).
func (s *Status) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	s.last = ""
	if s.tty {
		fmt.Fprint(s.w, "\r\033[2K")
	}
}

// Wait animates for delay (or until cancel).
// TTY: spinner + countdown ticks. Non-TTY: one line for the whole phase.
func (s *Status) Wait(delay time.Duration, paint func(left time.Duration) string, cancel <-chan struct{}) bool {
	if delay <= 0 {
		return true
	}
	if !s.tty {
		s.Render(paint(delay))
		select {
		case <-cancel:
			return false
		case <-time.After(delay):
			return true
		}
	}
	deadline := time.Now().Add(delay)
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	for {
		left := time.Until(deadline)
		if left <= 0 {
			s.Render(paint(0))
			return true
		}
		s.Render(paint(left))
		select {
		case <-cancel:
			return false
		case <-tick.C:
		}
	}
}

func (s *Status) termCols() int {
	if s.fd < 0 {
		return 72
	}
	w, _, err := term.GetSize(s.fd)
	if err != nil || w <= 0 {
		return 72
	}
	return w
}

// FormatDuration shortens a duration for status lines (e.g. "2s", "1m4s").
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	m := int(d / time.Minute)
	sec := int((d % time.Minute) / time.Second)
	if sec == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, sec)
}

// Truncate caps a string to max runes, appending an ellipsis when cut.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return string(r[0])
	}
	return string(r[:max-1]) + "…"
}

// AttemptLabel returns "attempt" or "attempts".
func AttemptLabel(n int) string {
	if n == 1 {
		return "attempt"
	}
	return "attempts"
}

// JoinStatus builds "a · b · c" skipping empty parts.
func JoinStatus(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "  ·  ")
}
