package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dostos/relay/internal/coord"
)

// RelayRepo finds the checkout the installed relay was built from
// (RELAY_REPO, else ~/dev/relay).
func RelayRepo() (string, error) {
	if v := strings.TrimSpace(os.Getenv("RELAY_REPO")); v != "" {
		return v, nil
	}
	for _, c := range []string{filepath.Join(os.Getenv("HOME"), "dev", "relay"), "."} {
		if _, err := os.Stat(filepath.Join(c, "cmd", "relay")); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("cannot find the relay checkout (set RELAY_REPO)")
}

// BuildRelayBinary cross-compiles the exact commit this relay was built from
// for goos/goarch into dir and returns the binary path. It refuses dirty or
// unstamped builds, so a fleet host's build id always names a commit.
func BuildRelayBinary(ctx context.Context, goos, goarch, dir string) (string, error) {
	repo, err := RelayRepo()
	if err != nil {
		return "", err
	}
	src, cleanup, err := exactBuildWorktree(ctx, repo, coord.Build)
	if err != nil {
		return "", fmt.Errorf("%w (reinstall the hub from a clean checkout: cd %s && git pull --ff-only && ./install.sh)", err, repo)
	}
	defer cleanup()
	goBin, err := bootstrapGoBinary()
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, fmt.Sprintf("relay-%s-%s", goos, goarch))
	cmd := exec.CommandContext(ctx, goBin, "build", "-trimpath", "-ldflags", "-X github.com/dostos/relay/internal/coord.Build="+coord.Build, "-o", out, "./cmd/relay")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build relay for %s/%s: %w (%s)", goos, goarch, err, strings.TrimSpace(string(b)))
	}
	return out, nil
}

// GoPlatform maps `uname -s` / `uname -m` to GOOS/GOARCH.
func GoPlatform(unameS, unameM string) (string, string, error) {
	var goos string
	switch strings.ToLower(strings.TrimSpace(unameS)) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported OS %q", unameS)
	}
	goarch := mapUnameToGoarch(strings.TrimSpace(unameM))
	if goarch == "" {
		return "", "", fmt.Errorf("unsupported arch %q", unameM)
	}
	return goos, goarch, nil
}

// KnownHosts lists the relay hosts this control host has profiles cached
// for (~/.local/state/relay/hosts/*.json), sorted.
func KnownHosts() []string {
	entries, err := os.ReadDir(ProfileCacheDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}
