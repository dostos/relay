package cmux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMalformedPendingUpdateReceiptIsQuarantined(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{ServiceID: "test"}
	if err := os.MkdirAll(filepath.Dir(v.pendingUpdateAckPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v.pendingUpdateAckPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.emitPendingUpdateAck(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.pendingUpdateAckPath()); !os.IsNotExist(err) {
		t.Fatalf("corrupt receipt remained: %v", err)
	}
	matches, err := filepath.Glob(v.pendingUpdateAckPath() + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantine=%v err=%v", matches, err)
	}
}
