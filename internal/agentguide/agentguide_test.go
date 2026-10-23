package agentguide

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnv(t *testing.T, shell string) Env {
	t.Helper()
	home := t.TempDir()
	return Env{Home: home, CodexHome: filepath.Join(home, ".codex"), Shell: shell,
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallIdempotentAndRespectsUserContent(t *testing.T) {
	e := testEnv(t, "/bin/sh")
	mkdir(t, filepath.Join(e.Home, ".claude"))
	mkdir(t, e.CodexHome)
	userAgents := "# My rules\n\nAlways be brief.\n"
	if err := os.WriteFile(filepath.Join(e.CodexHome, "AGENTS.md"), []byte(userAgents), 0o600); err != nil {
		t.Fatal(err)
	}
	// a user-owned cursor skill (no marker) must survive
	mkdir(t, filepath.Join(e.Home, ".cursor", "skills", "relay"))
	userSkill := "---\nname: relay\n---\nmine\n"
	cursorSkill := filepath.Join(e.Home, ".cursor", "skills", "relay", "SKILL.md")
	if err := os.WriteFile(cursorSkill, []byte(userSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	dry := Install(e, Options{DryRun: true, NoPath: true})
	if !dry.Changed || exists(filepath.Join(e.Home, ".claude", "skills", "relay", "SKILL.md")) {
		t.Fatalf("dry run wrote or planned nothing: %+v", dry)
	}
	r := Install(e, Options{NoPath: true})
	if !r.Changed {
		t.Fatalf("nothing installed: %+v", r)
	}
	got, _ := os.ReadFile(filepath.Join(e.CodexHome, "AGENTS.md"))
	if !strings.HasPrefix(string(got), userAgents) || !strings.Contains(string(got), blockBegin) {
		t.Fatalf("AGENTS.md user content not preserved:\n%s", got)
	}
	if b, _ := os.ReadFile(cursorSkill); string(b) != userSkill {
		t.Fatalf("user skill clobbered")
	}
	skill, _ := os.ReadFile(filepath.Join(e.Home, ".claude", "skills", "relay", "SKILL.md"))
	if !strings.HasPrefix(string(skill), "---\nname: relay\ndescription: ") {
		t.Fatalf("skill frontmatter: %s", skill)
	}
	again := Install(e, Options{NoPath: true})
	if again.Changed {
		t.Fatalf("second install changed something: %+v", again.Targets)
	}
	// user edits around the block survive a block update
	edited := strings.Replace(string(got), "Always be brief.", "Always be brief!", 1)
	edited = strings.Replace(edited, "## relay (inter-session messaging)", "## relay OLD", 1) + "\ntrailing user note\n"
	_ = os.WriteFile(filepath.Join(e.CodexHome, "AGENTS.md"), []byte(edited), 0o600)
	Install(e, Options{NoPath: true})
	got, _ = os.ReadFile(filepath.Join(e.CodexHome, "AGENTS.md"))
	if !strings.Contains(string(got), "Always be brief!") || !strings.Contains(string(got), "trailing user note") || strings.Contains(string(got), "relay OLD") || strings.Count(string(got), blockBegin) != 1 {
		t.Fatalf("block update damaged user content:\n%s", got)
	}
	st, _ := os.Stat(filepath.Join(e.CodexHome, "AGENTS.md"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed: %v", st.Mode())
	}
	forced := Install(e, Options{NoPath: true, Force: true})
	if b, _ := os.ReadFile(cursorSkill); string(b) != SkillFile() {
		t.Fatalf("--force did not replace: %+v", forced.Targets)
	}
	baks, _ := filepath.Glob(cursorSkill + ".bak.*")
	if len(baks) != 1 {
		t.Fatalf("no backup")
	}
}

func TestCcsInstances(t *testing.T) {
	e := testEnv(t, "/bin/sh")
	mkdir(t, filepath.Join(e.Home, ".claude", "skills"))
	mkdir(t, filepath.Join(e.Home, ".ccs", "instances", "work"))
	mkdir(t, filepath.Join(e.Home, ".ccs", "instances", "bare", "skills"))
	if err := os.Symlink(filepath.Join(e.Home, ".claude", "skills"), filepath.Join(e.Home, ".ccs", "instances", "work", "skills")); err != nil {
		t.Fatal(err)
	}
	Install(e, Options{NoPath: true})
	if !exists(filepath.Join(e.Home, ".ccs", "instances", "bare", "skills", "relay", "SKILL.md")) {
		t.Fatal("bare ccs profile did not get the skill")
	}
	if !exists(filepath.Join(e.Home, ".ccs", "instances", "work", "skills", "relay", "SKILL.md")) {
		t.Fatal("shared ccs profile does not see the claude skill")
	}
	d := Doctor(e)
	for _, a := range d.Agents {
		if (a.Agent == "ccs" || a.Agent == "claude") && !a.SeesGuidance {
			t.Fatalf("%s does not see guidance: %+v", a.Agent, a)
		}
	}
}

func TestPathFixForNonInteractiveBash(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash")
	}
	e := testEnv(t, "/bin/bash")
	bin := filepath.Join(e.Home, ".local", "bin")
	mkdir(t, bin)
	if err := os.WriteFile(filepath.Join(bin, "relay"), []byte("#!/bin/sh\necho relay\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Debian-style bashrc that returns early for non-interactive shells.
	bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"
	_ = os.WriteFile(filepath.Join(e.Home, ".bashrc"), []byte(bashrc), 0o644)
	before := CheckPath(e)
	for _, c := range before {
		if c.OK {
			t.Fatalf("%s unexpectedly found relay before the fix", c.Check)
		}
	}
	r := Install(e, Options{})
	for _, c := range r.Path {
		if !c.OK {
			t.Fatalf("%s still fails after install: %+v fixes=%+v", c.Check, c, r.PathFix)
		}
	}
	got, _ := os.ReadFile(filepath.Join(e.Home, ".bashrc"))
	if !strings.HasPrefix(string(got), pathBegin) || !strings.HasSuffix(string(got), bashrc) {
		t.Fatalf("bashrc:\n%s", got)
	}
	again := Install(e, Options{})
	if len(again.PathFix) != 0 {
		t.Fatalf("second install planned more PATH fixes: %+v", again.PathFix)
	}
}

// #83: ccs bookkeeping dot-dirs (~/.ccs/instances/.locks) are not instances;
// a relay skill an earlier install left there is removed, nothing else is.
func TestCcsDotDirsAreNotInstances(t *testing.T) {
	e := testEnv(t, "/bin/sh")
	mkdir(t, filepath.Join(e.Home, ".claude", "skills"))
	mkdir(t, filepath.Join(e.Home, ".ccs", "instances", "bare", "skills"))
	locks := filepath.Join(e.Home, ".ccs", "instances", ".locks")
	mkdir(t, locks)
	_ = os.WriteFile(filepath.Join(e.Home, ".ccs", "instances", "stray-file"), []byte("x"), 0o644)
	leftover := filepath.Join(locks, "skills", "relay", "SKILL.md")
	mkdir(t, filepath.Dir(leftover))
	_ = os.WriteFile(leftover, []byte(SkillFile()), 0o644)
	// a user file under another dot-dir is never touched
	other := filepath.Join(e.Home, ".ccs", "instances", ".cache", "skills", "relay", "SKILL.md")
	mkdir(t, filepath.Dir(other))
	_ = os.WriteFile(other, []byte("mine"), 0o644)

	if st := Status(e); st.OK {
		t.Fatalf("status should flag the orphan: %+v", st)
	}
	r := Install(e, Options{NoPath: true})
	if !r.OK {
		t.Fatalf("install: %+v", r.Problems)
	}
	for _, tg := range r.Targets {
		if strings.Contains(tg.ID, "stray-file") || tg.ID == "ccs-skill:.locks" || strings.Contains(tg.ID, ".cache") {
			t.Fatalf("non-instance targeted: %+v", tg)
		}
	}
	if exists(leftover) || exists(filepath.Join(locks, "skills")) {
		t.Fatal("orphaned relay skill (and its empty dirs) not removed")
	}
	if !exists(locks) {
		t.Fatal(".locks itself must stay")
	}
	if b, _ := os.ReadFile(other); string(b) != "mine" {
		t.Fatal("user file touched")
	}
	if exists(filepath.Join(e.Home, ".ccs", "instances", ".locks", "skills", "relay", "SKILL.md")) {
		t.Fatal("skill rewritten into .locks")
	}
	if !exists(filepath.Join(e.Home, ".ccs", "instances", "bare", "skills", "relay", "SKILL.md")) {
		t.Fatal("real instance lost its skill")
	}
	if st := Status(e); !st.OK {
		t.Fatalf("status after install: %+v", st.Problems)
	}
}
