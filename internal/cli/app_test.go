package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
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

func TestSourceEnvironmentUsesAuthenticatedRegistryIdentity(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv(bridge.SourceSessionEnv, "sess-real")
	t.Setenv(bridge.SourceHostEnv, "spoofed-host")
	t.Setenv(bridge.SourcePersistEnv, "spoofed-name")
	now := time.Now().UTC()
	reg := &core.Registry{}
	if err := reg.PutSession(&core.Session{
		ID: "sess-real", HostID: "c3", RepoRef: "/local/repo",
		Persist: ports.PersistHandle{Name: "research"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	id, host, persist, repo := sourceFromEnvironment(reg)
	if id != "sess-real" || host != "c3" || persist != "research" || repo != "/local/repo" {
		t.Fatalf("unexpected source: %q %q %q %q", id, host, persist, repo)
	}
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
