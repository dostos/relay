package core

import (
	"strings"
	"testing"
)

func TestSpecForAgentCCS(t *testing.T) {
	spec, err := SpecForAgent(nil, "ccs:personal")
	if err != nil || spec.Command != "ccs personal" {
		t.Fatalf("%+v err=%v", spec, err)
	}
	p := &HostProfile{Agents: []AgentSpec{{Name: "ccs:hcs", Command: "ccs hcs"}}}
	spec, err = SpecForAgent(p, "ccs:hcs")
	if err != nil || spec.Name != "ccs:hcs" {
		t.Fatalf("%+v err=%v", spec, err)
	}
}

func TestWrapLoginShellIdempotent(t *testing.T) {
	once := wrapLoginShell("ccs personal")
	twice := wrapLoginShell(once)
	if once != twice {
		t.Fatalf("double wrap: %q vs %q", once, twice)
	}
	if !strings.HasPrefix(once, "bash -ilc ") {
		t.Fatalf("got %q", once)
	}
}

func TestExtractWrappedURL(t *testing.T) {
	capture := `Opening browser to sign in…
If the browser didn't open, visit: https://claude.com/ca
i/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9
-88ed-5944d1962f5e&response_type=code&redirect_uri=https
%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&s
cope=org%3Acreate_api_key+user%3Aprofile
Paste code here if prompted >
`
	u := ExtractWrappedURL(capture)
	wantPrefix := "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	if !strings.HasPrefix(u, wantPrefix) {
		t.Fatalf("got %q", u)
	}
	if strings.Contains(u, "\n") || strings.Contains(u, "Paste") {
		t.Fatalf("url still dirty: %q", u)
	}
}
