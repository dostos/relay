package core

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

func TestProposedAccountAgentsSkipsExisting(t *testing.T) {
	tr := &matchTransport{
		id: "c1",
		rules: []struct{ contain, out string }{
			{contain: "ccs auth list", out: "│ hcs │\n│ personal │\n"},
			{contain: "codex-multi-auth list --json", out: `{"accounts":[{"index":0,"label":"A (a@example.com)","enabled":true},{"index":1,"label":"B","enabled":true}]}`},
		},
	}
	existing := []AgentSpec{{Name: "ccs:personal", Command: "ccs personal"}}
	proposed, skipped := proposedAccountAgents(context.Background(), tr, existing)
	var pnames, snames []string
	for _, a := range proposed {
		pnames = append(pnames, a.Name)
		if strings.HasPrefix(a.Name, "codex:") {
			if a.Command != "codex-multi-auth-codex" || len(a.Args) != 2 || a.Args[0] != "--account" {
				t.Fatalf("bad codex spec %#v", a)
			}
			if strings.Contains(strings.Join(a.Args, " "), "switch") {
				t.Fatal("must not use switch")
			}
		}
	}
	for _, a := range skipped {
		snames = append(snames, a.Name)
	}
	joined := strings.Join(pnames, ",")
	if !strings.Contains(joined, "ccs:hcs") || !strings.Contains(joined, "codex:a@example.com") || !strings.Contains(joined, "codex:2") {
		t.Fatalf("proposed=%v", pnames)
	}
	if strings.Contains(joined, "ccs:personal") {
		t.Fatalf("personal should be skipped, proposed=%v", pnames)
	}
	if !strings.Contains(strings.Join(snames, ","), "ccs:personal") {
		t.Fatalf("skipped=%v", snames)
	}
}

func TestMergeAccountAgentsPreservesOtherFields(t *testing.T) {
	p := &HostProfile{
		Version:  1,
		HostID:   "c1",
		Agents:   []AgentSpec{{Name: "claude", Command: "claude"}},
		PathMap:  []PathMapEntry{{Match: "relay", RemoteCWD: "~/gh/relay"}},
		Defaults: HostDefaults{PreferredAgent: "claude", SilenceSec: 10},
	}
	proposed := []AgentSpec{
		{Name: "ccs:hcs", Command: "ccs hcs"},
		{Name: "claude", Command: "claude"},
	}
	merged, added := mergeAccountAgents(p, proposed)
	if added != 1 {
		t.Fatalf("added=%d", added)
	}
	if len(merged.PathMap) != 1 || merged.Defaults.PreferredAgent != "claude" {
		t.Fatalf("lost fields: %#v", merged)
	}
	if len(p.Agents) != 1 {
		t.Fatal("original profile mutated")
	}
	if !hasAgent(merged.Agents, "ccs:hcs") || !hasAgent(merged.Agents, "claude") {
		t.Fatalf("agents=%#v", merged.Agents)
	}
}

type ensureTransport struct {
	id       string
	rules    []struct{ contain, out string }
	profile  string
	writes   []string
	writeErr error
}

func (e *ensureTransport) ID() string { return e.id }
func (e *ensureTransport) Run(_ context.Context, _, command string) (string, string, error) {
	for _, r := range e.rules {
		if strings.Contains(command, r.contain) {
			return r.out, "", nil
		}
	}
	return "", "", nil
}
func (e *ensureTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (e *ensureTransport) ReadFile(context.Context, string) ([]byte, error) {
	if e.profile == "" {
		return nil, io.EOF
	}
	return []byte(e.profile), nil
}
func (e *ensureTransport) WriteFile(_ context.Context, _ string, data []byte, _ string) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	e.writes = append(e.writes, string(data))
	e.profile = string(data)
	return nil
}
func (e *ensureTransport) Interactive(context.Context, string) error  { return nil }
func (e *ensureTransport) InteractiveCommand(remoteCmd string) string { return remoteCmd }

func ensureHappyRules() []struct{ contain, out string } {
	return []struct{ contain, out string }{
		{contain: "codex-multi-auth list --json", out: `{"accounts":[{"index":0,"label":"A (a@example.com)","enabled":true}]}`},
		{contain: "ccs auth list", out: "│ hcs │\n"},
		{contain: "codex-multi-auth rotation status", out: "Runtime rotation proxy: enabled\nStored setting: enabled\n"},
		{contain: `'\''codex-multi-auth-codex'\''`, out: "PRESENT"},
		{contain: `'\''codex-multi-auth'\''`, out: "PRESENT"},
		{contain: `'\''ccs'\''`, out: "PRESENT"},
		{contain: "codex login status", out: "Logged in"},
		{contain: "ccs hcs", out: "PONG"},
	}
}

func TestEnsureDryRunDoesNotWrite(t *testing.T) {
	tr := &ensureTransport{
		id:      "c1",
		rules:   ensureHappyRules(),
		profile: "version: 1\nhost_id: c1\nagents:\n  - name: claude\n    command: claude\n",
	}
	svc := &EnsureService{NewTransport: func(string) (ports.Transport, error) { return tr, nil }}
	res, err := svc.Ensure(context.Background(), "c1", EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.DryRun || len(tr.writes) != 0 {
		t.Fatalf("res=%+v writes=%d", res, len(tr.writes))
	}
	if len(res.ProposedAgents) == 0 {
		t.Fatal("expected proposed agents")
	}
	if !strings.Contains(res.Next, "--apply") {
		t.Fatalf("next=%q", res.Next)
	}
}

func TestEnsureApplyWritesMergedProfile(t *testing.T) {
	tr := &ensureTransport{
		id:      "c1",
		rules:   ensureHappyRules(),
		profile: "version: 1\nhost_id: c1\nagents:\n  - name: claude\n    command: claude\ndefaults:\n  preferred_agent: claude\n  silence_sec: 10\n",
	}
	svc := &EnsureService{NewTransport: func(string) (ports.Transport, error) { return tr, nil }}
	res, err := svc.Ensure(context.Background(), "c1", EnsureOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.WroteProfile || len(tr.writes) != 1 {
		t.Fatalf("res=%+v writes=%d", res, len(tr.writes))
	}
	body := tr.writes[0]
	if !strings.Contains(body, "ccs:hcs") || !strings.Contains(body, "codex:a@example.com") {
		t.Fatalf("body=%s", body)
	}
	if !strings.Contains(body, "preferred_agent: claude") || !strings.Contains(body, "name: claude") {
		t.Fatalf("lost existing fields: %s", body)
	}
	if strings.Contains(body, "switch") {
		t.Fatal("must not write switch")
	}
}

func TestEnsureMissingWrapperFails(t *testing.T) {
	tr := &ensureTransport{
		id: "c1",
		rules: []struct{ contain, out string }{
			{contain: "codex-multi-auth list --json", out: `{"accounts":[{"index":0,"label":"A (a@example.com)","enabled":true}]}`},
			{contain: "ccs auth list", out: ""},
			{contain: "codex-multi-auth rotation status", out: "Runtime rotation proxy: enabled\n"},
			{contain: `'\''codex-multi-auth-codex'\''`, out: "MISSING"},
			{contain: `'\''codex-multi-auth'\''`, out: "PRESENT"},
			{contain: `'\''ccs'\''`, out: "PRESENT"},
		},
		profile: "version: 1\nhost_id: c1\nagents:\n  - name: claude\n    command: claude\n",
	}
	svc := &EnsureService{NewTransport: func(string) (ports.Transport, error) { return tr, nil }}
	res, err := svc.Ensure(context.Background(), "c1", EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("expected ok=false: %+v", res)
	}
	if !strings.Contains(res.Detail, "codex-multi-auth-codex") {
		t.Fatalf("detail=%q", res.Detail)
	}
	if len(tr.writes) != 0 {
		t.Fatal("must not write on dep failure")
	}
}

func TestEnsureApplyRequiresProfile(t *testing.T) {
	tr := &ensureTransport{id: "c1", rules: ensureHappyRules(), profile: ""}
	svc := &EnsureService{NewTransport: func(string) (ports.Transport, error) { return tr, nil }}
	res, err := svc.Ensure(context.Background(), "c1", EnsureOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Next, "host init") {
		t.Fatalf("res=%+v", res)
	}
}
