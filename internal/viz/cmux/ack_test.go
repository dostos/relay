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
	result, _ := json.Marshal(map[string]any{"session_id": sess.ID, "revision": int64(17), "surface": "surface:42"})
	event := coord.Event{Seq: 3, Kind: "viz_ack", Meta: map[string]any{
		"request_seq": float64(17), "request_kind": "project", "op": "upsert", "session_id": sess.ID, "result": string(result),
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

func TestApplyPresentationAckRefusesLegacyBareSurface(t *testing.T) {
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
	if got.VizSurfaceRef != "viz:queued:1" {
		t.Fatalf("viz ref = %q", got.VizSurfaceRef)
	}
}

func TestApplyProjectionAckRequiresMatchingSessionAndRevision(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &core.Registry{}
	sess := &core.Session{ID: "sess-current", VizSurfaceRef: "viz:queued:22", CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"session_id": sess.ID, "revision": int64(22), "surface": "surface:279"})
	event := coord.Event{Kind: "viz_ack", Meta: map[string]any{
		"request_seq": float64(22), "request_kind": "project", "op": "upsert", "session_id": sess.ID, "result": string(result),
	}}
	if err := applyPresentationAck(reg, event); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.GetSession(sess.ID)
	if got.VizSurfaceRef != "surface:279" {
		t.Fatalf("viz ref=%q", got.VizSurfaceRef)
	}

	sess.VizSurfaceRef = "viz:queued:23"
	_ = reg.PutSession(sess)
	event.Meta["request_seq"] = float64(23)
	event.Meta["result"] = string(result) // still revision 22
	if err := applyPresentationAck(reg, event); err == nil {
		t.Fatal("mismatched projection receipt must fail")
	}
}
