package ui

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
)

// SSHNoiseFilter wraps a writer and drops OpenSSH client disconnect chatter
// that otherwise scrolls past an in-place reconnect status line.
type SSHNoiseFilter struct {
	W    io.Writer
	mu   sync.Mutex
	buf  []byte
	last string
}

func (f *SSHNoiseFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i+1]
		f.buf = f.buf[i+1:]
		if isSSHNoise(string(line)) {
			f.last = strings.TrimSpace(string(line))
			continue
		}
		if _, err := f.W.Write(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Flush writes any buffered non-noise remnant (call when the SSH child exits).
func (f *SSHNoiseFilter) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buf) == 0 {
		return nil
	}
	line := string(f.buf)
	f.buf = nil
	if isSSHNoise(line) {
		f.last = strings.TrimSpace(line)
		return nil
	}
	_, err := f.W.Write([]byte(line))
	return err
}

// LastDiagnostic returns the latest suppressed OpenSSH diagnostic. It is intended
// for an in-place reconnect status, where showing one current error is useful
// but writing every failed attempt would corrupt the status line.
func (f *SSHNoiseFilter) LastDiagnostic() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// OpenSSH-shaped client diagnostics only (prefix/anchored). Avoids dropping
// remote app stderr that merely mentions "connection closed" in prose.
var (
	reSSHConnClosed = regexp.MustCompile(`^Connection closed by (\S+) port \d+$`)
	reSSHShared     = regexp.MustCompile(`^Shared connection to \S+ closed\.$`)
	reSSHReadReset  = regexp.MustCompile(`^Read from remote host \S+: Connection reset`)
	reSSHClientLoop = regexp.MustCompile(`^client_loop:`)
	reSSHConnect    = regexp.MustCompile(`^ssh: connect to host \S+(?: port \d+)?: `)
	reSSHForward    = regexp.MustCompile(`^(?:Warning|Error): remote port forwarding failed for listen path `)
)

func isSSHNoise(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" {
		return false
	}
	switch {
	case reSSHConnClosed.MatchString(s):
		return true
	case reSSHShared.MatchString(s):
		return true
	case reSSHReadReset.MatchString(s):
		return true
	case reSSHClientLoop.MatchString(s):
		return true
	case reSSHConnect.MatchString(s):
		return true
	case reSSHForward.MatchString(s):
		return true
	default:
		return false
	}
}
