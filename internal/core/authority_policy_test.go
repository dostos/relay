package core

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/ports"
)

func TestAuthorityPolicyAllowsApexHierarchyAndPreservesHumanGates(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	apex := &Session{ID: "sess-apex", RepoRef: "/work/relay", RepoRefs: []string{"/work/relay", "/work/folio"}, Labels: map[string]string{ApexLabel: "true"}, Persist: ports.PersistHandle{Name: "apex"}, CreatedAt: now}
	apex.HostID = "home"
	apex.RemoteCWD = "/srv/relay"
	manager := &Session{ID: "sess-manager", SourceSessionID: apex.ID, Persist: ports.PersistHandle{Name: "manager"}, CreatedAt: now}
	child := &Session{ID: "sess-child", SourceSessionID: manager.ID, Persist: ports.PersistHandle{Name: "child"}, CreatedAt: now}
	unrelated := &Session{ID: "sess-unrelated", SourceSessionID: "sess-other-root", Persist: ports.PersistHandle{Name: "unrelated"}, CreatedAt: now}
	for _, session := range []*Session{apex, manager, child, unrelated} {
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
		{manager, []string{"session", "capture", unrelated.ID}, false},
		{apex, []string{"agent", "start", "home", "codex", "--parent", manager.ID, "--", "goal"}, true},
		{apex, []string{"agent", "start", "home", "codex", "--repo", "/work/relay/subtree", "--", "goal"}, true},
		{apex, []string{"agent", "start", "home", "codex", "--repo", "/work/unrelated", "--", "goal"}, false},
		{apex, []string{"agent", "start", "home", "codex", "--cwd", "/srv/relay/worktree", "--", "goal"}, true},
		{apex, []string{"agent", "start", "home", "codex", "--cwd", "/etc", "--", "goal"}, false},
		{apex, []string{"agent", "start", "other", "codex", "--cwd", "/srv/relay", "--", "goal"}, false},
		{manager, []string{"agent", "start", "home", "codex", "--", "goal"}, true},
		{child, []string{"agent", "start", "home", "codex", "--parent", apex.ID, "--", "goal"}, false},
		{apex, []string{"root", "control-plane", "--always-on"}, false},
		{apex, []string{"auth", "copy"}, false},
	} {
		got, _ := authorizeOperation(reg, tc.actor, tc.args)
		if got != tc.allowed {
			t.Fatalf("actor=%s args=%v allowed=%v want=%v", tc.actor.ID, tc.args, got, tc.allowed)
		}
	}
}

func TestAuthorityOperationParserSeparatesDiscoveryStartLifecycleAndTargets(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		kind   authorityOperationKind
		target string
	}{
		{[]string{"agent", "protocol"}, authorityDiscovery, ""},
		{[]string{"agent", "pick", "home"}, authorityDiscovery, ""},
		{[]string{"handoff", "list"}, authorityDiscovery, ""},
		{[]string{"agent", "start", "home", "codex", "--parent", "sess-manager", "--", "goal"}, authorityStart, "sess-manager"},
		{[]string{"handoff", "reconcile"}, authorityLifecycle, ""},
		{[]string{"client", "list"}, authorityDiscovery, ""},
		{[]string{"client", "status"}, authorityDiscovery, ""},
		{[]string{"client", "update-status"}, authorityDiscovery, ""},
		{[]string{"client", "update", "--client", "client-mac"}, authorityLifecycle, ""},
		{[]string{"agent", "wait", "ho-child"}, authorityHandoffTarget, "ho-child"},
		{[]string{"session", "capture", "sess-child"}, authoritySessionTarget, "sess-child"},
		{[]string{"auth", "login", "-H", "home"}, authorityHumanRequired, ""},
	} {
		op := parseAuthorityOperation(tc.args)
		if op.Kind != tc.kind || op.Target != tc.target {
			t.Fatalf("args=%v operation=%+v want kind=%s target=%q", tc.args, op, tc.kind, tc.target)
		}
	}
}

func TestAuthorityPolicyAllowsDiscoveryAndInternalLifecycleWithoutFakeTargets(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	actor := &Session{ID: "sess-agent", Persist: ports.PersistHandle{Name: "agent"}, CreatedAt: time.Now().UTC()}
	reg := &Registry{}
	if err := reg.PutSession(actor); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"agent", "protocol"},
		{"agent", "pick", "home"},
		{"handoff", "list"},
		{"handoff", "reconcile"},
		{"client", "list"},
		{"client", "status"},
		{"client", "update-status"},
		{"client", "update", "--client", "client-mac"},
		{"supervise"},
		{"gc", "--dry-run"},
	} {
		if allowed, reason := authorizeOperation(reg, actor, args); !allowed {
			t.Fatalf("args=%v refused: %s", args, reason)
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

func TestAuthorityReceiptSeparatesDistinctInvocationsWithSameArgv(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	apex := &Session{ID: "sess-apex", Labels: map[string]string{ApexLabel: "true"}, Persist: ports.PersistHandle{Name: "apex"}, CreatedAt: now}
	if err := reg.PutSession(apex); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"} {
		source := bridge.Source{SessionID: apex.ID, RequestID: requestID}
		for retry := 0; retry < 2; retry++ {
			if err := AuthorizeBridgeRequest(source, []string{"root", "status"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	f, err := os.Open(LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	receipts := 0
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record["type"] == "authority_decision" {
			receipts++
		}
	}
	if receipts != 2 {
		t.Fatalf("authority receipts=%d want=2", receipts)
	}
}

func TestAuthorityReceiptIgnoresLargeUnrelatedLedgerRecordsAndConcurrentRetries(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	apex := &Session{ID: "sess-apex", Labels: map[string]string{ApexLabel: "true"}, Persist: ports.PersistHandle{Name: "apex"}, CreatedAt: now}
	if err := reg.PutSession(apex); err != nil {
		t.Fatal(err)
	}
	if err := AppendLedger(map[string]any{
		"v": 1, "type": "start", "handoff_id": "ho-large", "goal": strings.Repeat("large unrelated goal ", 10000),
	}); err != nil {
		t.Fatal(err)
	}

	source := bridge.Source{SessionID: apex.ID}
	const attempts = 24
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- AuthorizeBridgeRequest(source, []string{"agent", "protocol"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	f, err := os.Open(LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	receipts := 0
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record["type"] == "authority_decision" {
			receipts++
		}
	}
	if receipts != 1 {
		t.Fatalf("authority receipts=%d want=1", receipts)
	}
}

func TestAuthenticatedLocalHumanCrossesBoundaryWithoutSyntheticHierarchyNode(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	identity, err := EnsureHomeClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeBridgeRequest(identity, []string{"root", "control-plane", "--always-on"}); err != nil {
		t.Fatalf("explicit human policy command was treated as an agent bypass: %v", err)
	}
	if _, err := (&Registry{}).GetSession(HomeClientSessionID); err == nil {
		t.Fatal("home client invented a hierarchy session")
	}
}

func TestAuthorityReceiptReportsPartialLedgerWithoutAppendingPastIt(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	identity, err := EnsureHomeClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDirs(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(LedgerPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"v":1,"type":"start"`); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	err = AuthorizeBridgeRequest(identity, []string{"version"})
	if err == nil || !strings.Contains(err.Error(), "decode authority ledger record 1") {
		t.Fatalf("partial ledger error=%v", err)
	}
	raw, readErr := os.ReadFile(LedgerPath())
	if readErr != nil || strings.Contains(string(raw), "authority_decision") {
		t.Fatalf("receipt appended past partial record: %q err=%v", raw, readErr)
	}
}

func TestAuthorityReceiptIndexRefusesUnsafeFilesystemEntry(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	identity, err := EnsureHomeClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDirs(); err != nil {
		t.Fatal(err)
	}
	digest := authorityDigest(identity.SessionID, []string{"version"}, "")
	target := filepath.Join(StateRoot(), "untrusted-receipt")
	if err := os.WriteFile(target, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, authorityReceiptIndexPath(digest)); err != nil {
		t.Fatal(err)
	}
	err = AuthorizeBridgeRequest(identity, []string{"version"})
	if err == nil || !strings.Contains(err.Error(), "unsafe or oversized") {
		t.Fatalf("unsafe receipt index error=%v", err)
	}
}
