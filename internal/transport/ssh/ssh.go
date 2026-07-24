// Package ssh implements ports.Transport using the system ssh client.
package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Transport is an SSH-backed remote transport.
type Transport struct {
	Host string
}

func New(host string) *Transport {
	return &Transport{Host: host}
}

func (t *Transport) ID() string { return t.Host }

func (t *Transport) sshBase(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		t.Host,
	}
	base = append(base, args...)
	return exec.CommandContext(ctx, "ssh", base...)
}

func (t *Transport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	remote := command
	if cwd != "" {
		remote = fmt.Sprintf("cd %s && %s", remotePathExpr(cwd), command)
	}
	cmd := t.sshBase(ctx, remote)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (t *Transport) RunStream(ctx context.Context, cwd, command string, w io.Writer) error {
	remote := command
	if cwd != "" {
		remote = fmt.Sprintf("cd %s && %s", remotePathExpr(cwd), command)
	}
	// Keepalive on ONE long-lived stream (subscribe) — not new connections.
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=4",
		t.Host,
		remote,
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func (t *Transport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	stdout, stderr, err := t.Run(ctx, "", fmt.Sprintf("cat %s", remotePathExpr(path)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (%s)", path, err, strings.TrimSpace(stderr))
	}
	return []byte(stdout), nil
}

func (t *Transport) WriteFile(ctx context.Context, path string, data []byte, mode string) error {
	if mode == "" {
		mode = "644"
	}
	expr := remotePathExpr(path)
	script := fmt.Sprintf(
		`mkdir -p "$(dirname %s)" && cat > %s && chmod %s %s`,
		expr, expr, mode, expr,
	)
	cmd := t.sshBase(ctx, script)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write %s: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (t *Transport) Interactive(ctx context.Context, command string) error {
	args := []string{"-t", t.Host, command}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// remotePathExpr returns a shell expression for a remote path.
// ~/… becomes "$HOME/…" so expansion works; absolute paths are single-quoted.
func remotePathExpr(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		rest := strings.ReplaceAll(p[2:], `"`, `\"`)
		return `"$HOME/` + rest + `"`
	}
	return shellQuote(p)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
