package core

import (
	"testing"

	"github.com/dostos/relay/internal/ports"
)

func TestProjectLabel(t *testing.T) {
	cases := map[string]string{
		"dostos-workspace-cdx":    "cdx",
		"dostos-workspace-engram": "engram",
		"opaquebench-oqb":         "opaquebench-oqb",
		"beholder-minecraft-1":    "beholder-minecraft-1",
		"":                        "",
	}
	for in, want := range cases {
		if got := ProjectLabel(in); got != want {
			t.Fatalf("ProjectLabel(%q)=%q want %q", in, got, want)
		}
	}
}

func TestBrandTitle(t *testing.T) {
	if got := BrandTitle("dostos-workspace-cdx"); got != "◆ RELAY · cdx" {
		t.Fatalf("got %q", got)
	}
}

func TestSessionDisplayName(t *testing.T) {
	sess := &Session{
		Persist: ports.PersistHandle{Name: "dostos-workspace-6ec0e2"},
		Labels:  map[string]string{DisplayNameLabel: "personal-db"},
	}
	if got := SessionDisplayName(sess); got != "personal-db" {
		t.Fatalf("got %q", got)
	}
	delete(sess.Labels, DisplayNameLabel)
	if got := SessionDisplayName(sess); got != "6ec0e2" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestBrandStatus(t *testing.T) {
	got := BrandStatus([]string{"cdx", "opaquebench-oqb"})
	want := "◆ RELAY · cdx, opaquebench-oqb"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
