package cmux

import (
	"encoding/json"
	"errors"
	"strings"
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
		Labels:             map[string]string{"agent": "future-agent-cli"},
		CreatedByHandoffID: "ho-child", VizSurfaceRef: "viz:queued:17", CreatedAt: time.Now().UTC(),
	}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(&core.Handoff{ID: "ho-child", SessionID: sess.ID, PresentationState: core.EffectPending, CreatedAt: time.Now().UTC()}); err != nil {
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
	ho, err := reg.GetHandoff("ho-child")
	if err != nil || ho.PresentationState != core.EffectAcknowledged {
		t.Fatalf("presentation effect = %+v err=%v", ho, err)
	}
}

func TestUpdateAckReportsInstalledBuildBeforeRestart(t *testing.T) {
	event := coord.Event{Seq: 46, Kind: "update_relayd"}
	meta, ok := vizAckMeta(event, "d894f9f")
	if !ok || meta["request_seq"] != int64(46) || meta["request_kind"] != "update_relayd" || meta["result"] != "d894f9f" || meta["build"] != coord.Build {
		t.Fatalf("update ack=%+v ok=%v", meta, ok)
	}
}

func TestLifecycleFailureAckIsBoundedAndTyped(t *testing.T) {
	meta := lifecycleFailureAckMeta(coord.Event{Seq: 47, Kind: "update_relayd"}, errors.New("build failed\n"+strings.Repeat("x", 3000)))
	if meta["request_seq"] != int64(47) || meta["request_kind"] != "update_relayd" || meta["status"] != "failed" || meta["build"] != coord.Build || meta["result"] != coord.Build {
		t.Fatalf("failure ack=%+v", meta)
	}
	failure, _ := meta["error"].(string)
	if failure == "" || len(failure) > 2050 || strings.Contains(failure, "\n") {
		t.Fatalf("unbounded failure=%q len=%d", failure, len(failure))
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
