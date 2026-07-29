# Container Handoffs (MVP core) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `relay agent start --container NAME` — hand off an agent goal into a docker container on a host, with the agent running via `docker exec` (tmux stays on the host), a resolved working dir, and a verify gate that refuses the handoff if the agent can't actually run in the container.

**Architecture:** A container is a **session attribute**, not a new Transport or HostID. The base ssh transport and host tmux are unchanged; a `docker exec` wrapper is applied only to (a) the agent launch command sent into the holding shell and (b) ad-hoc `Exec`. Container targets are declared in the remote `host.yaml` `containers:` list and resolved at launch. Before the handoff starts, relay runs the agent's `--version` inside the container and maps known failure signatures (glibc, root, permission, missing binary, hook) to actionable errors.

**Tech Stack:** Go (stdlib `testing`), `gopkg.in/yaml.v3` (already a dep), the existing `internal/shellquote` package. No new dependencies.

## Global Constraints

- Module path: `github.com/dostos/relay`. Package under test: `internal/core` (`package core`).
- Tests are Go stdlib `testing` (`go test ./internal/core/...`); no test framework deps.
- Runtime supported this plan: **docker only** (the runtime verb is a single field; podman is a later change).
- Do **not** modify `internal/persist/tmux`, `internal/coord`, or `internal/transport/ssh` — the wrap is built in `internal/core` and passed as a command string.
- All shell interpolation goes through `internal/shellquote` (`Quote`, `PathExpr`) — never raw string concatenation of untrusted values.
- Commit style (from AGENTS.md): `[module] type: message`; end each commit body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Work stays on branch `feat/container-handoffs`.
- After each task: `gofmt -w` the changed files and ensure `go build ./...` passes.

## File Structure

- **Create** `internal/core/container.go` — `ContainerSpec` (host.yaml shape), `ContainerRef` (resolved binding on a `Session`), `ContainerExec` (the `docker exec` wrap builder), and `ClassifyContainerVerify` (stderr→guidance). One file, one responsibility: everything container-specific and mostly pure.
- **Create** `internal/core/container_test.go` — unit tests for the above (all pure, no ssh/registry).
- **Modify** `internal/core/profile.go` — add `Containers []ContainerSpec` to `HostProfile`; add `ResolveContainer`, `(*ContainerSpec).ResolveCWD`, and factor a shared `matchPathMap` helper (used by the existing `ResolveRemoteCWD` too).
- **Modify** `internal/core/types.go` — add `Container *ContainerRef` to `Session`.
- **Modify** `internal/core/session.go` — factor `execPlan` and have `Exec` wrap when the session has a container.
- **Modify** `internal/core/handoff.go` — add `Container` to `HandoffOpts`; resolve + wrap + verify in `Launch`; add `verifyContainerAgent`.
- **Modify** `internal/cli/app.go` — parse `--container` on `agent start` and pass it into `HandoffOpts`.

---

### Task 1: `ContainerExec` wrap builder

**Files:**
- Create: `internal/core/container.go`
- Test: `internal/core/container_test.go`

**Interfaces:**
- Produces: `type ContainerRef struct { Runtime, Ref, CWD, User, Home string }`; `func ContainerExec(runtime string, ref ContainerRef, inner string, tty bool) (string, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/core/container_test.go`:

```go
package core

import (
	"strings"
	"testing"
)

func TestContainerExecTTY(t *testing.T) {
	ref := ContainerRef{Ref: "beholder-run", CWD: "/workspace/beholder", User: "1005", Home: "/home/jingyulee"}
	got, err := ContainerExec("docker", ref, "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"docker exec -it", "-u '1005'", "-e 'HOME=/home/jingyulee'",
		"'beholder-run'", "bash -ilc", "exec claude",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestContainerExecNonTTY(t *testing.T) {
	got, err := ContainerExec("", ContainerRef{Ref: "c1", CWD: "/w"}, "ls -la", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "docker exec -i ") || strings.Contains(got, "-it") {
		t.Fatalf("want non-tty exec, got %q", got)
	}
	if !strings.Contains(got, "bash -lc") || strings.Contains(got, "bash -ilc") {
		t.Fatalf("non-tty should use login (non-interactive) shell: %q", got)
	}
	// PathExpr single-quotes the path and the whole inner script is re-quoted for
	// bash -lc, so the unescaped literal never appears — assert decomposed substrings.
	if !strings.Contains(got, "/w") || !strings.Contains(got, "&& ls -la") {
		t.Fatalf("cwd/join wrong: %q", got)
	}
}

func TestContainerExecRequiresRef(t *testing.T) {
	if _, err := ContainerExec("docker", ContainerRef{}, "x", true); err == nil {
		t.Fatal("expected error for empty container ref")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestContainerExec -v`
Expected: FAIL — `undefined: ContainerExec` / `undefined: ContainerRef`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/core/container.go`:

```go
package core

import (
	"fmt"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// ContainerRef is the resolved container binding carried on a Session.
type ContainerRef struct {
	Runtime string `json:"runtime"`         // docker (default)
	Ref     string `json:"ref"`             // container name/id to `docker exec` into
	CWD     string `json:"cwd,omitempty"`   // working dir inside the container
	User    string `json:"user,omitempty"`  // exec uid[:gid]
	Home    string `json:"home,omitempty"`  // container $HOME (for cred resolution)
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/core/ -run TestContainerExec -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/core/container.go internal/core/container_test.go
git add internal/core/container.go internal/core/container_test.go
git commit -m "[core] feat: docker exec wrap builder for container targets" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `ContainerSpec` in host.yaml + resolve

**Files:**
- Modify: `internal/core/container.go` (add `ContainerSpec` + methods)
- Modify: `internal/core/profile.go` (add `Containers` field; factor `matchPathMap`)
- Test: `internal/core/container_test.go`

**Interfaces:**
- Consumes: `PathMapEntry` (from profile.go).
- Produces: `type ContainerSpec struct{ Name, Runtime, Container, Image, User, DefaultCWD, Toolchain, Hooks string; PathMap []PathMapEntry; Expose, Env []string }`; `(*ContainerSpec).RuntimeVerb() string`; `(*ContainerSpec).ResolveCWD(localRepo string) string`; `(*HostProfile).ResolveContainer(name string) (*ContainerSpec, error)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/container_test.go`:

```go
func TestParseContainersAndResolve(t *testing.T) {
	yaml := `
version: 1
host_id: hamburg
agents:
  - name: claude
    command: claude
containers:
  - name: beholder
    runtime: docker
    container: beholder-run
    default_cwd: /workspace
    path_map:
      - match: beholder
        remote_cwd: /workspace/beholder
`
	p, err := ParseHostProfileYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.ResolveContainer("beholder")
	if err != nil {
		t.Fatal(err)
	}
	if c.Container != "beholder-run" || c.RuntimeVerb() != "docker" {
		t.Fatalf("spec wrong: %+v", c)
	}
	if cwd := c.ResolveCWD("/Users/x/dev/beholder"); cwd != "/workspace/beholder" {
		t.Fatalf("path_map cwd %q", cwd)
	}
	if cwd := c.ResolveCWD("/Users/x/dev/other"); cwd != "/workspace" {
		t.Fatalf("default cwd %q", cwd)
	}
	if _, err := p.ResolveContainer("nope"); err == nil {
		t.Fatal("expected miss error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestParseContainersAndResolve -v`
Expected: FAIL — `p.Containers` undefined / `ResolveContainer` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/core/profile.go`, add the field to `HostProfile` (after `PathMap`):

```go
	Containers []ContainerSpec        `yaml:"containers,omitempty" json:"containers,omitempty"`
```

In `internal/core/profile.go`, factor the path-map matcher and reuse it in the existing `ResolveRemoteCWD`. Add:

```go
// matchPathMap returns the remote cwd for a local repo/basename, or ("", false).
func matchPathMap(entries []PathMapEntry, localRepo string) (string, bool) {
	base := filepath.Base(strings.TrimRight(localRepo, string(filepath.Separator)))
	for _, e := range entries {
		m := e.Match
		if m == localRepo || m == base || filepath.Base(m) == base {
			return e.RemoteCWD, true
		}
		if strings.HasSuffix(localRepo, string(filepath.Separator)+m) || strings.HasSuffix(localRepo, "/"+m) {
			return e.RemoteCWD, true
		}
	}
	return "", false
}
```

Replace the body of the existing `ResolveRemoteCWD` with:

```go
func (p *HostProfile) ResolveRemoteCWD(localRepo string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("nil host profile")
	}
	if cwd, ok := matchPathMap(p.PathMap, localRepo); ok {
		return cwd, nil
	}
	return "", fmt.Errorf("no path_map entry for %q on host (configure ~/.config/relay/host.yaml)", localRepo)
}
```

In `internal/core/container.go`, add:

```go
// ContainerSpec declares a container target in a host profile (host.yaml).
type ContainerSpec struct {
	Name       string         `yaml:"name" json:"name"`
	Runtime    string         `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Container  string         `yaml:"container" json:"container"`
	Image      string         `yaml:"image,omitempty" json:"image,omitempty"`
	User       string         `yaml:"user,omitempty" json:"user,omitempty"`
	DefaultCWD string         `yaml:"default_cwd,omitempty" json:"default_cwd,omitempty"`
	Toolchain  string         `yaml:"toolchain,omitempty" json:"toolchain,omitempty"`
	Hooks      string         `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	PathMap    []PathMapEntry `yaml:"path_map,omitempty" json:"path_map,omitempty"`
	Expose     []string       `yaml:"expose,omitempty" json:"expose,omitempty"`
	Env        []string       `yaml:"env,omitempty" json:"env,omitempty"`
}

// RuntimeVerb is the container CLI to invoke (default docker).
func (c *ContainerSpec) RuntimeVerb() string {
	if c == nil || c.Runtime == "" {
		return "docker"
	}
	return c.Runtime
}

// ResolveCWD picks the container working dir for a local repo: path_map first,
// then default_cwd, then "/".
func (c *ContainerSpec) ResolveCWD(localRepo string) string {
	if c == nil {
		return "/"
	}
	if localRepo != "" {
		if cwd, ok := matchPathMap(c.PathMap, localRepo); ok {
			return cwd
		}
	}
	if c.DefaultCWD != "" {
		return c.DefaultCWD
	}
	return "/"
}

// ResolveContainer finds a container spec by name in the host profile.
func (p *HostProfile) ResolveContainer(name string) (*ContainerSpec, error) {
	if p == nil {
		return nil, fmt.Errorf("nil host profile")
	}
	for i := range p.Containers {
		if p.Containers[i].Name == name {
			return &p.Containers[i], nil
		}
	}
	avail := make([]string, 0, len(p.Containers))
	for i := range p.Containers {
		avail = append(avail, p.Containers[i].Name)
	}
	return nil, fmt.Errorf("container %q not in host profile; available: %s", name, strings.Join(avail, ", "))
}
```

Note: `container.go` already imports `fmt` and `strings`; `ResolveContainer` is on `*HostProfile` but can live in `container.go` (same package). `profile.go` already imports `filepath` and `strings`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/core/ -run 'TestParseContainersAndResolve|TestParseAndResolvePathMap' -v`
Expected: PASS (both — the refactor keeps the existing path-map test green).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/core/container.go internal/core/profile.go internal/core/container_test.go
go build ./...
git add internal/core/container.go internal/core/profile.go internal/core/container_test.go
git commit -m "[core] feat: containers: in host.yaml + ResolveContainer/ResolveCWD" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `Session.Container` + ad-hoc `Exec` wrap

**Files:**
- Modify: `internal/core/types.go` (add `Container` field)
- Modify: `internal/core/session.go` (factor `execPlan`, wrap in `Exec`)
- Test: `internal/core/container_test.go`

**Interfaces:**
- Consumes: `ContainerRef`, `ContainerExec` (Task 1).
- Produces: `Session.Container *ContainerRef`; `(*SessionService).execPlan(sess *Session, command string) (cwd, cmd string, err error)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/container_test.go`:

```go
func TestExecPlanWrapsContainer(t *testing.T) {
	s := &SessionService{}
	sess := &Session{Container: &ContainerRef{Ref: "cbox", CWD: "/w"}}
	cwd, cmd, err := s.execPlan(sess, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if cwd != "" {
		t.Fatalf("container exec must not set a host cwd, got %q", cwd)
	}
	if !strings.Contains(cmd, "docker exec -i") || !strings.Contains(cmd, "echo hi") {
		t.Fatalf("exec not wrapped: %q", cmd)
	}
}

func TestExecPlanHostPassthrough(t *testing.T) {
	s := &SessionService{}
	sess := &Session{RemoteCWD: "/home/x"}
	cwd, cmd, err := s.execPlan(sess, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if cwd != "/home/x" || cmd != "ls" {
		t.Fatalf("host passthrough broken: cwd=%q cmd=%q", cwd, cmd)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestExecPlan -v`
Expected: FAIL — `sess.Container` undefined / `s.execPlan` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/core/types.go`, add to `Session` (after `Labels`):

```go
	Container     *ContainerRef       `json:"container,omitempty"`
```

In `internal/core/session.go`, add the planner and route `Exec` through it. Add:

```go
// execPlan resolves the (cwd, command) for an ad-hoc exec. Container sessions
// are wrapped with `docker exec` (no host cwd — the cwd is inside the wrap);
// host sessions pass through with their RemoteCWD.
func (s *SessionService) execPlan(sess *Session, command string) (cwd, cmd string, err error) {
	if sess.Container != nil {
		wrapped, werr := ContainerExec(sess.Container.Runtime, *sess.Container, command, false)
		if werr != nil {
			return "", "", werr
		}
		return "", wrapped, nil
	}
	return sess.RemoteCWD, command, nil
}
```

Replace the tail of `Exec` (the `return t.Run(ctx, sess.RemoteCWD, command)` line) with:

```go
	cwd, cmd, err := s.execPlan(sess, command)
	if err != nil {
		return "", "", err
	}
	return t.Run(ctx, cwd, cmd)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/core/ -run TestExecPlan -v && go build ./...`
Expected: PASS (2 tests); build clean.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/core/types.go internal/core/session.go internal/core/container_test.go
git add internal/core/types.go internal/core/session.go internal/core/container_test.go
git commit -m "[core] feat: carry ContainerRef on Session; wrap ad-hoc exec" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Verify-signature classifier

**Files:**
- Modify: `internal/core/container.go` (add `ClassifyContainerVerify`)
- Test: `internal/core/container_test.go`

**Interfaces:**
- Produces: `func ClassifyContainerVerify(output string) (ok bool, guidance string)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/container_test.go`:

```go
func TestClassifyContainerVerify(t *testing.T) {
	cases := []struct {
		name   string
		output string
		ok     bool
		want   string // substring of guidance when ok==false
	}{
		{"glibc", "node: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.28' not found (required by node)", false, "newer base image"},
		{"root", "--dangerously-skip-permissions cannot be used with root/sudo privileges", false, "non-root user:"},
		{"perm", "bash: line 1: claude: Permission denied", false, "user: uid must match"},
		{"missing", "bash: codex: command not found", false, "provision:"},
		{"hook", "SyntaxError: Unexpected token '.'", false, "hooks: off"},
		{"clean", "2.1.220 (Claude Code)", true, ""},
	}
	for _, c := range cases {
		ok, guidance := ClassifyContainerVerify(c.output)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v (guidance=%q)", c.name, ok, c.ok, guidance)
		}
		if !c.ok && !strings.Contains(guidance, c.want) {
			t.Fatalf("%s: guidance %q missing %q", c.name, guidance, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestClassifyContainerVerify -v`
Expected: FAIL — `undefined: ClassifyContainerVerify`.

- [ ] **Step 3: Write minimal implementation**

In `internal/core/container.go`, add:

```go
// ClassifyContainerVerify inspects combined stdout+stderr from an in-container
// agent probe. It returns ok=false with actionable guidance when a known
// failure signature is present; ok=true means no known failure was detected.
func ClassifyContainerVerify(output string) (ok bool, guidance string) {
	low := strings.ToLower(output)
	switch {
	case strings.Contains(output, "GLIBC_") && strings.Contains(low, "not found"):
		return false, "container libc is older than the host toolchain requires; use a self-contained agent that runs here, or a newer base image (node ≥18 needs glibc ≥2.28)"
	case strings.Contains(low, "cannot be used with root") || strings.Contains(low, "root/sudo"):
		return false, "agent refuses to run as root; set a non-root user: (default is the host owner uid)"
	case strings.Contains(low, "permission denied"):
		return false, "permission denied on the agent binary; user: uid must match the owner of the bound files (the host file-owner uid)"
	case strings.Contains(low, "command not found"):
		return false, "agent binary not present in the container; declare a provision: command or use toolchain: bind"
	case strings.Contains(low, "syntaxerror") || strings.Contains(low, "unexpected token"):
		return false, "agent hook/plugin failed under the container toolchain; set hooks: off (default) or the container node is too old"
	}
	return true, ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/core/ -run TestClassifyContainerVerify -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/core/container.go internal/core/container_test.go
git add internal/core/container.go internal/core/container_test.go
git commit -m "[core] feat: classify container-verify failure signatures" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Wire `--container` through `Launch` + CLI

**Files:**
- Modify: `internal/core/handoff.go` (`HandoffOpts.Container`; resolve/wrap/verify in `Launch`; add `verifyContainerAgent`)
- Modify: `internal/cli/app.go` (parse `--container` on `agent start`)

**Interfaces:**
- Consumes: `ResolveContainer`, `ResolveCWD`, `RuntimeVerb` (Task 2), `ContainerExec` (Task 1), `ClassifyContainerVerify` (Task 4), `AgentSpec.InnerCommand()` (existing, profile.go).
- Produces: `HandoffOpts.Container string`; behavior — an agent handoff with a container runs the agent via `docker exec` and is refused if verify fails.

- [ ] **Step 1: Add the field and helper (no test-first — this is wiring exercised by the manual integration step; the pure pieces it composes are already tested).**

In `internal/core/handoff.go`, add to `HandoffOpts` (after `Name`):

```go
	Container string // optional: container name from host.yaml `containers:`
```

Add the verify helper to `internal/core/handoff.go`:

```go
// verifyContainerAgent runs the agent's --version inside the container and maps
// known failure signatures to an actionable error. Returns nil when the agent
// appears runnable.
func (h *HandoffService) verifyContainerAgent(ctx context.Context, t ports.Transport, ref ContainerRef, agentInner string) error {
	probe, err := ContainerExec(ref.Runtime, ref, agentInner+" --version", false)
	if err != nil {
		return err
	}
	out, errOut, _ := t.Run(ctx, "", probe)
	combined := strings.TrimSpace(out + "\n" + errOut)
	if ok, guidance := ClassifyContainerVerify(combined); !ok {
		return fmt.Errorf("container verify failed: %s\n--- probe output ---\n%s", guidance, combined)
	}
	return nil
}
```

Note: `handoff.go` already imports `context`, `fmt`, `strings`, and `ports`.

- [ ] **Step 2: Resolve + wrap + set in `Launch`**

In `internal/core/handoff.go` `Launch`, immediately after `profile, err := h.Profiles.Get(ctx, opts.HostID, false)` (and its error check), add container resolution:

```go
	var cref *ContainerRef
	if opts.Container != "" {
		cspec, err := profile.ResolveContainer(opts.Container)
		if err != nil {
			return nil, nil, err
		}
		cwd := opts.RemoteCWD
		if cwd == "" {
			cwd = cspec.ResolveCWD(opts.RepoRef)
		}
		cref = &ContainerRef{
			Runtime: cspec.RuntimeVerb(),
			Ref:     cspec.Container,
			CWD:     cwd,
			User:    cspec.User,
		}
	}
```

In the `else` (agent) branch, capture the bare inner command and wrap the launch command when a container is set. Replace:

```go
		agentName = ag.Name
		launchCmd = ag.LaunchCommand(opts.Goal)
```

with:

```go
		agentName = ag.Name
		launchCmd = ag.LaunchCommand(opts.Goal)
		if cref != nil {
			launchCmd, err = ContainerExec(cref.Runtime, *cref, ag.InnerCommand(), true)
			if err != nil {
				return nil, nil, err
			}
		}
```

- [ ] **Step 3: Persist container on the session + verify before starting work**

In `Launch`, after the holding session is created (`sess, err := h.Sessions.Create(...)` and its error check) and after `t, err := h.NewTransport(opts.HostID)` is available, add — placed right after the `t` is created and before `h.Coord.Ensure`:

```go
	if cref != nil {
		sess.Container = cref
		_ = h.Sessions.Reg.PutSession(sess)
		agInner := "" // resolved agent inner command for the probe
		if ag, aerr := profile.FindAgent(agentName); aerr == nil {
			agInner = ag.InnerCommand()
		}
		if agInner != "" {
			if verr := h.verifyContainerAgent(ctx, t, *cref, agInner); verr != nil {
				_ = h.Sessions.Destroy(ctx, sess.ID, false) // tear down the holding shell
				return nil, nil, verr
			}
		}
	}
```

Rationale: the holding shell exists on the host tmux; verify runs `docker exec … <agent> --version` through the same transport before we `Send` the real launch command. On failure we destroy the holding session so no orphan pane remains.

- [ ] **Step 4: Parse `--container` on `agent start` in the CLI**

In `internal/cli/app.go`, find the `agent` command's `start` subcommand argument loop (mirrors the existing `--agent`/`--goal`/`--host` flags). Add a `container` variable initialized to `""`, a flag case:

```go
		case "--container":
			i++
			container = args[i]
```

and set it on the opts passed to `AgentStart`/`Launch`:

```go
		Container: container,
```

(Place `container` alongside the other parsed locals for the `start` case; wire it into the `core.HandoffOpts{...}` literal already constructed there.)

- [ ] **Step 5: Build, test, verify wiring compiles**

Run:
```bash
gofmt -w internal/core/handoff.go internal/cli/app.go
go build ./... && go test ./internal/core/... 
```
Expected: build clean; all `internal/core` tests PASS (Tasks 1–4).

- [ ] **Step 6: Commit**

```bash
git add internal/core/handoff.go internal/cli/app.go
git commit -m "[core] feat: relay agent start --container (resolve, wrap, verify)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Manual integration verification (gated)

**Files:** none (runtime verification against a real host).

This mirrors the evidence in the design spec. Requires a reachable host with docker and a `containers:` entry in its `host.yaml`. Do **not** wire this into CI (it needs live SSH + docker + creds).

- [ ] **Step 1: Add a container to a host profile**

On a reachable host (e.g. `hamburg`), edit `~/.config/relay/host.yaml` to add:

```yaml
containers:
  - name: itest
    runtime: docker
    container: <a-running-container-name>   # from `docker ps`
    user: "<host-uid>"                       # e.g. 1005 (non-root, host owner)
    default_cwd: /tmp
```

- [ ] **Step 2: Positive path — a self-contained agent that runs in the container**

Run:
```bash
relay agent start -H hamburg --container itest --agent claude --goal "reply with the single word PONG"
```
Expected: returns `{"next":"wait",...}` (verify passed; handoff started). Then `relay agent wait --handoff ho-… --timeout 60` progresses to an actionable event; `relay agent capture --handoff ho-…` shows the agent in the container.

- [ ] **Step 3: Negative path — verify gate refuses a non-runnable agent**

Point `itest.container` at an old-glibc container (e.g. an Ubuntu 18.04 image) and run:
```bash
relay agent start -H hamburg --container itest --agent codex --goal "x"
```
Expected: non-zero exit with `container verify failed: … newer base image (node ≥18 needs glibc ≥2.28)` and **no** started handoff / orphan pane.

- [ ] **Step 4: Durability — pane survives container restart**

With a positive-path handoff running, `docker restart <container>` on the host, then `relay agent capture --handoff ho-…`. Expected: the host tmux pane persists (shows a dead `docker exec` pane), confirming tmux-on-host durability.

- [ ] **Step 5: Record results**

Append observed outputs to the spec's evidence section (or a short `docs/superpowers/plans/2026-07-29-container-handoffs-itest-log.md`) and commit with `[docs]`.

---

## Self-Review

**Spec coverage (this plan = MVP core; remainder is the follow-up plan below):**
- Decision 1 (container = session attribute; tmux on host) → Tasks 1, 3, 5. ✓
- Decision 2 (`containers:` in host.yaml) → Task 2. ✓
- Decision 3 (docker only; runtime a single field) → Task 1/2 (`RuntimeVerb`). ✓
- Decision 5 (verify hard gate) → Tasks 4, 5 (step 3), 6. ✓
- Decision 6 (non-root/host uid) → carried as `ContainerSpec.User` → `ContainerRef.User` (Tasks 2, 5); verified in Task 6 step 2/3.
- Decisions 4 (two toolchain strategies + `expose`/`provision`), 7 (hooks-off application), 8 (`relay container plan`), 9 (`provision`) and the ad-hoc `session create --container` / `discover --probe-containers` / `container up` surfaces → **deferred to the follow-up plan** (see below). The schema fields (`Expose`, `Toolchain`, `Hooks`, `Image`, `Env`) are parsed now (Task 2) so the follow-up is additive.

**Placeholder scan:** No TBD/TODO; every code step shows complete code; the one non-TDD wiring task (Task 5) composes already-tested pure functions and is covered by the manual integration task.

**Type consistency:** `ContainerRef{Runtime,Ref,CWD,User,Home}` used identically in Tasks 1, 3, 5. `ContainerExec(runtime, ref, inner, tty)` signature consistent across Tasks 1, 3, 5. `ClassifyContainerVerify(output)(ok,guidance)` consistent Tasks 4, 5. `ResolveContainer`/`ResolveCWD`/`RuntimeVerb` consistent Tasks 2, 5.

## Follow-up plan (out of scope here)

A second plan (`2026-07-DD-relay-container-tooling.md`) should cover, building only on the fields already parsed here:
1. `relay session create/adopt --container` (interactive pane runs `docker exec`; ad-hoc `exec` already wraps via Task 3).
2. Applying `expose:` — bind-mode via `relay container up` (docker run from `image`+`expose`) and cp-mode into a running container; `hooks: off` neutralization; `env:` passthrough.
3. `relay container plan -H h --container c --agent a` — host closure resolver + container glibc/node/user probe + run/won't-run verdict with paste-ready config.
4. `relay host discover --probe-containers` — list `docker ps` as `containers:` proposals + per-agent verdict.
5. Per-agent `provision:` (declarative install-once) and per-agent overrides (`agents:` block).
