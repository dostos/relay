package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dostos/relay/internal/agentguide"
)

// Layered help (#76): `relay --help` is a short map; `relay CMD --help` and
// `relay help CMD SUB` print one command with flags and copy-paste
// examples; `relay help all` is the full reference.

type helpTopic struct {
	usage    string
	summary  string
	body     string
	examples []string
}

const overviewText = `relay — durable agent sessions over SSH + tmux, and safe messaging between them

Usage: relay [--json] <command> [args]      Every command takes --help (flags + examples).
Global: --json  machine-readable output (errors too: {"ok":false,"error",...,"argv"})

Messaging between agent sessions:
  session send      Paste a message into another session and prove it was submitted
  session wait      Wait until a session finished its turn (the reply)
  session readiness Is a session idle, busy, or at a human-only gate?
  session capture   Read a session's screen
Sessions:  session list|get|create|adopt|exec|op|attach|rename|destroy …   (relay session --help)
Fleet:     fleet status|upgrade|report     keep every host on the hub build
Agents:    agents install|status|doctor     relay guidance for Claude Code, ccs, Codex, cursor-agent
Usage:     report                          this machine's send/wait outcomes and findings
Hosts:     host …  container …  auth …  targets        Attach: resume …
Info:      version  capabilities  doctor

More: relay help messaging · relay help exit-codes · relay help all

Examples:
  relay --json session send c1/reviewer --idempotency-key r-42 -- "Review PR #42"
  relay --json session wait c1/reviewer --idle --timeout 100s --tail 60
  relay fleet status
`

const exitCodesText = `Exit codes (stable; automation may key on them)

  0    success (send: submitted or queued; wait: idle)
  1    error / refused before anything was typed (send: not_sent) — see JSON error, reason, argv
  2    usage error in a command group (missing subcommand); help is printed
  65   session wait: target stopped at an approval/login/trust gate only a human may answer
  66   session absent (resume --no-create; session wait: tmux session gone)
  67   send/submit: delivery unconfirmed (deposited or uncertain) — inspect, never resend blindly
  124  session wait: timed out; the JSON argv resumes the wait
  130  interrupted (Ctrl+C)          255  ssh failure (resume)
`

var helpTopics = map[string]helpTopic{
	"session": {
		usage:   "relay session <verb> ID …   (ID = sess-ID | HOST/NAME | unique NAME)",
		summary: "Durable tmux sessions on any host, and messaging between them.",
		body: `Messaging:  send  submit  wait  readiness  capture
Records:    list  get  create  adopt  rename  bridge  destroy  sensors
Commands:   exec  op status|wait|output  resize  attach (humans only)

ID forms: sess-… (registry id), HOST/NAME (tmux NAME on HOST, registered or not),
NAME (when unique; ambiguous → exit 1 with candidates), tmux:HOST:NAME (ad-hoc id).`,
		examples: []string{
			"relay --json session list --readiness",
			"relay --json session send c1/reviewer -- \"status?\"",
			"relay session send --help",
		},
	},
	"session send": {
		usage:   "relay [--json] session send ID [--idempotency-key KEY] [--force-append] (-- TEXT | --stdin)",
		summary: "Paste TEXT as one bracketed paste, press Enter once, and prove a turn started.",
		body: `Flags:
  --idempotency-key KEY  record the attempt on the target host; a retry with the same KEY
                         never types again and replays the first receipt ("replayed": true)
  --force-append         type even if the target's input already holds other text
  --stdin                read the message from stdin (up to 1 MiB; no shell quoting needed)
TEXT is every word after --, joined with spaces (quote it once).

Receipt states (--json: receipt.state), exit code:
  submitted 0  a turn started after this send
  queued    0  target was mid-turn; accepted and runs after the current turn
  not_sent  1  nothing typed: evidence composer_not_empty (receipt.leftover shows the draft,
               never cleared) | target_at_gate | send_lock_busy | error
  deposited 67 text is in the input but no turn started (relay session submit ID if intended)
  uncertain 67 unknown; inspect with relay session capture ID; never resend blindly
Bridge down (detached session): runs on this host; address targets as HOST/NAME.`,
		examples: []string{
			"relay --json session send c1/reviewer --idempotency-key review-42 -- \"Please review PR #42\"",
			"printf '%s\n' \"$MSG\" | relay --json session send worker --idempotency-key m-7 --stdin",
			"relay --json session send c1/reviewer --force-append -- \"(appending to my own draft)\"",
		},
	},
	"session submit": {
		usage:    "relay [--json] session submit ID [--idempotency-key KEY]",
		summary:  "Press Enter once on text already in the input (after a deposited receipt); same confirmation as send.",
		examples: []string{"relay --json session submit c1/reviewer"},
	},
	"session wait": {
		usage:   "relay [--json] session wait ID --idle [--timeout 5m] [--settle 3s] [--interval 1s] [--tail N]",
		summary: "Block (read-only) until the target has been idle for --settle: its reply is done.",
		body: `Outcomes (result.outcome) and exit codes:
  idle 0 · blocked 65 (gate only a human answers) · gone 66 · timeout 124 (argv resumes)
--tail N adds the last N pane lines (result.tail, max 500). Use --timeout below your own tool
timeout (e.g. 100s) and loop on 124.`,
		examples: []string{
			"relay --json session wait c1/reviewer --idle --timeout 100s --tail 60",
			"relay session wait worker --idle --settle 5s",
		},
	},
	"session readiness": {
		usage:    "relay session readiness ID [--lines N]",
		summary:  "Capture and classify one pane: state ready|blocked|absent|gone|unknown, activity busy|idle|blocked|unknown, gate details. Always JSON.",
		examples: []string{"relay session readiness c1/reviewer"},
	},
	"session capture": {
		usage:    "relay [--json] session capture ID [-n LINES]",
		summary:  "Print the last LINES (default 50) of the session's pane.",
		examples: []string{"relay session capture c1/reviewer -n 80", "relay --json session capture worker"},
	},
	"session list": {
		usage:    "relay [--json] session list [--readiness [--readiness-lines N]]",
		summary:  "List registered sessions; --readiness adds state/activity per session (bounded per host).",
		examples: []string{"relay --json session list", "relay session list --readiness"},
	},
	"session get": {
		usage:    "relay [--json] session get ID [--readiness]",
		summary:  "Show one session record.",
		examples: []string{"relay --json session get c1/reviewer --readiness"},
	},
	"session create": {
		usage:   "relay [--json] session create -H HOST [--repo DIR] [--cwd REMOTE] [--name NAME] [--container NAME …] [-- ARGV]",
		summary: "Create a durable tmux session on HOST and register it. Missing host profile → argv for relay host init.",
		examples: []string{
			"relay --json session create -H c1 --cwd ~/dev/app --name reviewer",
			"relay --json session create -H c1 --container dev --ephemeral -- claude",
		},
	},
	"session adopt": {
		usage:    "relay [--json] session adopt -H HOST --name TMUX [--cwd REMOTE] [--repo DIR]",
		summary:  "Register an already-running tmux session (idempotent).",
		examples: []string{"relay --json session adopt -H c1 --name reviewer"},
	},
	"session exec": {
		usage:   "relay [--json] session exec ID [--detach [--op-id OP]] -- CMD",
		summary: "Run CMD in the session's cwd (remote shell parses it). --detach starts a durable op that survives ssh loss; the same --op-id never runs twice.",
		examples: []string{
			"relay session exec c1/reviewer -- 'git status --short'",
			"relay session exec c1/reviewer --detach --op-id build-1 -- 'make test'",
		},
	},
	"session op": {
		usage:   "relay session op status|wait|output ID OP [--cursor N] [--timeout 60s] [--stream stdout|stderr] [--offset N] [--limit BYTES]",
		summary: "Query a detached op: status, block until its cursor moves, or read bounded output. Always JSON.",
		examples: []string{
			"relay session op wait c1/reviewer build-1 --cursor 1 --timeout 60s",
			"relay session op output c1/reviewer build-1 --offset 0 --limit 16384",
		},
	},
	"session destroy": {
		usage:    "relay session destroy ID [--keep-remote]",
		summary:  "Kill the tmux session (unless --keep-remote) and drop the record. Registered sessions only.",
		examples: []string{"relay session destroy c1/old-worker"},
	},
	"session rename": {
		usage:    "relay [--json] session rename ID NAME",
		summary:  "Rename the tmux session in place. Registered sessions only.",
		examples: []string{"relay --json session rename c1/worker reviewer"},
	},
	"session attach": {
		usage:    "relay session attach ID",
		summary:  "Interactive attach (humans only; agents use send/capture/wait).",
		examples: []string{"relay session attach c1/reviewer"},
	},
	"fleet": {
		usage:   "relay fleet status|upgrade|report [--hosts a,b] [--json]",
		summary: "Keep every known relay host (~/.local/state/relay/hosts/*.json) on the control host's build. Unreachable hosts are reported, never fatal; ssh fan-out is capped at 3 (--parallel 1-4).",
		examples: []string{
			"relay fleet status",
			"relay fleet upgrade --dry-run",
			"relay --json fleet upgrade --hosts c1,jetson",
			"relay --json fleet report --since 24h",
		},
	},
	"fleet status": {
		usage:    "relay [--json] fleet status [--hosts a,b] [--parallel N]",
		summary:  "Per host: reachable, os/arch, installed vs hub build, missing capabilities, relay on the ssh PATH, agent guidance state.",
		examples: []string{"relay --json fleet status", "relay fleet status --hosts c1,c3"},
	},
	"fleet upgrade": {
		usage:   "relay [--json] fleet upgrade [--hosts a,b] [--dry-run] [--force] [--skip-agents] [--parallel N]",
		summary: "Install the hub build on each host and refresh agent guidance. Idempotent: current hosts only get relay agents install.",
		body: `Per host: probe → cross-compile the hub commit for its GOOS/GOARCH (once per platform; refuses a
dirty hub build) → upload ~/.local/bin/.relay.new.fleet.<rand> → verify its build → atomic mv to
~/.local/bin/relay, or relay-real behind a text forwarding shim (previous kept as .previous) → restart active user services
(relay-event/relay/relay-control systemd units, com.dostos.relay-supervisor launchd agent, or a
nohup event service) → verify → relay agents install. The hub itself is skipped (./install.sh).
Entries that resolve to one machine (machine-id + hostname + user + HOME) are acted on once;
the rest report alias_of.
Exit 1 if any reachable host failed.`,
		examples: []string{"relay fleet upgrade --dry-run", "relay --json fleet upgrade", "relay fleet upgrade --hosts jetson --force"},
	},
	"fleet report": {
		usage:    "relay [--json] fleet report [--hosts a,b] [--since 24h]",
		summary:  "Run relay report on every host (including the hub) and merge totals and findings.",
		examples: []string{"relay --json fleet report --since 24h"},
	},
	"agents": {
		usage:   "relay agents install|status|doctor [--json]",
		summary: "Relay guidance for agent CLIs on THIS machine (never forwarded to the control host).",
		body: `Files (written only for agent CLIs found, unless --all):
  Claude Code + ccs  ~/.claude/skills/relay/SKILL.md   (ccs shares it; bare ccs profiles get a copy)
  Codex              ~/.codex/skills/relay/SKILL.md + managed block in ~/.codex/AGENTS.md
  cursor-agent       ~/.cursor/skills/relay/SKILL.md  (Cursor also reads ~/.claude/skills)
Relay-owned files carry a marker line; a file without it is never overwritten (--force backs it
up first). AGENTS.md and shell startup files only get a marked block; other content is kept.`,
		examples: []string{"relay agents install --dry-run", "relay --json agents install", "relay --json agents doctor"},
	},
	"agents install": {
		usage:    "relay [--json] agents install [--dry-run] [--all] [--force] [--no-path]",
		summary:  "Create/refresh guidance files and add ~/.local/bin to PATH where a login or non-interactive shell would miss relay. Safe to re-run (no-op when current).",
		examples: []string{"relay agents install --dry-run", "relay --json agents install", "relay agents install --all --no-path"},
	},
	"agents status": {
		usage:    "relay [--json] agents status",
		summary:  "Detected agent CLIs and the state of each guidance file (missing|current|stale|conflict|shared). Exit 1 when something is missing or stale.",
		examples: []string{"relay --json agents status"},
	},
	"agents doctor": {
		usage:    "relay [--json] agents doctor",
		summary:  "Per agent CLI: is relay on PATH in login and non-interactive shells (sshd-like minimal env), and does it see current guidance. Read-only.",
		examples: []string{"relay --json agents doctor"},
	},
	"report": {
		usage:   "relay [--json] report [--since 24h|90m|7d] [--examples N]",
		summary: "Aggregate this machine's usage log ($STATE/usage/*.jsonl; receipts only, never message text).",
		body: `Includes: events by command; send states, not_sent and unconfirmed by evidence, accept rate,
idempotency replays, direct (bridge-down) and bridged sends; lock contention, gate and composer
refusals; wait outcomes; op states on this host; stale send locks; top normalized errors and
failing targets; recent failures; findings (heuristic flags for the daily review).`,
		examples: []string{"relay --json report --since 24h", "relay report --since 7d --examples 20"},
	},
	"host":         {usage: "relay host <verb> -H HOST …", summary: "Host profiles, discovery and setup.", body: section("New machine", "Dev containers"), examples: []string{"relay host discover -H c1", "relay host init -H c1 --apply", "relay --json host caps -H c1"}},
	"container":    {usage: "relay container up|down|status|stop|start|auth sync -H HOST --container NAME …", summary: "Dev containers declared in the host profile.", body: section("Dev containers", "Host profiles"), examples: []string{"relay container up -H c1 --container dev", "relay container auth sync -H c1 --container dev --dry-run"}},
	"auth":         {usage: "relay auth status|login|url|copy …", summary: "Agent logins on hosts.", body: section("Agent auth", "Sessions"), examples: []string{"relay --json auth status -H c1", "relay auth login -H c1 --agent codex"}},
	"resume":       {usage: "relay resume --session NAME [--host HOST] [--no-create] | list [--probe] | reap [--dry-run] | prune", summary: "Re-attach a terminal (Forge tab) to a session; waits through ssh drops.", body: section("Attach", "Fleet"), examples: []string{"relay resume --session reviewer --host c1 --no-create", "relay --json resume list --probe", "relay resume reap --dry-run"}},
	"capabilities": {usage: "relay [--json] capabilities [-H HOST] [--refresh]", summary: "What the installed client (and HOST's event service) support — check before relying on a feature.", examples: []string{"relay --json capabilities", "relay --json capabilities -H c1"}},
	"version":      {usage: "relay [--json] version", summary: "Release version; --json adds build id, protocol and capability list.", examples: []string{"relay --json version"}},
	"doctor":       {usage: "relay doctor", summary: "Control-host health: bridge, home service, host event services and build drift.", examples: []string{"relay doctor"}},
	"targets":      {usage: "relay [--json] targets", summary: "Host aliases from ~/.ssh/config.", examples: []string{"relay --json targets"}},
	"client":       {usage: "relay client list|update|update-status", summary: "Restricted relayd clients.", examples: []string{"relay client list"}},
	"pane":         {usage: "relay pane classify [--lines N] < text", summary: "Classify pane text from stdin (readiness without a capture).", examples: []string{"tmux capture-pane -p | relay pane classify"}},
}

// section returns the full-reference lines from the heading starting with
// from up to the heading starting with to.
func section(from, to string) string {
	i := strings.Index(fullHelpText, "\n"+from)
	if i < 0 {
		return ""
	}
	rest := fullHelpText[i+1:]
	if j := strings.Index(rest, "\n"+to); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimRight(rest, "\n")
}

// helpPath is the non-flag words of argv (without help flags), i.e. the
// command path help was asked for.
func helpPath(args []string) []string {
	var out []string
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// helpFor prints the most specific topic for path and returns code.
func (a *App) helpFor(path []string, code int) int {
	if len(path) > 0 && path[0] == "sess" {
		path = append([]string{"session"}, path[1:]...)
	}
	switch strings.Join(path, " ") {
	case "":
		fmt.Print(overviewText)
		return code
	case "all":
		fmt.Print(fullHelpText)
		return code
	case "exit-codes", "exit":
		fmt.Print(exitCodesText)
		return code
	case "messaging", "agents guide":
		fmt.Print(agentguide.Guide())
		return code
	}
	for n := len(path); n > 0; n-- {
		key := strings.Join(path[:n], " ")
		if t, ok := helpTopics[key]; ok {
			printTopic(key, t)
			return code
		}
	}
	fmt.Printf("no help topic %q\n\n", strings.Join(path, " "))
	fmt.Print(overviewText)
	if code == 0 {
		return 2
	}
	return code
}

func printTopic(key string, t helpTopic) {
	fmt.Printf("Usage: %s\n\n%s\n", t.usage, t.summary)
	if t.body != "" {
		fmt.Printf("\n%s\n", t.body)
	}
	var subs []string
	for k := range helpTopics {
		if strings.HasPrefix(k, key+" ") && !strings.Contains(k[len(key)+1:], " ") {
			subs = append(subs, k)
		}
	}
	sort.Strings(subs)
	if len(subs) > 0 {
		fmt.Println("\nSubcommands (relay <subcommand> --help):")
		for _, k := range subs {
			fmt.Printf("  %-22s %s\n", k, firstSentence(helpTopics[k].summary))
		}
	}
	if len(t.examples) > 0 {
		fmt.Println("\nExamples:")
		for _, e := range t.examples {
			fmt.Println("  " + e)
		}
	}
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
