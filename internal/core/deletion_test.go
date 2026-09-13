package core

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestDeleteSessionsCleansIdentityWithTheRecord(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{ID: "sess-delete", HostID: "home", Persist: ports.PersistHandle{Name: "worker"}, CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	RememberResume(sess)
	if err := rememberBridgeToken(sess.ID, "br-delete"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSessions(context.Background(), reg, []*Session{sess}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetSession(sess.ID); err == nil {
		t.Fatal("authoritative session survived delete")
	}
	resume, err := LookupResume(sess.Persist.Name)
	if err != nil || resume.State != ResumeStateCleaned {
		t.Fatalf("destroyed session remained resumable: %+v err=%v", resume, err)
	}
	if _, err := os.Stat(bridgeTokenPath(sess.ID)); !os.IsNotExist(err) {
		t.Fatalf("bridge token survived destructive commit: %v", err)
	}
}

func TestDeleteSessionKeepsIdentityWhenTheRemoteIsKept(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{ID: "sess-keep", HostID: "home", Persist: ports.PersistHandle{Name: "worker"}, CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	RememberResume(sess)
	if err := rememberBridgeToken(sess.ID, "br-keep"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession(context.Background(), reg, sess); err != nil {
		t.Fatal(err)
	}
	if resume, err := LookupResume(sess.Persist.Name); err != nil || resume.State != ResumeStateResumable {
		t.Fatalf("a kept remote must stay resumable: %+v err=%v", resume, err)
	}
	if _, err := os.Stat(bridgeTokenPath(sess.ID)); err != nil {
		t.Fatalf("a kept remote must keep its bridge identity: %v", err)
	}
}

func TestDeletionTeardownFailureLeavesTheRecord(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{ID: "sess-manager", CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	want := errors.New("remote teardown failed")
	err := DeleteSessions(context.Background(), reg, []*Session{sess}, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("delete err=%v", err)
	}
	if _, err := reg.GetSession(sess.ID); err != nil {
		t.Fatalf("session removed after failed teardown: %v", err)
	}
}
