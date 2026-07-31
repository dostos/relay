// Package ssh implements ports.Transport using the system ssh client.
package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// Transport is an SSH-backed remote transport.
type Transport struct {
	Host string
	// Stderr overrides the interactive session's stderr (nil → os.Stderr).
	// Used by resume reconnect to filter ssh disconnect chatter.
	Stderr io.Writer
}

const maxStreamStderrBytes = 8 * 1024

func New(host string) *Transport {
	return &Transport{Host: host}
}

// SetStderr implements an optional stderr override for Interactive.
func (t *Transport) SetStderr(w io.Writer) { t.Stderr = w }

func (t *Transport) ID() string { return t.Host }

func controlPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ssh control path: %w", err)
	}
	dir := filepath.Join(home, ".ssh", "relay-cm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ssh control path: %w", err)
	}
	// %C is a hash of %l%h%p%r — short and safe for path length limits.
	return filepath.Join(dir, "%C"), nil
}

// controlOpts returns shared ControlMaster settings (single source for all SSH launches).
func controlOpts() ([]string, error) {
	cp, err := controlPath()
	if err != nil {
		return nil, err
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + cp,
		"-o", "ControlPersist=60",
	}, nil
}

func (t *Transport) sshBase(ctx context.Context, extra ...string) (*exec.Cmd, error) {
	ctrl, err := controlOpts()
	if err != nil {
		return nil, err
	}
	base := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
	}
	base = append(base, ctrl...)
	base = append(base, t.Host)
	base = append(base, extra...)
	return exec.CommandContext(ctx, "ssh", base...), nil
}

func (t *Transport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	remote := command
	if cwd != "" {
		expr, err := shellquote.PathExpr(cwd)
		if err != nil {
			return "", "", err
		}
		remote = fmt.Sprintf("cd %s && %s", expr, command)
	}
	cmd, err := t.sshBase(ctx, remote)
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (t *Transport) RunStream(ctx context.Context, cwd, command string, w io.Writer) error {
	remote := command
	if cwd != "" {
		expr, err := shellquote.PathExpr(cwd)
		if err != nil {
			return err
		}
		remote = fmt.Sprintf("cd %s && %s", expr, command)
	}
	ctrl, err := controlOpts()
	if err != nil {
		return err
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=4",
	}
	args = append(args, ctrl...)
	args = append(args, t.Host, remote)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = w
	// Event streams are JSON-lines on stdout. Keeping stderr out of that stream
	// prevents reconnect failures from being rendered as repeated fake events or
	// raw terminal chatter. Return the final SSH diagnostic to the caller so it
	// can present one structured error instead.
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return streamError(t.Host, err, stderr.String())
	}
	return nil
}

// tailBuffer retains only the end of a stream. SSH diagnostics arrive at the
// end of stderr, so this keeps error reporting useful without allowing a
// broken or hostile remote command to grow the local process unboundedly.
type tailBuffer struct {
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= maxStreamStderrBytes {
		b.data = append(b.data[:0], p[n-maxStreamStderrBytes:]...)
		return n, nil
	}
	excess := len(b.data) + n - maxStreamStderrBytes
	if excess > 0 {
		b.data = append(b.data[:0], b.data[excess:]...)
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.data) }

func streamError(host string, err error, stderr string) error {
	detail := lastNonEmptyLine(stderr)
	if detail == "" {
		return fmt.Errorf("ssh stream to %s: %w", host, err)
	}
	return fmt.Errorf("ssh stream to %s: %s", host, detail)
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func (t *Transport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	expr, err := shellquote.PathExpr(path)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := t.Run(ctx, "", fmt.Sprintf("cat %s", expr))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (%s)", path, err, strings.TrimSpace(stderr))
	}
	return []byte(stdout), nil
}

func (t *Transport) WriteFile(ctx context.Context, path string, data []byte, mode string) error {
	if mode == "" {
		mode = "644"
	}
	expr, err := shellquote.PathExpr(path)
	if err != nil {
		return err
	}
	script := fmt.Sprintf(
		`mkdir -p "$(dirname %s)" && cat > %s && chmod %s %s`,
		expr, expr, mode, expr,
	)
	cmd, err := t.sshBase(ctx, script)
	if err != nil {
		return err
	}
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write %s: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (t *Transport) Interactive(ctx context.Context, command string) error {
	ctrl, err := controlOpts()
	if err != nil {
		return err
	}
	args := append([]string{}, ctrl...)
	args = append(args, "-t", t.Host, command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	stderr := t.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr
	return cmd.Run()
}

func (t *Transport) InteractiveCommand(remoteCmd string) string {
	return fmt.Sprintf("ssh -t %s -- %s", t.Host, remoteCmd)
}
