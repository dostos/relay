package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallPrimaryReplacesBinaryAndCreatesCompatibilitySymlink(t *testing.T) {
	stage, install := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "relay"), []byte("new-relay"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay", "relayd"} {
		if err := os.WriteFile(filepath.Join(install, name), []byte("old-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installPrimary(stage, install); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(install, "relay"))
	if string(got) != "new-relay" {
		t.Fatalf("relay current=%q", got)
	}
	link, err := os.Readlink(filepath.Join(install, "relayd"))
	if err != nil || link != "relay" {
		t.Fatalf("relayd compatibility link=%q err=%v", link, err)
	}
	for _, name := range []string{"relay", "relayd"} {
		previous, _ := os.ReadFile(filepath.Join(install, name+".previous"))
		if string(previous) != "old-"+name {
			t.Fatalf("%s previous=%q", name, previous)
		}
	}
}

func TestInstallPrimaryDoesNotMutateWhenStagedBinaryIsMissing(t *testing.T) {
	stage, install := t.TempDir(), t.TempDir()
	for _, name := range []string{"relay", "relayd"} {
		if err := os.WriteFile(filepath.Join(install, name), []byte("old-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installPrimary(stage, install); err == nil {
		t.Fatal("missing staged relay must fail")
	}
	for _, name := range []string{"relay", "relayd"} {
		got, err := os.ReadFile(filepath.Join(install, name))
		if err != nil || string(got) != "old-"+name {
			t.Fatalf("%s rollback=%q err=%v", name, got, err)
		}
	}
}
