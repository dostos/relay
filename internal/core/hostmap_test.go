package core

import (
	"strings"
	"testing"
)

const commentedProfile = `version: 1
host_id: hamburg

# Which agents this host can run. The order matters to nobody but the reader.
agents:
  - name: claude
    command: claude

path_map:
  # beholder lives outside the workspace checkout on this box
  - match: beholder
    remote_cwd: ~/gh/beholder
  - match: relay
    remote_cwd: ~/gh/dostos-workspace

defaults:
  preferred_agent: claude
`

// The whole reason this edits a node tree instead of re-marshalling the struct.
func TestUpsertPathMapKeepsCommentsAndUnrelatedKeys(t *testing.T) {
	out, err := UpsertPathMapEntry([]byte(commentedProfile), "opaquebench", "~/gh/opaquebench")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, comment := range []string{
		"# Which agents this host can run",
		"# beholder lives outside the workspace checkout on this box",
	} {
		if !strings.Contains(got, comment) {
			t.Errorf("comment erased: %q\n---\n%s", comment, got)
		}
	}
	for _, keep := range []string{"host_id: hamburg", "preferred_agent: claude", "name: claude"} {
		if !strings.Contains(got, keep) {
			t.Errorf("unrelated content lost: %q\n---\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "match: opaquebench") || !strings.Contains(got, "remote_cwd: ~/gh/opaquebench") {
		t.Errorf("new entry missing:\n%s", got)
	}
	if !strings.Contains(got, "match: beholder") || !strings.Contains(got, "match: relay") {
		t.Errorf("existing entries lost:\n%s", got)
	}
}

// Two entries claiming the same repo is a file that answers one question twice.
func TestUpsertPathMapUpdatesRatherThanDuplicates(t *testing.T) {
	out, err := UpsertPathMapEntry([]byte(commentedProfile), "beholder", "/data3/beholder")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Count(got, "match: beholder") != 1 {
		t.Errorf("expected exactly one beholder entry:\n%s", got)
	}
	if !strings.Contains(got, "remote_cwd: /data3/beholder") {
		t.Errorf("entry not updated:\n%s", got)
	}
	if strings.Contains(got, "remote_cwd: ~/gh/beholder") {
		t.Errorf("old value left behind:\n%s", got)
	}
}

func TestUpsertPathMapCreatesTheSectionWhenAbsent(t *testing.T) {
	minimal := "version: 1\nhost_id: c1\n"
	out, err := UpsertPathMapEntry([]byte(minimal), "engram", "~/gh/engram")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "path_map:") || !strings.Contains(got, "match: engram") {
		t.Errorf("section not created:\n%s", got)
	}
	// The result has to still parse as a host profile, not merely look like one.
	profile, err := ParseHostProfileYAML([]byte(got))
	if err != nil {
		t.Fatalf("result no longer parses: %v\n%s", err, got)
	}
	if len(profile.PathMap) != 1 || profile.PathMap[0].RemoteCWD != "~/gh/engram" {
		t.Errorf("parsed back wrong: %+v", profile.PathMap)
	}
}

func TestUpsertPathMapRefusesEmptyInput(t *testing.T) {
	if _, err := UpsertPathMapEntry([]byte(commentedProfile), "", "~/x"); err == nil {
		t.Error("expected an error for an empty match")
	}
	if _, err := UpsertPathMapEntry([]byte(commentedProfile), "x", ""); err == nil {
		t.Error("expected an error for an empty remote_cwd")
	}
	if _, err := UpsertPathMapEntry([]byte("- not: a mapping\n"), "x", "~/y"); err == nil {
		t.Error("expected an error for a non-mapping document")
	}
}

func TestRemoteDirsCommandIsBoundedAndQuoted(t *testing.T) {
	got, err := RemoteDirsCommand("~/gh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "head -500") {
		t.Errorf("listing is unbounded: %s", got)
	}
	if !strings.Contains(got, "grep '/$'") {
		t.Errorf("listing is not directories-only: %s", got)
	}
	if !strings.Contains(got, "$HOME") {
		t.Errorf("tilde not expanded by the remote shell: %s", got)
	}
	if _, err := RemoteDirsCommand("bad\npath"); err == nil {
		t.Error("expected an error for a path with a newline")
	}
}

// Blank lines are the one thing the encoder does not keep, so the guarantee is
// stated as content-and-comments, and pinned here rather than assumed.
func TestUpsertPathMapPreservesEveryNonBlankLine(t *testing.T) {
	out, err := UpsertPathMapEntry([]byte(commentedProfile), "opaquebench", "~/gh/opaquebench")
	if err != nil {
		t.Fatal(err)
	}
	before := nonBlankLines(commentedProfile)
	after := map[string]bool{}
	for _, line := range nonBlankLines(string(out)) {
		after[line] = true
	}
	for _, line := range before {
		if !after[line] {
			t.Errorf("line lost: %q\n---\n%s", line, out)
		}
	}
}

func nonBlankLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, " "))
		}
	}
	return out
}
