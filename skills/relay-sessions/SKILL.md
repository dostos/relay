---
name: relay-sessions
description: Use when working with relay remote sessions — host profiles, create/list sessions, exec/capture/send/resize without interactive attach (SSH+tmux defaults; cmux optional viz).
---

# relay Sessions

Repo: `~/dev/relay`. Binary: `relay` (install via `~/dev/relay/install.sh`).

Adapters (defaults, not required forever): **Transport=SSH**, **Persistence=tmux**, **Viz=cmux**.

## Host profile first

Each remote owns `~/.config/relay/host.yaml` (agents + path_map). Local only caches.

```bash
relay host example -H HOST          # template
relay host fetch -H HOST            # pull + cache
relay host probe -H HOST            # present/authed checks
```

No profile → clear error. Do not invent `~/dev→gh` path mappings.

## Agent workflow (never attach)

```bash
cd <git-repo>
relay session create -H HOST --json     # or use existing id from list
relay session list --json
relay session capture sess-… -n 100
relay session send sess-… -- "make test"
relay session exec sess-… -- "uname -a"
relay session resize sess-…             # garbled TUI / pty desync
```

**Do not** run `relay session attach` from an agent (interactive SSH).

## Viz (optional)

```bash
relay viz present sess-…              # cmux pane bound to session id
relay viz layout
relay viz close sess-…                # never guess panes by title
```

If cmux is down, sessions/handoffs still work.

## Mistakes

| Mistake | Fix |
|---------|-----|
| Guess host/session from ambient state | Use explicit session id from `list` / create |
| Interactive attach in agent | capture / send / exec only |
| Close panes by role heuristic | `viz close <session_id>` only |
| Path convention without profile | configure remote `path_map` |
