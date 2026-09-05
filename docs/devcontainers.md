# relay — dev containers on a fleet host

Bring a repo's own Dev Containers workspace up on a fleet host and hand agent
goals into it. The agent CLIs and their credentials live in **docker named
volumes**, so they survive container recreation and are shared by every
container on that host.

Status: implemented and verified live on hamburg, 2026-09-05 — `claude`,
`codex` and `cursor-agent` all run from the toolkit volume, and the full
open → work → destroy → auto-reap cycle was exercised end to end.

## Why named volumes

The earlier container design (`docs/superpowers/specs/2026-07-29-relay-container-handoff-design.md`)
offered two ways to get an agent into a container, and both have a durable cost:

- **Bind the host's agent closure.** Fails whenever the container's libc is
  older than the host's. Measured on hamburg: host `node`/`codex`/`cursor-agent`
  all die with `GLIBC_2.28 not found` inside an 18.04 image.
- **Install into the container.** Works, but it is repeated on every container,
  and every recreate throws the install away along with the agent's login.

A named volume is neither. It is populated **once, from inside a container**, so
its binaries are linked against the container's own libc by construction, and it
outlives the container that populated it. A second volume mounted as the agent's
`$HOME` does the same for credentials and config, so `claude` is logged in once
per host rather than once per container.

relay never binds the host's `~/.claude`: that drags host hooks, plugins and MCP
config into a container whose toolchain cannot run them.

## Declare it

```yaml
# ~/.config/relay/host.yaml (authoritative on the host)
containers:
  - name: oqb                       # relay handle → --container oqb
    devcontainer:
      workspace_folder: ~/gh/opaquebench   # HOST path holding .devcontainer/
      # config: ~/gh/opaquebench/.devcontainer/devcontainer.json   # optional override
      # id_label: relay.container=oqb                              # default
      gpu: none                     # all | detect | none
      # cli: /abs/path/to/devcontainer                             # if not on the login PATH
    toolkit:                        # named volume carrying the agent CLIs
      volume: relay-toolkit
      target: /relay/toolkit        # bin_dir defaults to <target>/bin
      provision: |
        npm i -g --prefix /relay/toolkit @anthropic-ai/claude-code @openai/codex
        HOME=/relay/toolkit bash -c "curl -fsS https://cursor.com/install | bash"
        ln -sf /relay/toolkit/.local/bin/cursor-agent /relay/toolkit/bin/cursor-agent
    home:                           # named volume that becomes $HOME for agent execs
      volume: relay-home
      target: /relay/home
    volumes:                        # optional extra named volumes, NAME:/path
      - relay-pip:/home/node/.cache/pip
    env: [ANTHROPIC_API_KEY]        # passed through by NAME; the value never enters relay
    path_map:
      - match: opaquebench
        remote_cwd: /workspaces/opaquebench
```

`user:` is optional. Left unset, relay reads the devcontainer CLI's own
`remoteUser` and execs as it — a devcontainer's `Config.User` is still `root`,
because the CLI applies `remoteUser` only to execs it runs itself.

## Use it

One command from your own machine — brings the container up, opens a tmux
session whose shell runs inside it, and puts that pane in front of you through
cmux:

```bash
relay container open -H hamburg --container oqb
```

The container is **ephemeral by default**: relay brought it up, so relay removes
it when the session is destroyed. A container nobody remembers starting is the
one still running a week later. `--keep` opts out for a long-lived shared
runner.

The rest of the surface:

```bash
relay container up -H hamburg --container oqb        # up + provision only (idempotent)
relay container up -H hamburg --container oqb --recreate --reprovision
relay container status -H hamburg --container oqb
relay container down -H hamburg --container oqb      # named volumes survive
relay session create -H hamburg --container oqb [--ephemeral]
relay agent start hamburg claude --container oqb -- "run the smoke suite"
```

## Lifetime

tmux runs **on the host**, not in the container, so the pane survives a
container recreate and cmux keeps its binding. What relay ties to the session is
the *container*, not the pane:

| Event | Container | Named volumes |
|---|---|---|
| `relay container open` | created if absent | created if absent, provisioned once |
| container recreated (`--recreate`) | replaced, new id | kept, provision skipped by its stamp |
| `relay session destroy` (ephemeral) | removed | **kept** |
| `relay session destroy` (`--keep`) | kept | kept |
| `relay container down` | removed | **kept** |

Teardown never removes the volumes. The toolkit and the agent's `$HOME` are
exactly the state meant to outlive any one container; remove them deliberately
with `docker volume rm` when you mean to.

`up` is idempotent: the toolkit install is guarded by a stamp file on the
volume, so a recreated container reuses the populated volume instead of
reinstalling. `--reprovision` forces it.

`down` removes the container only. The toolkit and the agent's `$HOME` are the
state worth keeping; remove them explicitly with `docker volume rm` if you mean
to.

## First login

Auth once per host, into the home volume:

```bash
relay session create -H hamburg --container oqb
relay session send <id> -- "claude /login"
```

The credential lands in the home volume and every later container mounting it is
already logged in.

## What relay does and does not do

relay **shells out to the host's `devcontainer` CLI** against the repo's own
`.devcontainer/devcontainer.json` and injects `--mount`, `--remote-env` and an
id label on top. Image builds, features and lifecycle commands stay the CLI's
job; relay does not reimplement the spec and does not build or patch images.

tmux still runs **on the host**, not in the container — a container recreate
leaves the pane intact. Only the pane's inner command and ad-hoc `exec` are
wrapped in `docker exec`.

## Traps this path already handles

| Trap | What relay does |
|---|---|
| The `devcontainer` CLI is a global npm install under nvm, so it is absent from the non-interactive PATH relay's ssh transport uses (`node -v` is empty under `bash -lc`, `v20.19.5` under `bash -ilc` on hamburg) | Runs the CLI under `bash -ilc`; `devcontainer.cli` takes an absolute path as an escape hatch |
| A named volume whose mount point is absent from the image is created `root:root 0755`, so a non-root agent cannot write to it | One guarded root exec chowns the relay volumes to the exec user at `up` |
| A devcontainer's `Config.User` is `root`, so every relay exec would run as root — which agents refuse and which writes root-owned files into the volumes | Resolves `remoteUser` from the `devcontainer.metadata` label |
| A container recreate changes the container id, so a declared `container:` name goes stale | Resolves the id from the relay id label at handoff time, not at declare time |
| A host path given as a volume source would silently become a bind mount that docker creates on the host | `volumes:` refuses anything that looks like a path; bind mounts belong in `expose:` |
| A repo with no host `path_map` entry would fail to resolve a working directory, even though for a container session the host cwd is only where the pane sits | Falls back to `~` on the host; the directory that matters is resolved inside the container from the spec |

## Requirements on the host

`docker`, and the `devcontainer` CLI (`npm i -g @devcontainers/cli`, needs node
≥ 18). relay reports the CLI's absence with that install line rather than
failing opaquely.

## Agent CLIs in the toolkit volume

Verified on hamburg, 2026-09-05, on `mcr.microsoft.com/devcontainers/javascript-node:20`
(Debian, glibc 2.41), all running as the non-root `remoteUser` with `$HOME` on
the home volume:

| Agent | Install | Version |
|---|---|---|
| `claude` | npm, honours `--prefix` | 2.1.197 |
| `codex` | npm, honours `--prefix` | 0.153.4 |
| `cursor-agent` | native installer, **hardcoded to `$HOME/.local`** | 2026.09.02 |

`cursor-agent` is the one that needs care: its installer takes no install-dir
override, so the provision command runs it with `HOME` pointed at the toolkit
volume and then symlinks the result into the volume's `bin_dir`. That keeps a
single PATH entry for all three. This is provision data in `host.yaml`, not
relay code — relay has no per-agent install logic.

The base `devcontainers/base:ubuntu-22.04` image has no node, so an npm-based
provision fails there with `npm: command not found`. Use a node-bearing image or
add the node feature.
