package core

import (
	"context"
	"io"
	"strings"
	"testing"
)

type matchTransport struct {
	id    string
	rules []struct{ contain, out string }
}

func (m *matchTransport) ID() string { return m.id }
func (m *matchTransport) Run(_ context.Context, _, command string) (string, string, error) {
	for _, r := range m.rules {
		if strings.Contains(command, r.contain) {
			return r.out, "", nil
		}
	}
	return "", "", nil
}
func (m *matchTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (m *matchTransport) ReadFile(context.Context, string) ([]byte, error)           { return nil, nil }
func (m *matchTransport) WriteFile(context.Context, string, []byte, string) error    { return nil }
func (m *matchTransport) Interactive(context.Context, string) error                  { return nil }

func TestProbeAgentCatalogSuggestsCodexMultiAuthAccounts(t *testing.T) {
	fa := &fakeAccounts{logins: `[{"backend":"codex","name":"Account 1 (a@example.com)","handle":"0","enabled":true,"usable":true},
		{"backend":"codex","name":"Account 2","handle":"1","enabled":true,"usable":true}]`}
	tr := &matchTransport{
		id: "c1",
		rules: []struct{ contain, out string }{
			// loginShellRun single-quotes the script, so binaries appear as '\''bin'\''.
			{contain: `'\''codex-multi-auth-codex'\''`, out: "PRESENT"},
			{contain: `'\''codex'\''`, out: "PRESENT"},
			{contain: "codex login status", out: "Logged in"},
		},
	}
	detected := probeAgentCatalog(context.Background(), tr, "c1", fa.run)
	var names []string
	var present []string
	for _, d := range detected {
		if d.Present {
			present = append(present, d.Name)
		}
		if d.SuggestedSpec == nil {
			continue
		}
		names = append(names, d.SuggestedSpec.Name)
		if strings.HasPrefix(d.SuggestedSpec.Name, "codex:") {
			if d.SuggestedSpec.Command != "codex-multi-auth-codex" {
				t.Fatalf("%s command=%q", d.SuggestedSpec.Name, d.SuggestedSpec.Command)
			}
			if len(d.SuggestedSpec.Args) != 2 || d.SuggestedSpec.Args[0] != "--account" {
				t.Fatalf("%s args=%v", d.SuggestedSpec.Name, d.SuggestedSpec.Args)
			}
			if d.SuggestedSpec.UsageKey != "codex" {
				t.Fatalf("%s usage_key=%q", d.SuggestedSpec.Name, d.SuggestedSpec.UsageKey)
			}
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "codex:a@example.com") || !strings.Contains(joined, "codex:2") {
		t.Fatalf("suggested names: %v present=%v", names, present)
	}
	if !strings.Contains(joined, "codex") {
		t.Fatalf("expected bare codex suggestion too: %v", names)
	}
}
