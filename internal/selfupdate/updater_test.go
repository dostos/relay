package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallPairReplacesBothAndKeepsRollbackCopies(t *testing.T) {
	stage, install := t.TempDir(), t.TempDir()
	for _, name := range []string{"relay", "relayd"} {
		if err := os.WriteFile(filepath.Join(stage, name), []byte("new-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(install, name), []byte("old-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installPair(stage, install); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay", "relayd"} {
		got, _ := os.ReadFile(filepath.Join(install, name))
		previous, _ := os.ReadFile(filepath.Join(install, name+".previous"))
		if string(got) != "new-"+name || string(previous) != "old-"+name {
			t.Fatalf("%s current=%q previous=%q", name, got, previous)
		}
	}
}

func TestInstallPairRollsBackWhenSecondBinaryIsMissing(t *testing.T) {
	stage, install := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "relay"), []byte("new-relay"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay", "relayd"} {
		if err := os.WriteFile(filepath.Join(install, name), []byte("old-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installPair(stage, install); err == nil {
		t.Fatal("missing staged relayd must fail")
	}
	for _, name := range []string{"relay", "relayd"} {
		got, err := os.ReadFile(filepath.Join(install, name))
		if err != nil || string(got) != "old-"+name {
			t.Fatalf("%s rollback=%q err=%v", name, got, err)
		}
	}
}
