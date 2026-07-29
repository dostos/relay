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
