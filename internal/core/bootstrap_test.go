package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapGoBinaryFindsUserInstallWithEmptyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	t.Setenv("GOROOT", "")
	want := filepath.Join(home, ".local", "go", "bin", "go")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := bootstrapGoBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("go binary=%q want=%q", got, want)
	}
}

func TestBootstrapGoBinaryRejectsNonExecutableUserInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	t.Setenv("GOROOT", "")
	path := filepath.Join(home, ".local", "go", "bin", "go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not executable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrapGoBinary(); err == nil {
		t.Fatal("non-executable compiler accepted")
	}
}
