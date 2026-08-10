package cli

import (
	"strings"
	"testing"
)

func registerUnder(t *testing.T, name, under string) string {
	t.Helper()
	args := []string{"--json", "parent", "register", "--headless", "--name", name, "--ttl", "1h"}
	if under != "" {
		args = append(args, "--under", under)
	}
	result := runJSON(t, args...)
	session, _ := result["session"].(map[string]any)
	id, _ := session["id"].(string)
	if id == "" {
		t.Fatalf("no session id in %v", result)
	}
	if under != "" && result["manager_session_id"] != under {
		t.Fatalf("registered manager = %v, want %s", result["manager_session_id"], under)
	}
	return id
}

// The channel-agent shape, end to end through the CLI: a root creates a
// manager under itself, that manager adopts an already-running session, and
// the manager can enumerate exactly its own subtree.
func TestParentRegisterUnderAndAdoptSessionBuildASubtree(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv("CMUX_SURFACE_REF", "")

	root := registerUnder(t, "cli-apex", "")
	channel := registerUnder(t, "cli-chan-gazer", root)
	if channel == root {
		t.Fatal("a child manager must be its own session")
	}
	// Idempotent, which is what makes it safe in a container start hook.
	if again := registerUnder(t, "cli-chan-gazer", root); again != channel {
		t.Fatalf("re-registration produced %s, want %s", again, channel)
	}

	orphan := registerUnder(t, "cli-orphan-manager", "")
	adopted := runJSON(t, "--json", "parent", "adopt", channel, orphan)
	if adopted["child_session_id"] != orphan || adopted["old_parent_session_id"] != "" {
		t.Fatalf("adopt = %v", adopted)
	}

	scoped := runJSON(t, "--json", "parent", "list", "--under", channel)
	parents, _ := scoped["parents"].([]any)
	if len(parents) != 2 {
		t.Fatalf("subtree of %s = %v", channel, scoped)
	}
	whole := runJSON(t, "--json", "parent", "list", "--under", root)
	all, _ := whole["parents"].([]any)
	if len(all) != 3 {
		t.Fatalf("subtree of %s = %v", root, whole)
	}
}

// The policy reads the FIRST occurrence of a flag; an ordinary parse loop keeps
// the LAST. Every one of these argv shapes must be refused by the command too,
// or a caller can have one value authorized and another one executed —
// `--under <self> --under ""` being the worst of them: authorized as "inside my
// own subtree", executed as "a brand new root".
func TestRepeatedOrBlankFlagsAreRefusedByTheCommandToo(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv("CMUX_SURFACE_REF", "")
	root := registerUnder(t, "cli-apex", "")
	other := registerUnder(t, "cli-other", "")

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"repeated --under emptied", []string{"--json", "parent", "register", "--headless", "--name", "evil", "--under", root, "--under", ""}},
		{"repeated --under rehomed", []string{"--json", "parent", "register", "--headless", "--name", "evil", "--under", root, "--under", other}},
		{"blank --under", []string{"--json", "parent", "register", "--headless", "--name", "evil", "--under", "   "}},
		{"repeated --name", []string{"--json", "parent", "register", "--headless", "--name", "a", "--name", "b", "--under", root}},
		{"repeated --under on list", []string{"--json", "parent", "list", "--under", root, "--under", other}},
		{"blank --under on list", []string{"--json", "parent", "list", "--under", ""}},
		{"repeated --from on adopt", []string{"--json", "parent", "adopt", root, other, "--from", root, "--from", other}},
	} {
		out := captureStdout(t, func() {
			if code := New().Run(tc.argv); code == 0 {
				t.Fatalf("%s: must be refused", tc.name)
			}
		})
		if !strings.Contains(out, "once") && !strings.Contains(out, "non-empty") {
			t.Fatalf("%s: refusal must say why: %q", tc.name, out)
		}
	}
	// Nothing was created by any of the refusals.
	listed := runJSON(t, "--json", "parent", "list")
	parents, _ := listed["parents"].([]any)
	if len(parents) != 2 {
		t.Fatalf("a refused registration created something: %v", listed)
	}
}

func TestParentAdoptRefusesAnUnconfirmedMoveFromTheCLI(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv("CMUX_SURFACE_REF", "")

	root := registerUnder(t, "cli-apex", "")
	gazer := registerUnder(t, "cli-chan-gazer", root)
	engram := registerUnder(t, "cli-chan-engram", root)
	child := registerUnder(t, "cli-child", "")
	runJSON(t, "--json", "parent", "adopt", gazer, child)

	out := captureStdout(t, func() {
		if code := New().Run([]string{"--json", "parent", "adopt", engram, child}); code == 0 {
			t.Fatal("an unconfirmed move must fail")
		}
	})
	if !strings.Contains(out, "--from") || !strings.Contains(out, gazer) {
		t.Fatalf("refusal must name the current manager and the confirming flag: %q", out)
	}

	moved := runJSON(t, "--json", "parent", "adopt", engram, child, "--from", gazer)
	if moved["old_parent_session_id"] != gazer || moved["moved"] != true {
		t.Fatalf("confirmed move = %v", moved)
	}
}
