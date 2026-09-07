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
	"time"

	"github.com/dostos/relay/internal/shellquote"
)

// Transport is an SSH-backed remote transport.
type Transport struct {
	Host        string
	port        int
	identity    string
	strictBatch bool
	// Stderr overrides the interactive session's stderr (nil → os.Stderr).
	// Used by resume reconnect to filter ssh disconnect chatter.
	Stderr              io.Writer
	reverseRemoteSocket string
	reverseLocalSocket  string
	// controlPathCache holds the %C-expanded multiplexing socket path.
	controlPathCache string
}

// ConfigureEndpoint pins projection-owned SSH policy for every reconnect.
func (t *Transport) ConfigureEndpoint(user string, port int, identity string) error {
	if strings.HasPrefix(t.Host, "-") || strings.ContainsAny(t.Host+user+identity, "\r\n\x00") || strings.HasPrefix(user, "-") || port < 0 || port > 65535 {
		return fmt.Errorf("invalid visualization SSH endpoint")
	}
	if user != "" {
		t.Host = user + "@" + t.Host
	}
	t.port, t.identity, t.strictBatch = port, identity, true
	return nil
}

const maxStreamStderrBytes = 8 * 1024

func New(host string) *Transport {
	return &Transport{Host: host}
}

// SetStderr implements an optional stderr override for Interactive.
func (t *Transport) SetStderr(w io.Writer) { t.Stderr = w }

func (t *Transport) SetReverseUnixForward(remoteSocket, localSocket string) {
	t.reverseRemoteSocket = remoteSocket
	t.reverseLocalSocket = localSocket
}

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
		// Keepalives belong here, on the shared options, because with
		// ControlMaster=auto the *master* inherits the options of whichever
		// launch happened to create it first. A master born without them
		// cannot notice a dead peer: it blocks forever holding the socket,
		// and every later client that multiplexes onto it blocks too.
		// ConnectTimeout does not help once the control socket exists — it
		// only bounds a fresh TCP connect, not the handshake with a wedged
		// master. Setting them at the single source makes the master mortal
		// no matter which code path reaches the host first.
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "TCPKeepAlive=yes",
	}, nil
}

// controlProbeTimeout bounds every multiplexing-socket probe. It must be
// short and it must exist: `ssh -O check` against a wedged master hangs
// exactly like the calls it is meant to protect.
const controlProbeTimeout = 3 * time.Second

// resolvedControlPath expands the %C token in the shared ControlPath to a
// concrete file, so a wedged socket can be removed when the master refuses
// to exit. The value depends only on the endpoint, so it is cached.
func (t *Transport) resolvedControlPath(ctx context.Context) (string, error) {
	if t.controlPathCache != "" {
		return t.controlPathCache, nil
	}
	ctrl, err := controlOpts()
	if err != nil {
		return "", err
	}
	gctx, cancel := context.WithTimeout(ctx, controlProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(gctx, "ssh", append(ctrl, "-G", "--", t.Host)...).Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "controlpath "); ok {
			t.controlPathCache = strings.TrimSpace(rest)
			return t.controlPathCache, nil
		}
	}
	return "", fmt.Errorf("ssh -G %s: no controlpath", t.Host)
}

// ensureLiveControlMaster clears a wedged multiplexing master before it can
// swallow another call. Killing the ssh *client* on a context deadline does
// not help on its own: the master survives, so the next attempt hangs
// identically and the hung clients pile up. This reaps the master itself.
func (t *Transport) ensureLiveControlMaster(ctx context.Context) {
	cp, err := t.resolvedControlPath(ctx)
	if err != nil || cp == "" {
		return
	}
	if _, err := os.Stat(cp); err != nil {
		return // no master yet — nothing to reap
	}
	ctrl, err := controlOpts()
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, controlProbeTimeout)
	healthy := exec.CommandContext(cctx, "ssh", append(ctrl, "-O", "check", t.Host)...).Run() == nil
	cancel()
	if healthy {
		return
	}
	ectx, cancel2 := context.WithTimeout(ctx, controlProbeTimeout)
	_ = exec.CommandContext(ectx, "ssh", append(ctrl, "-O", "exit", t.Host)...).Run()
	cancel2()
	// A master that answers neither check nor exit still owns the socket
	// file; dropping it forces the next launch to dial fresh.
	if _, err := os.Stat(cp); err == nil {
		_ = os.Remove(cp)
	}
}

func (t *Transport) sshBase(ctx context.Context, extra ...string) (*exec.Cmd, error) {
	ctrl, err := controlOpts()
	if err != nil {
		return nil, err
	}
	t.ensureLiveControlMaster(ctx)
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
	t.ensureLiveControlMaster(ctx)
	args := []string{
		"-o", "BatchMode=yes",
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
	if t.reverseRemoteSocket != "" && t.reverseLocalSocket != "" {
		if err := t.prepareReverseUnixForward(ctx); err != nil {
			return err
		}
	}
	args, err := t.interactiveArgs(command)
	if err != nil {
		return err
	}
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

// prepareReverseUnixForward removes only this session's stale socket before a
// dedicated attach reconnects. StreamLocalBindUnlink is not honored reliably
// by every server for remote stream-local forwards.
func (t *Transport) prepareReverseUnixForward(ctx context.Context) error {
	cleanup, err := reverseSocketCleanupCommand(t.reverseRemoteSocket)
	if err != nil {
		return err
	}
	cmd, err := t.sshBase(ctx, cleanup)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clear stale relay bridge socket on %s: %w (%s)", t.Host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func reverseSocketCleanupCommand(remoteSocket string) (string, error) {
	expr, err := shellquote.PathExpr(remoteSocket)
	if err != nil {
		return "", err
	}
	return "rm -f -- " + expr, nil
}

func (t *Transport) interactiveArgs(command string) ([]string, error) {
	var args []string
	if t.strictBatch {
		args = append(args, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes")
		if t.port > 0 {
			args = append(args, "-p", fmt.Sprint(t.port))
		}
		if t.identity != "" {
			identity := t.identity
			if strings.HasPrefix(identity, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					identity = filepath.Join(home, strings.TrimPrefix(identity, "~/"))
				}
			}
			args = append(args, "-o", "IdentitiesOnly=yes", "-i", identity)
		}
	}
	if t.reverseRemoteSocket != "" && t.reverseLocalSocket != "" {
		// A dedicated connection owns the reverse stream-local forwarding. Using
		// a shared ControlMaster here would make forward lifetime depend on an
		// unrelated SSH client and can leave a stale remote socket on reconnect.
		args = []string{
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
			"-o", "ServerAliveInterval=30",
			"-o", "ServerAliveCountMax=4",
			"-o", "ExitOnForwardFailure=yes",
			"-o", "StreamLocalBindUnlink=yes",
			"-o", "StreamLocalBindMask=0177",
			"-R", t.reverseRemoteSocket + ":" + t.reverseLocalSocket,
		}
	} else {
		ctrl, err := controlOpts()
		if err != nil {
			return nil, err
		}
		args = append(args, ctrl...)
	}
	args = append(args, "-t", t.Host, command)
	return args, nil
}

func (t *Transport) InteractiveCommand(remoteCmd string) string {
	return fmt.Sprintf("ssh -t %s -- %s", t.Host, remoteCmd)
}
