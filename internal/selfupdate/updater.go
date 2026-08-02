// Package selfupdate performs transactional runtime upgrades of relay and
// relayd. Bootstrap/service registration remains outside this package.
package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const buildVariable = "github.com/dostos/relay/internal/coord.Build"

type Plan struct {
	Repo       string
	Remote     string
	Branch     string
	InstallDir string
	StateDir   string
}

type Result struct {
	Build string
}

func Apply(ctx context.Context, plan Plan) (*Result, error) {
	if strings.TrimSpace(plan.Repo) == "" {
		return nil, fmt.Errorf("update repository required")
	}
	if plan.Remote == "" {
		plan.Remote = "origin"
	}
	if plan.Branch == "" {
		plan.Branch = "master"
	}
	if plan.InstallDir == "" {
		home, _ := os.UserHomeDir()
		plan.InstallDir = filepath.Join(home, ".local", "bin")
	}
	if plan.StateDir == "" {
		home, _ := os.UserHomeDir()
		plan.StateDir = filepath.Join(home, ".local", "state", "relay")
	}
	if err := os.MkdirAll(plan.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(plan.InstallDir, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(plan.StateDir, "update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("relay update already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if out, err := git(ctx, plan.Repo, "status", "--porcelain"); err != nil {
		return nil, err
	} else if strings.TrimSpace(out) != "" {
		return nil, fmt.Errorf("refuse update: relay worktree is dirty")
	}
	if branch, err := git(ctx, plan.Repo, "branch", "--show-current"); err != nil {
		return nil, err
	} else if strings.TrimSpace(branch) != plan.Branch {
		return nil, fmt.Errorf("refuse update: checkout is on %q, policy requires %q", strings.TrimSpace(branch), plan.Branch)
	}
	if _, err := git(ctx, plan.Repo, "fetch", plan.Remote, plan.Branch); err != nil {
		return nil, err
	}
	ref := plan.Remote + "/" + plan.Branch
	if _, err := git(ctx, plan.Repo, "merge-base", "--is-ancestor", "HEAD", ref); err != nil {
		return nil, fmt.Errorf("refuse non-fast-forward update to %s", ref)
	}
	build, err := git(ctx, plan.Repo, "rev-parse", "--short", ref)
	if err != nil {
		return nil, err
	}
	build = strings.TrimSpace(build)

	worktree, err := os.MkdirTemp(plan.StateDir, "update-src-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(worktree)
	if _, err := git(ctx, plan.Repo, "worktree", "add", "--detach", worktree, ref); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = git(context.Background(), plan.Repo, "worktree", "remove", "--force", worktree)
	}()
	stage, err := os.MkdirTemp(plan.InstallDir, ".relay-update-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	for _, item := range []struct{ name, pkg string }{{"relay", "./cmd/relay"}, {"relayd", "./cmd/relayd"}} {
		cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X "+buildVariable+"="+build, "-o", filepath.Join(stage, item.name), item.pkg)
		cmd.Dir = worktree
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build %s: %w (%s)", item.name, err, strings.TrimSpace(string(output)))
		}
		if err := verifyBuild(ctx, filepath.Join(stage, item.name), build); err != nil {
			return nil, err
		}
	}
	if _, err := git(ctx, plan.Repo, "merge", "--ff-only", ref); err != nil {
		return nil, err
	}
	if err := installPair(stage, plan.InstallDir); err != nil {
		return nil, err
	}
	return &Result{Build: build}, nil
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	argv := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func verifyBuild(ctx context.Context, binary, want string) error {
	cmd := exec.CommandContext(ctx, binary, "build")
	cmd.Env = append(os.Environ(), "RELAY_BRIDGE_LOCAL_INVOKE=1")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != want {
		return fmt.Errorf("staged %s build verification failed: got %q want %q (%v)", filepath.Base(binary), strings.TrimSpace(string(out)), want, err)
	}
	return nil
}

func installPair(stage, installDir string) error {
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	names := []string{"relay", "relayd"}
	var backed, installed []string
	rollback := func() {
		for _, name := range installed {
			_ = os.Remove(filepath.Join(installDir, name))
		}
		for _, name := range backed {
			_ = os.Rename(filepath.Join(installDir, name+".previous"), filepath.Join(installDir, name))
		}
	}
	for _, name := range names {
		dst, previous := filepath.Join(installDir, name), filepath.Join(installDir, name+".previous")
		_ = os.Remove(previous)
		if _, err := os.Stat(dst); err == nil {
			if err := os.Rename(dst, previous); err != nil {
				rollback()
				return err
			}
			backed = append(backed, name)
		}
		if err := os.Rename(filepath.Join(stage, name), dst); err != nil {
			rollback()
			return err
		}
		installed = append(installed, name)
	}
	return nil
}
