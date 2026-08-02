package core

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type deletionViz struct{ fail bool }

func (*deletionViz) Kind() string                   { return "test" }
func (*deletionViz) Available(context.Context) bool { return true }
func (*deletionViz) Present(context.Context, string, string, ports.Layout) (string, error) {
	return "", nil
}
func (*deletionViz) Focus(context.Context, string) error { return nil }
func (v *deletionViz) Close(context.Context, string) error {
	if v.fail {
		return errors.New("offline")
	}
	return nil
}
func (*deletionViz) Layout(context.Context) (string, error)               { return "", nil }
func (*deletionViz) SaveRestorable(context.Context) (int, error)          { return 0, nil }
func (*deletionViz) RestoreSaved(context.Context) (int, error)            { return 0, nil }
func (*deletionViz) BrandLabels(context.Context, map[string]string) error { return nil }

func TestProjectedDeleteSurvivesVizOutage(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{ID: "sess-delete", HostID: "home", Persist: ports.PersistHandle{Name: "worker"}, CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	viz := &deletionViz{fail: true}
	if err := DeleteSessionProjected(context.Background(), reg, viz, sess, false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetSession(sess.ID); err == nil {
		t.Fatal("authoritative session survived delete")
	}
	entries, err := os.ReadDir(deletionDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("durable tombstone missing: entries=%v err=%v", entries, err)
	}
	viz.fail = false
	pending, err := RecoverSessionDeletions(context.Background(), reg, viz)
	if err != nil || pending != 0 {
		t.Fatalf("recovery pending=%d err=%v", pending, err)
	}
	entries, _ = os.ReadDir(deletionDir())
	if len(entries) != 0 {
		t.Fatalf("completed tombstone remains: %v", entries)
	}
}

func TestDeletionReservationRejectsNewLineageBeforeRemoteTeardown(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	manager := &Session{ID: "sess-manager", HostID: "home", Persist: ports.PersistHandle{Name: "manager"}, CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(manager); err != nil {
		t.Fatal(err)
	}
	putDone := make(chan error, 1)
	teardownObserved := false
	err := DeleteSessionsProjected(context.Background(), reg, nil, []*Session{manager}, true, func() error {
		teardownObserved = true
		go func() {
			putDone <- reg.PutSession(&Session{ID: "sess-late", SourceSessionID: manager.ID, CreatedAt: time.Now().UTC()})
		}()
		select {
		case err := <-putDone:
			t.Fatalf("lineage write escaped reservation: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		return nil
	})
	if err != nil || !teardownObserved {
		t.Fatalf("delete err=%v teardown=%v", err, teardownObserved)
	}
	if err := <-putDone; err == nil {
		t.Fatal("new child accepted for reserved/deleted manager")
	}
	if _, err := reg.GetSession(manager.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("manager remains: %v", err)
	}
}

func TestDeletionTeardownFailureRollsBackReservation(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	manager := &Session{ID: "sess-manager", CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(manager); err != nil {
		t.Fatal(err)
	}
	want := errors.New("remote teardown failed")
	err := DeleteSessionsProjected(context.Background(), reg, nil, []*Session{manager}, false, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("delete err=%v", err)
	}
	if _, err := reg.GetSession(manager.ID); err != nil {
		t.Fatalf("manager removed after failed teardown: %v", err)
	}
	if managerDeletionReserved(manager.ID) {
		t.Fatal("failed teardown left deletion reservation")
	}
}
