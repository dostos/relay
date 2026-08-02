package core

import (
	"context"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

type targetViz struct {
	ports.Viz
	req ports.Presentation
}

func (v *targetViz) PresentTarget(_ context.Context, req ports.Presentation) (string, error) {
	v.req = req
	return "surface:1", nil
}

func TestPresentSessionSendsIdentityWithoutPlacement(t *testing.T) {
	viz := &targetViz{}
	sess := &Session{ID: "sess-1", HostID: "home-relay", Persist: ports.PersistHandle{Name: "beholder-pdf-main"}}
	ref, err := PresentSession(context.Background(), viz, sess, "untrusted attach command", ports.Layout{Workspace: "must-not-cross", Pane: "must-not-cross"})
	if err != nil || ref != "surface:1" {
		t.Fatalf("ref=%q err=%v", ref, err)
	}
	want := (ports.Presentation{SessionID: sess.ID, Target: sess.HostID, TmuxName: sess.Persist.Name})
	if viz.req != want {
		t.Fatalf("request=%+v want=%+v", viz.req, want)
	}
}
