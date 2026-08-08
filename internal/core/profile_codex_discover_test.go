package core

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestParseCodexMultiAuthListJSONSelectors(t *testing.T) {
	raw := `{
  "accountCount": 2,
  "accounts": [
    {"index": 0, "label": "Account 1 (dostos, dostos10@gmail.com, id:w80WCB)", "enabled": true},
    {"index": 1, "label": "Account 2 (no-email-here)", "enabled": true},
    {"index": 2, "label": "Account 3 (skip@example.com)", "enabled": false}
  ]
}`
	got := parseCodexMultiAuthListJSON(raw)
	if len(got) != 2 || got[0] != "dostos10@gmail.com" || got[1] != "2" {
		t.Fatalf("got %#v", got)
	}
}

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
func (m *matchTransport) InteractiveCommand(remoteCmd string) string                 { return remoteCmd }

func TestProbeAgentCatalogSuggestsCodexMultiAuthAccounts(t *testing.T) {
	listJSON := `{"accounts":[{"index":0,"label":"Account 1 (a@example.com)","enabled":true},{"index":1,"label":"Account 2","enabled":true}]}`
	tr := &matchTransport{
		id: "c1",
		rules: []struct{ contain, out string }{
			{contain: "codex-multi-auth list --json", out: listJSON},
			// loginShellRun single-quotes the script, so binaries appear as '\''bin'\''.
			{contain: `'\''codex-multi-auth-codex'\''`, out: "PRESENT"},
			{contain: `'\''codex'\''`, out: "PRESENT"},
			{contain: "codex login status", out: "Logged in"},
		},
	}
	detected := probeAgentCatalog(context.Background(), tr)
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
