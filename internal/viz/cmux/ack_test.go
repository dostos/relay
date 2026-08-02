package cmux

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func TestApplyPresentationAckReplacesQueuedReference(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &core.Registry{}
	sess := &core.Session{
		ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"},
		Labels:        map[string]string{"agent": "future-agent-cli"},
		VizSurfaceRef: "viz:queued:17", CreatedAt: time.Now().UTC(),
	}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]string{"surface": "surface:42", "workspace": "workspace:9", "pane": "pane:2"})
	event := coord.Event{Seq: 3, Kind: "viz_ack", Meta: map[string]any{
		"request_seq": float64(17), "request_kind": "present", "result": string(result),
	}}
	if err := applyPresentationAck(reg, event); err != nil {
		t.Fatal(err)
	}
	got, err := reg.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.VizSurfaceRef != "surface:42" {
		t.Fatalf("viz ref = %q", got.VizSurfaceRef)
	}
	if got.Labels["agent"] != "future-agent-cli" {
		t.Fatalf("agent-agnostic metadata changed: %+v", got.Labels)
	}
}

func TestApplyPresentationAckIgnoresRetiredSession(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &core.Registry{}
	result, _ := json.Marshal(map[string]string{"surface": "surface:42"})
	err := applyPresentationAck(reg, coord.Event{Kind: "viz_ack", Meta: map[string]any{
		"request_seq": float64(17), "request_kind": "present", "result": string(result),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestApplyPresentationAckAcceptsLegacyBareSurface(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &core.Registry{}
	sess := &core.Session{ID: "sess-old", VizSurfaceRef: "viz:queued:1", CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	err := applyPresentationAck(reg, coord.Event{Kind: "viz_ack", Meta: map[string]any{
		"request_seq": float64(1), "request_kind": "present", "result": "surface:256",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reg.GetSession(sess.ID)
	if got.VizSurfaceRef != "surface:256" {
		t.Fatalf("viz ref = %q", got.VizSurfaceRef)
	}
}
