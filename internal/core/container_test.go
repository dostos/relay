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
	if !strings.Contains(got, "/w") || !strings.Contains(got, "&& ls -la") {
		t.Fatalf("cwd/join wrong: %q", got)
	}
}

func TestContainerExecRequiresRef(t *testing.T) {
	if _, err := ContainerExec("docker", ContainerRef{}, "x", true); err == nil {
		t.Fatal("expected error for empty container ref")
	}
}

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
