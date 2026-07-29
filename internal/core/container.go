package core

import (
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// ContainerRef is the resolved container binding carried on a Session.
type ContainerRef struct {
	Runtime string `json:"runtime"`        // docker (default)
	Ref     string `json:"ref"`            // container name/id to `docker exec` into
	CWD     string `json:"cwd,omitempty"`  // working dir inside the container
	User    string `json:"user,omitempty"` // exec uid[:gid]
	Home    string `json:"home,omitempty"` // container $HOME (for cred resolution)
}

// ContainerExec builds a host-side shell command that runs `inner` inside the
// container. tty=true adds -it and an interactive login shell (for a tmux pane);
// tty=false uses -i and a non-interactive login shell (ad-hoc capture / probes).
// cwd is applied inside the shell so the container's own PATH is present.
func ContainerExec(runtime string, ref ContainerRef, inner string, tty bool) (string, error) {
	if ref.Ref == "" {
		return "", fmt.Errorf("container ref required")
	}
	if runtime == "" {
		runtime = "docker"
	}
	execFlag, shell := "-i", "bash -lc"
	if tty {
		execFlag, shell = "-it", "bash -ilc"
	}
	args := []string{runtime, "exec", execFlag}
	if ref.User != "" {
		args = append(args, "-u", shellquote.Quote(ref.User))
	}
	if ref.Home != "" {
		args = append(args, "-e", shellquote.Quote("HOME="+ref.Home))
	}
	args = append(args, shellquote.Quote(ref.Ref))

	script := inner
	if ref.CWD != "" {
		cd, err := shellquote.PathExpr(ref.CWD)
		if err != nil {
			return "", err
		}
		if tty {
			script = fmt.Sprintf("cd %s; exec %s", cd, inner)
		} else {
			script = fmt.Sprintf("cd %s && %s", cd, inner)
		}
	} else if tty {
		script = "exec " + inner
	}
	args = append(args, shell, shellquote.Quote(script))
	return strings.Join(args, " "), nil
}
