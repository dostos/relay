// Package local implements ports.Transport by running commands on this
// machine, for sessions whose HostID is core.LocalHostID.
//
// Without it every transport went through SSH, so a local session resolved to
// the literal hostname "local" and failed with "could not resolve hostname".
// That made local panes — including root manager panes — impossible to capture
// or send to, so an apex could govern subtrees it structurally could not
// observe.
//
// Commands still go through a shell rather than exec'ing argv directly. relay
// generates POSIX sh (tmux hook scripts, `cd X && …`, `cat > f`), and the SSH
// transport gets a remote login shell for free — including tilde and $HOME
// expansion. Running argv directly here would silently change the meaning of
// every path relay passes, so the shell stays.
package local

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// Transport runs commands on the local machine.
type Transport struct {
	// Stderr overrides the interactive session's stderr (nil → os.Stderr),
	// matching the SSH transport's SetStderr contract.
	Stderr io.Writer
}

// New returns a transport bound to this machine.
func New() *Transport { return &Transport{} }

// SetStderr implements the optional stderr override used by Interactive.
func (t *Transport) SetStderr(w io.Writer) { t.Stderr = w }

// ID reports the host identity, matching core.LocalHostID.
func (t *Transport) ID() string { return "local" }

// shellArgs builds the sh invocation, applying cwd the same way the SSH
// transport does so both produce identical semantics for the same input.
func shellArgs(cwd, command string) ([]string, error) {
	full := command
	if cwd != "" {
		expr, err := shellquote.PathExpr(cwd)
		if err != nil {
			return nil, err
		}
		full = fmt.Sprintf("cd %s && %s", expr, command)
	}
	return []string{"-c", full}, nil
}

func (t *Transport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	args, err := shellArgs(cwd, command)
	if err != nil {
		return "", "", err
	}
	cmd := exec.CommandContext(ctx, "sh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (t *Transport) RunStream(ctx context.Context, cwd, command string, w io.Writer) error {
	args, err := shellArgs(cwd, command)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", args...)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write %s: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (t *Transport) Interactive(ctx context.Context, command string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	stderr := t.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr
	return cmd.Run()
}

// InteractiveCommand returns the command unchanged. The SSH transport wraps it
// in "ssh -t HOST --" to cross a network; there is no hop to cross here, and
// wrapping it would reintroduce the "hostname local" failure this package
// exists to remove.
func (t *Transport) InteractiveCommand(remoteCmd string) string { return remoteCmd }
