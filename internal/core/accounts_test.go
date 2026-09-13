package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

// fakeAccounts stands in for the agent-accounts CLI: it records every argv
// and answers `logins` and `status` from canned JSON.
type fakeAccounts struct {
	calls  [][]string
	logins string
	status string
	err    error
}

func (f *fakeAccounts) run(_ context.Context, args []string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.err != nil {
		return "", f.err
	}
	for _, a := range args {
		switch a {
		case "logins":
			return f.logins, nil
		case "status":
			return f.status, nil
		}
	}
	return "", errors.New("fake: unknown verb")
}

func TestAccountLoginsAsksTheHostAndParses(t *testing.T) {
	fa := &fakeAccounts{logins: `[{"backend":"claude","name":"personal","handle":"/h/p","enabled":true,"is_default":true,"markers":[],"usable":true}]`}
	rows, err := AccountLogins(context.Background(), fa.run, "hamburg", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fa.calls[0], " ") != "--json --host hamburg logins --backend claude" {
		t.Fatalf("argv: %v", fa.calls[0])
	}
	if len(rows) != 1 || rows[0].Name != "personal" || !rows[0].Usable || !rows[0].IsDefault {
		t.Fatalf("rows: %+v", rows)
	}
	// The local identity is this machine: no --host.
	if _, err := AccountLogins(context.Background(), fa.run, LocalHostID, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fa.calls[1], " ") != "--json logins" {
		t.Fatalf("local argv: %v", fa.calls[1])
	}
}

func TestAgentNameForLoginIsRelaysSpellingOfTheCatalogsFact(t *testing.T) {
	cases := []struct {
		in   AccountLogin
		want string
	}{
		{AccountLogin{Backend: "claude", Name: "hcs", Handle: "/h/hcs"}, "ccs:hcs"},
		{AccountLogin{Backend: "codex", Name: "Account 1 (dostos, dostos10@gmail.com)", Handle: "0"}, "codex:dostos10@gmail.com"},
		{AccountLogin{Backend: "codex", Name: "Account 2", Handle: "1"}, "codex:2"},
		{AccountLogin{Backend: "cursor-agent", Name: "host", Handle: "host"}, "cursor-agent"},
	}
	for _, c := range cases {
		if got := AgentNameForLogin(c.in); got != c.want {
			t.Errorf("%+v → %q, want %q", c.in, got, c.want)
		}
	}
	// And the spec behind a ccs name never wraps ccs around the CLI.
	spec := agentSpecForLogin(cases[0].in)
	if spec.Command != "claude" || strings.Contains(spec.InnerCommand(), "ccs ") {
		t.Fatalf("never_wrap violated: %+v", spec)
	}
}

func TestAuthStatusRendersTheCatalogAndRefusesToGuess(t *testing.T) {
	fa := &fakeAccounts{status: `[
		{"backend":"claude","name":"hcs-old","handle":"/h/old","state":"ok","confidence":"local-expiry","detail":"token valid","expires_in_s":7200},
		{"backend":"codex","name":"A (a@example.com)","handle":"0","state":"unknown","confidence":"declared","detail":"quota exhausted"},
		{"backend":"droid","name":"host","handle":"host","state":"unknown","confidence":"none","detail":"one host login and no manager to ask"}
	]`}
	svc := &AuthService{
		Profiles:     &ProfileService{NewTransport: func(string) (ports.Transport, error) { return &ensureTransport{id: "c1"}, nil }},
		NewTransport: func(string) (ports.Transport, error) { return &ensureTransport{id: "c1"}, nil },
		Accounts:     fa.run,
	}
	rows, err := svc.Status(context.Background(), "c1", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fa.calls[0], " ") != "--json --host c1 status" {
		t.Fatalf("argv: %v", fa.calls[0])
	}
	byName := map[string]AuthStatusRow{}
	for _, r := range rows {
		byName[r.Agent] = r
	}
	old := byName["ccs:hcs-old"]
	if !old.Authed || old.Confidence != "local-expiry" || !strings.HasPrefix(old.Detail, "local-expiry: ") {
		t.Fatalf("confidence must reach the row, so a green expiry is not read as usable: %+v", old)
	}
	if c := byName["codex:a@example.com"]; c.Authed || c.State != "unknown" {
		t.Fatalf("an unusable login must not render authed: %+v", c)
	}
	if d := byName["droid"]; !d.Present || d.Authed || d.Confidence != "none" {
		t.Fatalf("a host login with no manager is present and unjudged: %+v", d)
	}

	// --agent narrows, by relay name or by backend, and names what it cannot find.
	if rows, err := svc.Status(context.Background(), "c1", "codex"); err != nil || len(rows) == 0 {
		t.Fatalf("backend filter: %v %v", rows, err)
	} else {
		for _, r := range rows {
			if r.Backend != "codex" && r.Agent != "codex" {
				t.Fatalf("backend filter leaked %+v", r)
			}
		}
	}
	if _, err := svc.Status(context.Background(), "c1", "ccs:nope"); err == nil || !strings.Contains(err.Error(), "agent-accounts logins --host c1") {
		t.Fatalf("expected a pointer at the catalog, got %v", err)
	}

	// No CLI: say so. Never fall back to relay's own discovery.
	missing := &AuthService{Profiles: svc.Profiles, NewTransport: svc.NewTransport, Accounts: (&fakeAccounts{err: ErrAccountsCLIMissing}).run}
	if _, err := missing.Status(context.Background(), "c1", ""); err == nil || !strings.Contains(err.Error(), "pip install -e projects/infrastructure/agent-accounts") {
		t.Fatalf("expected the install hint, got %v", err)
	}
}

func TestProbeCatalogSuggestsLoginsFromTheCatalogOnly(t *testing.T) {
	fa := &fakeAccounts{logins: `[{"backend":"claude","name":"hcs","handle":"/h/hcs","enabled":true,"usable":true},
		{"backend":"claude","name":"stale","handle":"/h/stale","enabled":false,"usable":false}]`}
	tr := &matchTransport{id: "c1", rules: []struct{ contain, out string }{
		{contain: `'\''ccs'\''`, out: "PRESENT"},
	}}
	var names []string
	for _, d := range probeAgentCatalog(context.Background(), tr, "c1", fa.run) {
		if d.SuggestedSpec != nil && strings.HasPrefix(d.SuggestedSpec.Name, "ccs:") {
			names = append(names, d.SuggestedSpec.Name)
		}
	}
	if strings.Join(names, ",") != "ccs:hcs" {
		t.Fatalf("suggested %v: a disabled login is not proposed, and nothing is parsed off ccs's own output", names)
	}
	// Without the CLI, discover proposes no logins and does not invent them.
	for _, d := range probeAgentCatalog(context.Background(), tr, "c1", nil) {
		if d.SuggestedSpec != nil && strings.HasPrefix(d.SuggestedSpec.Name, "ccs:") {
			t.Fatalf("no CLI, yet a login was suggested: %+v", d)
		}
	}
}
