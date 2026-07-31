package cmux

import (
	"context"
	"testing"
)

func TestParseWorkspaceRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"list-panes shape", `{"workspace_ref":"workspace:6","panes":[]}`, "workspace:6"},
		{"missing field", `{"panes":[]}`, ""},
		{"empty", ``, ""},
		{"garbage", `not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseWorkspaceRef([]byte(c.in)); got != c.want {
				t.Fatalf("parseWorkspaceRef(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestActiveWorkspacePrefersCallingCmuxWorkspace(t *testing.T) {
	t.Setenv("CMUX_WORKSPACE_ID", "workspace:beholder-pdf")

	v := New()
	if got := v.activeWorkspace(context.Background()); got != "workspace:beholder-pdf" {
		t.Fatalf("activeWorkspace() = %q, want caller workspace", got)
	}
}
