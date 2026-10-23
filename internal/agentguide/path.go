package agentguide

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PathCheck is whether one kind of shell finds `relay` with a minimal,
// sshd-like starting environment.
type PathCheck struct {
	Check string `json:"check"` // login_shell | noninteractive_bash | zsh_env
	Shell string `json:"shell"`
	OK    bool   `json:"ok"`
	Found string `json:"found,omitempty"`
	Error string `json:"error,omitempty"`
}

// PathFix is a PATH block planned or written into a shell startup file.
type PathFix struct {
	Check  string `json:"check"`
	File   string `json:"file"`
	Top    bool   `json:"top,omitempty"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}

// minimalPATH is roughly what sshd and a fresh login start from.
const minimalPATH = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func shellName(sh string) string { return filepath.Base(strings.TrimSpace(sh)) }

func runCheck(e Env, check, shell string, args ...string) PathCheck {
	pc := PathCheck{Check: check, Shell: shell}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Env = []string{"HOME=" + e.Home, "PATH=" + minimalPATH, "SHELL=" + shell, "TERM=dumb",
		"USER=" + os.Getenv("USER"), "LOGNAME=" + os.Getenv("LOGNAME"), "LANG=C.UTF-8"}
	cmd.Stdin = nil
	out, err := cmd.Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if err == nil && strings.HasPrefix(last, "/") {
		pc.OK, pc.Found = true, last
		return pc
	}
	if err != nil && ctx.Err() == nil {
		pc.Error = "relay not on PATH"
	} else if ctx.Err() != nil {
		pc.Error = "shell startup timed out"
	}
	return pc
}

// CheckPath runs the PATH probes that matter for agent CLIs (login shells)
// and for ssh commands (non-interactive shells).
func CheckPath(e Env) []PathCheck {
	sh := e.Shell
	if sh == "" {
		sh = "/bin/sh"
	}
	name := shellName(sh)
	if name == "fish" {
		return []PathCheck{{Check: "login_shell", Shell: sh, Error: "fish is not checked; add ~/.local/bin with fish_add_path"}}
	}
	checks := []PathCheck{runCheck(e, "login_shell", sh, "-lc", "command -v relay")}
	if name == "bash" {
		checks = append(checks, runCheck(e, "noninteractive_bash", sh, "-c", `[ -r "$HOME/.bashrc" ] && . "$HOME/.bashrc" >/dev/null 2>&1; command -v relay`))
	}
	if name == "zsh" {
		checks = append(checks, runCheck(e, "zsh_env", sh, "-c", "command -v relay"))
	}
	return checks
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func planPathFixes(e Env, checks []PathCheck) []PathFix {
	if !exists(filepath.Join(e.Home, ".local", "bin", "relay")) {
		return nil // the block would not help: relay is not in ~/.local/bin
	}
	var fixes []PathFix
	seen := map[string]bool{}
	add := func(check, file string, top bool) {
		if seen[file] {
			return
		}
		seen[file] = true
		fixes = append(fixes, PathFix{Check: check, File: file, Top: top, Action: "add_path_block"})
	}
	for _, c := range checks {
		if c.OK || c.Error == "" || strings.HasPrefix(c.Error, "fish") {
			continue
		}
		switch c.Check {
		case "login_shell":
			switch shellName(c.Shell) {
			case "zsh":
				add(c.Check, filepath.Join(e.Home, ".zprofile"), false)
			case "bash":
				switch {
				case exists(filepath.Join(e.Home, ".bash_profile")):
					add(c.Check, filepath.Join(e.Home, ".bash_profile"), false)
				case exists(filepath.Join(e.Home, ".bash_login")):
					add(c.Check, filepath.Join(e.Home, ".bash_login"), false)
				default:
					add(c.Check, filepath.Join(e.Home, ".profile"), false)
				}
			default:
				add(c.Check, filepath.Join(e.Home, ".profile"), false)
			}
		case "noninteractive_bash":
			// At the top: Debian/Ubuntu ~/.bashrc returns early for
			// non-interactive shells, before anything appended below.
			add(c.Check, filepath.Join(e.Home, ".bashrc"), true)
		case "zsh_env":
			add(c.Check, filepath.Join(e.Home, ".zshenv"), false)
		}
	}
	return fixes
}

// AgentDoctor is one agent CLI's view: can it run relay, and does it see
// the guidance.
type AgentDoctor struct {
	Agent         string   `json:"agent"`
	Binary        string   `json:"binary,omitempty"`
	Present       bool     `json:"present"`
	RelayOnPath   bool     `json:"relay_on_path"`
	SeesGuidance  bool     `json:"sees_guidance"`
	GuidancePaths []string `json:"guidance_paths,omitempty"`
	Problems      []string `json:"problems,omitempty"`
}

// DoctorResult is `relay agents doctor`.
type DoctorResult struct {
	OK       bool          `json:"ok"`
	Relay    string        `json:"relay_binary,omitempty"`
	Path     []PathCheck   `json:"path_checks"`
	Agents   []AgentDoctor `json:"agents"`
	Targets  []Target      `json:"targets"`
	Problems []string      `json:"problems,omitempty"`
	Next     []string      `json:"argv,omitempty"`
}

// discovery lists, per agent, the guidance files that agent reads (from the
// agents' documentation): Claude Code ~/.claude/skills; ccs profiles share
// ~/.claude/skills or have their own; Codex AGENTS.md + ~/.codex/skills;
// Cursor ~/.cursor/skills and, for compatibility, ~/.claude/skills and
// ~/.codex/skills.
var discovery = map[string][]string{
	"claude":       {"claude-skill"},
	"ccs":          {"claude-skill", "ccs-skill:"},
	"codex":        {"codex-agents-md", "codex-skill"},
	"cursor-agent": {"cursor-skill", "claude-skill", "codex-skill"},
}

// Doctor checks, without writing, that each present agent CLI can run relay
// and finds the guidance.
func Doctor(e Env) DoctorResult {
	d := DoctorResult{OK: true, Path: CheckPath(e), Targets: Targets(e)}
	if p := filepath.Join(e.Home, ".local", "bin", "relay"); exists(p) {
		d.Relay = p
	}
	pathOK := true
	for _, c := range d.Path {
		if !c.OK {
			pathOK = false
			d.Problems = append(d.Problems, c.Check+" ("+c.Shell+"): "+c.Error)
		}
	}
	if !pathOK {
		d.OK = false
	}
	byID := map[string]Target{}
	for _, t := range d.Targets {
		byID[t.ID] = t
	}
	for _, a := range Agents(e) {
		ad := AgentDoctor{Agent: a.Name, Binary: a.Binary, Present: a.Present, RelayOnPath: pathOK}
		if a.Present {
			for _, want := range discovery[a.Name] {
				for id, t := range byID {
					if id != want && !(strings.HasSuffix(want, ":") && strings.HasPrefix(id, want)) {
						continue
					}
					if t.State == "current" || (t.State == "shared" && byID["claude-skill"].State == "current") {
						ad.SeesGuidance = true
						ad.GuidancePaths = append(ad.GuidancePaths, t.Path)
					}
				}
			}
			if !ad.SeesGuidance {
				ad.Problems = append(ad.Problems, "no current relay guidance in any location "+a.Name+" reads")
			}
			if !ad.RelayOnPath {
				ad.Problems = append(ad.Problems, "relay is not on PATH in every shell "+a.Name+" may use")
			}
			if len(ad.Problems) > 0 {
				d.OK = false
				d.Problems = append(d.Problems, a.Name+": "+strings.Join(ad.Problems, "; "))
			}
		}
		d.Agents = append(d.Agents, ad)
	}
	if d.Relay == "" {
		d.OK = false
		d.Problems = append(d.Problems, "relay is not installed at ~/.local/bin/relay")
	}
	if !d.OK {
		d.Next = []string{"relay", "agents", "install"}
	}
	return d
}
