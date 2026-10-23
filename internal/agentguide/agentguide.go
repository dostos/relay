// Package agentguide installs and verifies the agent-facing relay guidance
// (#76): a skill for Claude Code (and ccs profiles), Codex and cursor-agent,
// a managed block in ~/.codex/AGENTS.md, and ~/.local/bin on PATH for the
// shells agent CLIs and ssh use. Every write is idempotent and confined to
// files relay owns (marked) or to marked blocks inside user files.
package agentguide

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed relay.md
var guide string

// Version bumps when the guidance format changes.
const Version = "1"

const (
	skillMarker = "<!-- relay-managed: written by `relay agents install`; local edits are replaced. Delete this line to take ownership. -->"
	blockBegin  = "<!-- >>> relay managed (relay agents install) >>> -->"
	blockEnd    = "<!-- <<< relay managed <<< -->"
	pathBegin   = "# >>> relay managed: PATH (relay agents install) >>>"
	pathEnd     = "# <<< relay managed: PATH <<<"
)

const skillDescription = "Message another agent session (Claude Code, Codex, cursor-agent in tmux on any fleet host) through relay with delivery receipts, and wait for its reply. Use when asked to hand off to, ask, notify, request review from, or coordinate with another session or agent, or when you see relay HOST/NAME or sess-… session ids."

// Guide returns the guidance markdown body.
func Guide() string { return guide }

// SkillFile is the full SKILL.md content.
func SkillFile() string {
	return "---\nname: relay\ndescription: " + skillDescription + "\n---\n" + skillMarker + "\n\n" + guide
}

// AgentsBlock is the always-loaded AGENTS.md block (Codex).
func AgentsBlock() string {
	return blockBegin + `
## relay (inter-session messaging)
To message another agent session (tmux pane on any fleet host) use relay, never raw tmux send-keys:
` + "`relay --json session send HOST/NAME --idempotency-key KEY -- \"text\"`" + ` then
` + "`relay --json session wait HOST/NAME --idle --timeout 100s --tail 60`" + `.
Success = exit 0 with receipt.state submitted|queued. Exit 67 (deposited/uncertain): inspect with
` + "`relay session capture`" + `, never resend blindly. not_sent + composer_not_empty: someone's draft is in the
target input; do not clear it. Full guide: the relay skill, or ` + "`relay help messaging`" + `.
` + blockEnd + "\n"
}

// PathBlock is the shell snippet that puts ~/.local/bin on PATH.
func PathBlock() string {
	return pathBegin + "\ncase \":$PATH:\" in *\":$HOME/.local/bin:\"*) ;; *) PATH=\"$HOME/.local/bin:$PATH\"; export PATH ;; esac\n" + pathEnd + "\n"
}

// Target is one guidance file.
type Target struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	Path  string `json:"path"`
	Kind  string `json:"kind"` // skill | block
	// State: missing | current | stale | conflict | shared
	State string `json:"state"`
	// Action planned/taken: create | update | none | skip_conflict | skip_agent_absent | backup_and_replace
	Action string `json:"action,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Agent is one agent CLI's presence.
type Agent struct {
	Name      string `json:"name"`
	Binary    string `json:"binary,omitempty"`
	ConfigDir string `json:"config_dir,omitempty"`
	Present   bool   `json:"present"`
}

// Env is the environment the package reads (injectable for tests).
type Env struct {
	Home      string
	CodexHome string
	Shell     string
	LookPath  func(string) (string, error)
}

// DefaultEnv reads the process environment.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	codex := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return Env{Home: home, CodexHome: codex, Shell: os.Getenv("SHELL"), LookPath: lookPathWide(home)}
}

// lookPathWide finds a binary on PATH or in the usual per-user install
// directories, since agent CLIs often live where a non-login PATH misses.
func lookPathWide(home string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
		for _, d := range []string{".local/bin", ".npm-global/bin", ".bun/bin", ".cargo/bin", "bin", ".volta/bin"} {
			p := filepath.Join(home, d, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return p, nil
			}
		}
		for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
			p := filepath.Join(d, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
		return "", os.ErrNotExist
	}
}

// Agents detects the agent CLIs on this machine.
func Agents(e Env) []Agent {
	type spec struct {
		name, dir string
		bins      []string
	}
	specs := []spec{
		{"claude", filepath.Join(e.Home, ".claude"), []string{"claude"}},
		{"ccs", filepath.Join(e.Home, ".ccs"), []string{"ccs"}},
		{"codex", e.CodexHome, []string{"codex"}},
		{"cursor-agent", filepath.Join(e.Home, ".cursor"), []string{"cursor-agent"}},
	}
	out := make([]Agent, 0, len(specs))
	for _, s := range specs {
		a := Agent{Name: s.name}
		for _, b := range s.bins {
			if p, err := e.LookPath(b); err == nil {
				a.Binary = p
				break
			}
		}
		if st, err := os.Stat(s.dir); err == nil && st.IsDir() {
			a.ConfigDir = s.dir
		}
		a.Present = a.Binary != "" || a.ConfigDir != ""
		out = append(out, a)
	}
	return out
}

// Targets lists every guidance file for this machine with its state.
func Targets(e Env) []Target {
	ts := []Target{
		{ID: "claude-skill", Agent: "claude", Kind: "skill", Path: filepath.Join(e.Home, ".claude", "skills", "relay", "SKILL.md")},
		{ID: "codex-skill", Agent: "codex", Kind: "skill", Path: filepath.Join(e.CodexHome, "skills", "relay", "SKILL.md")},
		{ID: "codex-agents-md", Agent: "codex", Kind: "block", Path: filepath.Join(e.CodexHome, "AGENTS.md")},
		{ID: "cursor-skill", Agent: "cursor-agent", Kind: "skill", Path: filepath.Join(e.Home, ".cursor", "skills", "relay", "SKILL.md")},
	}
	// ccs profiles normally share ~/.claude/skills through symlinks; a
	// profile-local (bare) instance has its own skills dir and needs a copy.
	if inst, err := filepath.Glob(filepath.Join(e.Home, ".ccs", "instances", "*")); err == nil {
		sort.Strings(inst)
		for _, d := range inst {
			skills := filepath.Join(d, "skills")
			st, err := os.Lstat(skills)
			t := Target{ID: "ccs-skill:" + filepath.Base(d), Agent: "ccs", Kind: "skill", Path: filepath.Join(skills, "relay", "SKILL.md")}
			if err == nil && st.Mode()&os.ModeSymlink != 0 {
				t.Kind = "shared"
				t.Detail = "skills shared via symlink (~/.ccs/shared/skills → ~/.claude/skills)"
			}
			ts = append(ts, t)
		}
	}
	for i := range ts {
		ts[i].State = stateOf(ts[i])
	}
	return ts
}

func stateOf(t Target) string {
	if t.Kind == "shared" {
		return "shared"
	}
	raw, err := os.ReadFile(t.Path)
	if err != nil {
		return "missing"
	}
	s := string(raw)
	switch t.Kind {
	case "skill":
		if !strings.Contains(s, skillMarker) {
			return "conflict"
		}
		if s == SkillFile() {
			return "current"
		}
		return "stale"
	case "block":
		b, e := strings.Index(s, blockBegin), strings.Index(s, blockEnd)
		if b < 0 || e < b {
			return "missing"
		}
		if s[b:e+len(blockEnd)]+"\n" == AgentsBlock() {
			return "current"
		}
		return "stale"
	}
	return "missing"
}

// Options for Install.
type Options struct {
	DryRun bool
	// All installs for every agent, present or not.
	All bool
	// Force replaces an unmarked (user-owned) skill file after backing it up.
	Force bool
	// NoPath skips the PATH fixes.
	NoPath bool
}

// Result of Install / Status.
type Result struct {
	OK       bool        `json:"ok"`
	DryRun   bool        `json:"dry_run,omitempty"`
	Version  string      `json:"guidance_version"`
	Hash     string      `json:"guidance_hash"`
	Agents   []Agent     `json:"agents"`
	Targets  []Target    `json:"targets"`
	Path     []PathCheck `json:"path_checks,omitempty"`
	PathFix  []PathFix   `json:"path_fixes,omitempty"`
	Changed  bool        `json:"changed"`
	Problems []string    `json:"problems,omitempty"`
	Next     []string    `json:"argv,omitempty"`
}

// Hash identifies the guidance content.
func Hash() string {
	sum := sha256.Sum256([]byte(SkillFile() + AgentsBlock()))
	return hex.EncodeToString(sum[:])[:12]
}

func present(agents []Agent, name string) bool {
	for _, a := range agents {
		if a.Name == name {
			return a.Present
		}
	}
	return false
}

// Status reports agents, targets and PATH checks without writing.
func Status(e Env) Result {
	r := Result{OK: true, Version: Version, Hash: Hash(), Agents: Agents(e), Targets: Targets(e)}
	for i, t := range r.Targets {
		if !present(r.Agents, t.Agent) && t.State == "missing" {
			r.Targets[i].Action = "skip_agent_absent"
			continue
		}
		switch t.State {
		case "missing", "stale":
			r.OK = false
			r.Problems = append(r.Problems, fmt.Sprintf("%s %s: %s", t.Agent, t.State, t.Path))
		case "conflict":
			r.OK = false
			r.Problems = append(r.Problems, fmt.Sprintf("%s: %s exists and is not relay-managed (install --force backs it up and replaces it)", t.Agent, t.Path))
		}
	}
	if !r.OK {
		r.Next = []string{"relay", "agents", "install"}
	}
	return r
}

// Install writes missing/stale guidance (and PATH fixes) idempotently.
func Install(e Env, o Options) Result {
	r := Status(e)
	r.OK, r.Problems, r.Next, r.DryRun = true, nil, nil, o.DryRun
	for i := range r.Targets {
		t := &r.Targets[i]
		if t.Kind == "shared" {
			t.Action = "none"
			continue
		}
		if !o.All && !present(r.Agents, t.Agent) {
			t.Action = "skip_agent_absent"
			continue
		}
		switch t.State {
		case "current":
			t.Action = "none"
			continue
		case "missing":
			t.Action = "create"
		case "stale":
			t.Action = "update"
		case "conflict":
			if !o.Force {
				t.Action = "skip_conflict"
				r.Problems = append(r.Problems, fmt.Sprintf("%s is user-owned (no relay marker); left untouched. Re-run with --force to back it up and replace it", t.Path))
				continue
			}
			t.Action = "backup_and_replace"
		}
		if o.DryRun {
			r.Changed = true
			continue
		}
		var err error
		switch t.Kind {
		case "skill":
			if t.Action == "backup_and_replace" {
				err = backup(t.Path)
			}
			if err == nil {
				err = writeAtomic(t.Path, []byte(SkillFile()), 0o644)
			}
		case "block":
			err = upsertBlock(t.Path, AgentsBlock(), blockBegin, blockEnd, false)
		}
		if err != nil {
			t.Detail = err.Error()
			r.OK = false
			r.Problems = append(r.Problems, err.Error())
			continue
		}
		t.State = stateOf(*t)
		r.Changed = true
	}
	if !o.NoPath {
		r.Path = CheckPath(e)
		fixes := planPathFixes(e, r.Path)
		for i := range fixes {
			if o.DryRun {
				fixes[i].Action = "would_" + fixes[i].Action
				r.Changed = true
				continue
			}
			if err := upsertBlock(fixes[i].File, PathBlock(), pathBegin, pathEnd, fixes[i].Top); err != nil {
				fixes[i].Error = err.Error()
				r.OK = false
				r.Problems = append(r.Problems, err.Error())
				continue
			}
			r.Changed = true
		}
		r.PathFix = fixes
		if len(fixes) > 0 && !o.DryRun {
			r.Path = CheckPath(e)
		}
	}
	return r
}

func backup(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".bak."+time.Now().UTC().Format("20060102T150405Z"), raw, 0o644)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".relay-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// upsertBlock replaces the marked block in path, or adds it (at the top when
// top is set, else appended), leaving every other byte as it was.
func upsertBlock(path, block, begin, end string, top bool) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(raw)
	if b := strings.Index(s, begin); b >= 0 {
		if e := strings.Index(s[b:], end); e >= 0 {
			stop := b + e + len(end)
			if stop < len(s) && s[stop] == '\n' {
				stop++
			}
			out := s[:b] + block + s[stop:]
			if out == s {
				return nil
			}
			return writeAtomic(path, []byte(out), 0o644)
		}
		return fmt.Errorf("%s has %q without its end marker; fix it by hand", path, begin)
	}
	var buf bytes.Buffer
	if top {
		buf.WriteString(block)
		if s != "" {
			buf.WriteString("\n")
			buf.WriteString(s)
		}
	} else {
		buf.WriteString(s)
		if s != "" && !strings.HasSuffix(s, "\n") {
			buf.WriteString("\n")
		}
		if s != "" {
			buf.WriteString("\n")
		}
		buf.WriteString(block)
	}
	return writeAtomic(path, buf.Bytes(), 0o644)
}
