package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyReplyRequiresLiteralGuard(t *testing.T) {
	err := validatePolicyRule(PolicyRule{ID: "unsafe", Kind: "permission_required", Action: "reply", Reply: "y"})
	if err == nil {
		t.Fatal("unguarded automatic reply was accepted")
	}
	if err := validatePolicyRule(PolicyRule{ID: "safe", Kind: "permission_required", Agent: "cursor-agent", Contains: []string{"git status"}, Action: "reply", Reply: "y"}); err != nil {
		t.Fatalf("guarded reply rejected: %v", err)
	}
}

func TestPolicyAddCheckAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	service := &PolicyService{Path: path}
	rule := PolicyRule{ID: "cursor-read", Kind: "ask", Agent: "cursor-agent", Host: "cancun", Contains: []string{"Run this command?", "git status"}, Action: "reply", Reply: "y"}
	if err := service.Add(rule); err != nil {
		t.Fatal(err)
	}
	decision, err := service.Decide(PolicyContext{Kind: "ask", Agent: "cursor-agent", Host: "cancun", Text: "RUN THIS COMMAND?", Command: "git status"})
	if err != nil || !decision.Matched || decision.RuleID != rule.ID || decision.Reply != "y" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("policy mode=%v", info.Mode().Perm())
	}
	if err := service.Remove(rule.ID); err != nil {
		t.Fatal(err)
	}
	decision, err = service.Decide(PolicyContext{Kind: "ask", Agent: "cursor-agent", Host: "cancun", Text: "Run this command? git status"})
	if err != nil || decision.Matched {
		t.Fatalf("removed rule still matched: %+v err=%v", decision, err)
	}
}

func TestPolicyBuiltinsOnlyCoalesceRedundantEvents(t *testing.T) {
	service := &PolicyService{Path: filepath.Join(t.TempDir(), "missing.yaml")}
	idle, err := service.Decide(PolicyContext{Kind: "ask", SourceKind: "idle", PendingKinds: map[string]bool{"permission_required": true}})
	if err != nil || !idle.Builtin || idle.Action != "ack" {
		t.Fatalf("idle decision=%+v err=%v", idle, err)
	}
	realAsk, err := service.Decide(PolicyContext{Kind: "ask", SourceKind: "ask", PendingKinds: map[string]bool{"permission_required": true}})
	if err != nil || realAsk.Matched {
		t.Fatalf("real ask was mistaken for idle fallback: %+v err=%v", realAsk, err)
	}
	exit, err := service.Decide(PolicyContext{Kind: "exit", SeenKinds: map[string]bool{"result": true}})
	if err != nil || !exit.Builtin || exit.Action != "ack" {
		t.Fatalf("exit decision=%+v err=%v", exit, err)
	}
	unmatched, err := service.Decide(PolicyContext{Kind: "permission_required", Text: "approve deploy"})
	if err != nil || unmatched.Matched {
		t.Fatalf("unconfigured permission was auto-handled: %+v err=%v", unmatched, err)
	}
}
