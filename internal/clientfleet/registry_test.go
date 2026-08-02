package clientfleet

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dostos/relay/internal/coord"
)

func TestEnrollUsesOpaqueIdentityAndPreservesIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	first, err := Enroll(root, "visualization", "relay-viz-legacy-name", "Laptop", "SHA256:one")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID == first.Channel {
		t.Fatalf("identity not opaque: %+v", first)
	}
	second, err := Enroll(root, "visualization", first.Channel, "Portable display", first.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Label != "Portable display" {
		t.Fatalf("idempotent enrollment=%+v", second)
	}
}

func TestConcurrentEnrollmentDoesNotLoseClients(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	var wg sync.WaitGroup
	for _, channel := range []string{"relay-client-one", "relay-client-two"} {
		wg.Add(1)
		go func(channel string) {
			defer wg.Done()
			if _, err := Enroll(root, "worker", channel, "", ""); err != nil {
				t.Errorf("enroll: %v", err)
			}
		}(channel)
	}
	wg.Wait()
	clients, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 {
		t.Fatalf("concurrent enrollment lost a client: %+v", clients)
	}
}

func TestLegacyAuthorizationMigratesOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	keyType := "ssh-ed25519"
	blob := make([]byte, 4+len(keyType)+32)
	binary.BigEndian.PutUint32(blob, uint32(len(keyType)))
	copy(blob[4:], keyType)
	line := `restrict,command="$HOME/.local/bin/relayd viz-broker --service relay-viz-mac" ssh-ed25519 ` + base64.StdEncoding.EncodeToString(blob) + ` relay-viz-managed`
	unrelated := "# preceding ordinary key\nssh-ed25519 QUFBQUFBQUFBQUFBQUFBQUFBQUFB human\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(unrelated+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	clients, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].Channel != "relay-viz-mac" || clients[0].Label == "mac" {
		t.Fatalf("migration=%+v", clients)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != clients[0].ID {
		t.Fatalf("migration was not durable: %+v", again)
	}
}

func TestStatusRequiresMatchingEffectAck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	c, err := Enroll(root, "visualization", "relay-viz-test", "Display", "")
	if err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events")
	if err := os.MkdirAll(events, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEvent := func(name string, e coord.Event) {
		raw, _ := json.Marshal(e)
		if err := os.WriteFile(filepath.Join(events, name+".jsonl"), append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEvent(c.Channel, coord.Event{Seq: 7, TS: "requested", Kind: "update_relayd", Meta: map[string]any{"requested_build": "new"}})
	status, err := Status(root)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].State != "pending" {
		t.Fatalf("without effect ack=%+v", status[0])
	}
	writeEvent(c.Channel+"-ack", coord.Event{Seq: 2, TS: "acked", Kind: "client_ack", Meta: map[string]any{"request_seq": float64(7), "request_kind": "update_relayd", "result": "new"}})
	status, err = Status(root)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].State != "active" || status[0].InstalledBuild != "new" {
		t.Fatalf("effect ack=%+v", status[0])
	}
}
