package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

func TestUnknownFlagRejected(t *testing.T) {
	a := New()
	if code := a.Run([]string{"session", "create", "--bogus", "x"}); code == 0 {
		t.Fatal("expected non-zero")
	}
}

func TestJSONErrorShape(t *testing.T) {
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"--json", "session", "create", "--bogus"}); code == 0 {
			t.Fatal("expected non-zero")
		}
	})
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("json: %v out=%q", err, out)
	}
	if resp["ok"] != false {
		t.Fatalf("ok=%v", resp["ok"])
	}
	errStr, _ := resp["error"].(string)
	if errStr == "" || !bytes.Contains([]byte(errStr), []byte("unknown flag")) {
		t.Fatalf("error=%q", errStr)
	}
}
