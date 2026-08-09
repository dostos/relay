package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dostos/relay/internal/core"
)

func runJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	a := New()
	var decoded map[string]any
	out := captureStdout(t, func() {
		if code := a.Run(args); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, "see stdout")
		}
	})
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("%v produced non-JSON %q: %v", args, out, err)
	}
	return decoded
}

// The whole point: this command runs in a container with no cmux surface, and
// it has to work there. Running it twice must converge, because a seed hook
// runs on every container start.
func TestParentRegisterHeadlessIsIdempotentFromTheCLI(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv("CMUX_SURFACE_REF", "")

	first := runJSON(t, "--json", "parent", "register", "--headless", "--name", "cli-apex", "--ttl", "5m")
	if first["created"] != true {
		t.Fatalf("first register = %v", first)
	}
	session, _ := first["session"].(map[string]any)
	id, _ := session["id"].(string)
	if id == "" {
		t.Fatalf("no session id in %v", first)
	}
	health, _ := first["headless"].(map[string]any)
	if health["state"] != core.HeadlessFresh || health["ttl_seconds"] != float64(300) {
		t.Fatalf("headless health = %v", health)
	}

	second := runJSON(t, "--json", "parent", "register", "--headless", "--name", "cli-apex", "--ttl", "5m")
	if second["created"] != false {
		t.Fatalf("second register created again: %v", second)
	}
	again, _ := second["session"].(map[string]any)
	if again["id"] != id {
		t.Fatalf("registration is not stable: %v vs %v", again["id"], id)
	}

	listed := runJSON(t, "--json", "parent", "list")
	parents, _ := listed["parents"].([]any)
	if len(parents) != 1 {
		t.Fatalf("parent list = %v", listed)
	}
	reported, _ := listed["headless"].(map[string]any)
	if _, ok := reported[id]; !ok {
		t.Fatalf("parent list must report headless liveness: %v", listed)
	}

	beat := runJSON(t, "--json", "parent", "heartbeat", id)
	if beat["parent_session_id"] != id {
		t.Fatalf("heartbeat = %v", beat)
	}
}

func TestParentRegisterHeadlessRequiresAName(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"--json", "parent", "register", "--headless"}); code == 0 {
			t.Fatal("nameless headless registration must fail: the name IS the identity")
		}
	})
	if !strings.Contains(out, "--name") {
		t.Fatalf("error must name the missing flag: %q", out)
	}
}

// The identity is what lets the holder process — in another container — operate
// this root through the authenticated boundary. It must be stable across runs,
// or every restart would orphan the credential the holder is already using.
func TestHeadlessIdentityIsStableAcrossRegistrations(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	first := runJSON(t, "--json", "parent", "register", "--headless", "--name", "apex", "--print-identity")
	firstIdentity, _ := first["identity"].(map[string]any)
	if firstIdentity["token"] == nil || firstIdentity["session_id"] == nil {
		t.Fatalf("identity = %v", firstIdentity)
	}
	second := runJSON(t, "--json", "parent", "register", "--headless", "--name", "apex", "--print-identity")
	secondIdentity, _ := second["identity"].(map[string]any)
	if secondIdentity["token"] != firstIdentity["token"] {
		t.Fatalf("token rotated across idempotent registrations")
	}
}

// A credential is not printed unless it was asked for.
func TestHeadlessRegistrationWithholdsIdentityByDefault(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	result := runJSON(t, "--json", "parent", "register", "--headless", "--name", "apex")
	if _, present := result["identity"]; present {
		t.Fatalf("identity leaked without --print-identity: %v", result)
	}
}
