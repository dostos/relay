package core

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/ports"
)

func TestAuthorityPolicyAllowsApexHierarchyAndPreservesHumanGates(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	apex := &Session{ID: "sess-apex", Labels: map[string]string{ApexLabel: "true"}, Persist: ports.PersistHandle{Name: "apex"}, CreatedAt: now}
	manager := &Session{ID: "sess-manager", SourceSessionID: apex.ID, Persist: ports.PersistHandle{Name: "manager"}, CreatedAt: now}
	child := &Session{ID: "sess-child", SourceSessionID: manager.ID, Persist: ports.PersistHandle{Name: "child"}, CreatedAt: now}
	for _, session := range []*Session{apex, manager, child} {
		if err := reg.PutSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutHandoff(&Handoff{ID: "ho-child", SessionID: child.ID, SourceSessionID: manager.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		actor   *Session
		args    []string
		allowed bool
	}{
		{apex, []string{"root", "enroll", manager.ID}, true},
		{apex, []string{"parent", "reparent", apex.ID, "ho-child"}, true},
		{manager, []string{"parent", "reparent", manager.ID, "ho-child"}, true},
		{child, []string{"parent", "reparent", child.ID, "ho-child"}, false},
		{apex, []string{"root", "control-plane", "--always-on"}, false},
		{apex, []string{"auth", "copy"}, false},
	} {
		got, _ := authorizeOperation(reg, tc.actor, tc.args)
		if got != tc.allowed {
			t.Fatalf("actor=%s args=%v allowed=%v want=%v", tc.actor.ID, tc.args, got, tc.allowed)
		}
	}
}

func TestAuthorityReceiptIsDurableAndIdempotent(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	apex := &Session{ID: "sess-apex", Labels: map[string]string{ApexLabel: "true"}, Persist: ports.PersistHandle{Name: "apex"}, CreatedAt: now}
	if err := reg.PutSession(apex); err != nil {
		t.Fatal(err)
	}
	source := bridge.Source{SessionID: apex.ID}
	for i := 0; i < 2; i++ {
		if err := AuthorizeBridgeRequest(source, []string{"root", "status"}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record["type"] == "authority_decision" {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("authority receipts=%d want=1", count)
	}
}
