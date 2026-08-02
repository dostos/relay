package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectionOnlyRegistryReadNeverReportsAuthoritativeEmpty(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := (&Registry{}).ListSessions()
	if list != nil || !errors.Is(err, ErrProjectionOnlyAuthority) {
		t.Fatalf("list=%v err=%v", list, err)
	}
}

func TestProjectionOnlyAuthorityFamiliesFailClosed(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &Registry{}
	parents := &ParentService{Reg: reg}
	checks := []func() error{
		func() error { _, err := reg.ListHandoffs(); return err },
		func() error { _, err := reg.GetHandoff("ho-missing"); return err },
		func() error { _, err := parents.ListMessages("sess-parent", true); return err },
		func() error { _, err := parents.FindMessage("msg-missing"); return err },
		func() error { _, err := LoadHistory(); return err },
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, ErrProjectionOnlyAuthority) {
			t.Fatalf("check %d returned %v, want ErrProjectionOnlyAuthority", i, err)
		}
	}
}

func TestRetireLocalAuthorityQuarantinesSymlink(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(state, "handoffs")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RetireLocalAuthorityState(); err != nil {
		t.Fatal(err)
	}
	archives, err := filepath.Glob(filepath.Join(state, "retired-local-authority", "*", "handoffs"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("symlink archive=%v err=%v", archives, err)
	}
	if info, err := os.Lstat(archives[0]); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("archived path is not the symlink: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target was changed: %v", err)
	}
}

func TestRetireFencesReadsBeforeQuarantineFailure(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), []byte(`{"sessions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force archive creation to fail after the marker is durably published.
	if err := os.WriteFile(filepath.Join(state, "retired-local-authority"), []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RetireLocalAuthorityState(); err == nil {
		t.Fatal("retirement unexpectedly succeeded")
	}
	if _, err := (&Registry{}).ListSessions(); !errors.Is(err, ErrProjectionOnlyAuthority) {
		t.Fatalf("read was not fenced after partial retirement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "sessions.json")); err != nil {
		t.Fatalf("test did not fail before quarantine: %v", err)
	}
}

func TestRetireRefusesProjectionMarkerSymlink(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	target := filepath.Join(t.TempDir(), "target")
	const original = "do not truncate\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(state, ".viz-projection-only")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), []byte(`{"sessions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RetireLocalAuthorityState(); err == nil {
		t.Fatal("unsafe marker symlink was accepted")
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != original {
		t.Fatalf("marker symlink target changed: %q err=%v", raw, err)
	}
}
