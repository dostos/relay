# Fleet rollout, usage report and agent CLIs

Relay is one binary. The control host (the Mac hub) builds it with
`./install.sh`; `relay fleet` puts **that exact build** on every other relay
host, and `relay agents` makes it discoverable to the agent CLIs there.

## `relay fleet` (control host)

Hosts are the profiles the hub has cached: `~/.local/state/relay/hosts/*.json`
(`--hosts a,b` narrows it). ssh fan-out is capped at 3 (`--parallel 1-4`;
the campus IPS flags more than 4 concurrent ssh from one source). An
unreachable host is reported, never fatal.

```console
relay fleet status [--hosts a,b] [--json]
relay fleet upgrade [--hosts a,b] [--dry-run] [--force] [--skip-agents] [--json]
relay fleet report [--hosts a,b] [--since 24h] [--json]
```

**status** probes each host with one ssh command: `uname -s/-m`, the build of
`~/.local/bin/relay`, its capability list (diffed against the hub:
`missing_capabilities`), `command -v relay` in a non-interactive ssh shell
(`relay_on_ssh_path`; empty means agents and scripts that run `relay` over
ssh will not find it), and `relay agents status` (`agents_ok`). Summary:
`up_to_date | behind | unreachable | agents_problems`. `argv` follows the
global `--json` convention (the suggested next command, not this command's
argv): it is present only when hosts are behind and is
`relay fleet upgrade --hosts <the behind hosts>`, with `next` saying why.

Host entries that resolve to the same machine (same `/etc/machine-id` or
macOS platform UUID, hostname, user and `$HOME` — e.g. an ssh alias through a
tunnel, or the hub as both `local` and its own name) are acted on once; the
others are reported with `alias_of` and the primary's state, so two entries
never race on the same files.

If `~/.local/bin/relay` is a text forwarding shim and `~/.local/bin/relay-real`
exists (e.g. a shim that `docker exec`s into a relay container and falls back
to relay-real), fleet probes and installs **relay-real** and leaves the shim
untouched (`shim: true`, `install_target`).

**upgrade**, per host:

1. probe (as status). Up to date and no `--force` → only step 6.
2. cross-compile the hub's commit for the host's GOOS/GOARCH
   (linux/amd64, linux/arm64 incl. Jetson, linux/arm, darwin/*; WSL is linux)
   once per platform, `CGO_ENABLED=0`, stamped with the hub build. A dirty or
   unstamped hub build is refused: reinstall the hub from a clean checkout.
3. upload to a per-run `~/.local/bin/.relay.new.fleet.<random>`; verify its
   `relay build` (a failure reports the staged binary's stderr).
4. keep the old binary as `relay.previous`; atomic `mv` to
   `~/.local/bin/relay` (or `relay-real` behind a shim); `relayd` symlink.
5. restart what is running: active user systemd units `relay-event`,
   `relay`, `relay-control`; on macOS the loaded `com.dostos.relay-supervisor`
   / `com.dostos.relay-system` launchd agents (`kickstart -k`); otherwise a
   running `nohup relay service event run`. Verify the installed build.
6. `relay agents install` on the host (`--skip-agents` skips).

The hub itself is never overwritten (that is `./install.sh`); it only gets
step 6. `--dry-run` prints `would_upgrade OLD → NEW (platform)` per host.
Exit 1 if any reachable host failed.

Rollback on a host: `mv ~/.local/bin/relay.previous ~/.local/bin/relay`.

## `relay report` (any machine)

Every `session send/submit/wait` and `op wait` appends one JSON line to
`$STATE/usage/YYYY-MM-DD.jsonl` on the machine that executed it (with the
desktop bridge that is the hub; detached/direct sends are logged on the
host). Lines carry receipts, evidence, exit codes, errors, durations and
build ids — never message text. `RELAY_USAGE_LOG=0` disables logging.

```console
relay --json report [--since 24h|90m|7d] [--examples N]
```

Fields: `events`, `by_cmd`; `send` (`by_state`, `not_sent_by_evidence`,
`unconfirmed_by_evidence`, `accept_rate`, `keyed`, `replays`, `direct`,
`bridged`); `lock_contention` (send_lock_busy), `gate_refusals`
(target_at_gate), `composer_refusals` (composer_not_empty); `wait_outcomes`;
`op_wait_states`; `ops_on_host` (from `~/.local/state/relay/ops`);
`stale_send_locks`; `errors` (normalized, top 10); `failing_targets`;
`builds`; `recent_failures`; `findings` (heuristic flags: ≥10 % unconfirmed
sends, lock contention, gate/composer refusals, ≥30 % wait timeouts, bridge-down
direct sends, stale locks, mixed builds, malformed log lines).

`relay --json fleet report --since 24h` runs it on every host and adds
`totals` and host-prefixed `findings`.

### Daily review loop

```sh
cd ~/dev/relay && git pull --ff-only && ./install.sh
relay --json fleet status
relay --json fleet report --since 24h     # look at findings, errors, recent_failures
# fix → merge → reinstall hub → relay fleet upgrade
```

## `relay agents` (any machine; never forwarded to the hub)

```console
relay agents status  [--json]
relay agents install [--dry-run] [--all] [--force] [--no-path] [--json]
relay agents doctor  [--json]
```

Guidance is one document (`relay help messaging`; source
`internal/agentguide/relay.md`) installed where each CLI looks:

| agent CLI | file | why |
|-----------|------|-----|
| Claude Code | `~/.claude/skills/relay/SKILL.md` | user skills |
| ccs | same file via `~/.ccs/shared/skills → ~/.claude/skills`; bare / profile-local instances (`~/.ccs/instances/*/skills` as a real dir) get their own copy | ccs runs Claude Code with `CLAUDE_CONFIG_DIR=~/.ccs/instances/<profile>` |
| Codex | `$CODEX_HOME/skills/relay/SKILL.md` + a marked block in `$CODEX_HOME/AGENTS.md` (always loaded) | Codex global instructions + skills |
| cursor-agent | `~/.cursor/skills/relay/SKILL.md` | Cursor user skills (it also reads `~/.claude/skills`, `~/.codex/skills`); the CLI has no global rules file |

Files are written only for CLIs that are present (binary on PATH or in a
usual per-user bin dir, or the config dir exists); `--all` writes all.
Ownership rules: relay-owned files carry a marker line and are refreshed when
the guidance changes; a file without the marker is never touched (`--force`
backs it up to `*.bak.<time>` and replaces it). `AGENTS.md` and shell startup
files only get a block between `relay managed` markers; every other byte is
kept, and re-running changes nothing.

**PATH.** `install` (unless `--no-path`) checks, from an sshd-like minimal
environment, whether `relay` resolves in the user's login shell
(`$SHELL -lc`), in a non-interactive bash (sourcing `~/.bashrc` the way sshd's
bash does), and in zsh (`~/.zshenv`). For each that fails, and only if relay
is in `~/.local/bin`, it adds a marked PATH block: `~/.bash_profile` /
`~/.bash_login` / `~/.profile` (bash login), `~/.zprofile` (zsh login),
**the top of** `~/.bashrc` (Debian-style bashrc returns early for
non-interactive shells), `~/.zshenv`.

**doctor** reports, per present agent CLI, `relay_on_path` and
`sees_guidance` (a current file in a location that CLI reads), with problems
and `argv: relay agents install`. Read-only.
