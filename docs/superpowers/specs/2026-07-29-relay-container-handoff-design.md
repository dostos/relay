# relay — container handoffs

Date: 2026-07-29
Status: Proposed

## Problem

A relay target is an SSH host alias; work runs in tmux **on the host**. But a lot of real fleet work lives in **docker containers on a host** (reached via `docker exec`), which relay has no concept of. We want to hand off an agent goal — and run ad-hoc `exec` — **into a container**, bringing the host's authed agent + credentials + necessary data in, and make it easy to declare per host.

Scope (confirmed with the user):

- Container shape: **`docker exec` into a container living on a fleet host** (not an SSH-reachable container — that is already just an SSH alias).
- Interaction: **handoffs and ad-hoc `exec`**.
- Runtime: **docker** only (podman = swap one verb later).
- Config home: **`containers:` in the remote `host.yaml`**, surfaced by `discover`.

## Evidence (hamburg, 2026-07-29)

The design is grounded in real tests on hamburg (Ubuntu 22.04, glibc 2.35, docker 24.0.2, no sudo). Long-lived targets exist: `condalab`, `jupyterlab2` on `ohsai/deepo` (**Ubuntu 18.04.3, glibc 2.27**). Host agent closures: `codex` → nvm node prefix `~/.nvm/versions/node/v20.19.5/`; `claude` → native `~/.local/share/claude/versions/2.1.220` (+ `~/.local/bin/claude` shim); `cursor-agent` → native `~/.local/share/cursor-agent/versions/.../cursor-agent`.

Binding the host toolchain into the 18.04 container (mirror paths, `-e HOME`, PATH prepend):

| Host agent | Kind | Result in glibc-2.27 container |
|---|---|---|
| `node` / `codex` | nvm/node (dynamic) | ❌ `libc.so.6: version 'GLIBC_2.28' not found (required by node)` |
| `cursor-agent` | native binary | ❌ `GLIBC_2.28 not found (required by cursor-agent)` |
| `claude` | native binary | ✅ `2.1.220 (Claude Code)` — runs |

Further findings:

- **Portability is per-binary, not per-install-method.** Both `cursor-agent` and `claude` are "native" installs; only `claude`'s binary runs under glibc 2.27.
- **The container's own node cannot rescue node agents.** `condalab` ships `node v12.19.0`; modern node CLIs need ≥18, and **node ≥18 requires glibc ≥2.28**, which an 18.04 image does not have — so codex/cursor-agent are *impossible* on an 18.04 image by any toolchain path. A newer base image (≥20.04) is the only fix.
- **UID matters.** The container runs as root; `claude` refuses `--dangerously-skip-permissions` as root. Running `--user 1000` gave `Permission denied` because the host file owner is **uid 1005** — bound files are only accessible when the exec uid matches the owner. `--user 1005` (non-root, host owner) cleared both.
- **Even a self-contained agent has an environmental tail.** With host `~/.claude` bound, `claude` ran and answered but its `SessionEnd` **hook** crashed (`SyntaxError: Unexpected token '.'`) — the hook shelled out to the container's node v12. Binding the whole `~/.claude` drags host hooks/plugins/MCP into the container, where they hit the container's toolchain.

Reproduction commands are in the Appendix.

## Decisions

1. **A container is a session/handoff attribute, not a new HostID or Transport.** The base transport stays plain ssh-to-host and **tmux stays on the host**; container-awareness is a `docker exec` wrapper applied only to (a) the tmux pane's inner command and (b) ad-hoc `exec`. Rationale: survives container 재생성 (the pane persists as a visible dead pane), containers stay thin (no tmux/relayd inside), and `capture` / `send` / idle+exit sensors / `attach` / `viz` / the relayd event bus keep working through the host tmux **unchanged**. The rejected alternative — a Transport decorator that wraps every `Run` in `docker exec` — forces tmux *inside* the container (dies on 재생성, needs tmux+relayd in every image).

2. **Declared in the remote `host.yaml` `containers:` list**, authoritative on the host, surfaced by `relay host discover` (via `docker ps`) — the same flow as agent detection.

3. **Runtime = docker only.** The exec verb lives in one function; podman is a later one-line addition.

4. **Credentials are always host-sourced data** (auth files are glibc-independent). **The toolchain has two co-equal strategies**, auto-selected by a capability probe — *neither is privileged*:
   - **Strategy A — host-bind:** bind the host agent's resolved closure into the container. Viable only when the host binary runs under the container's libc (e.g. `claude` on 18.04).
   - **Strategy B — in-container:** the agent runs from the container's **own** install (already in the image, or installed once by a declarative `provision:` command). Primary for node/glibc-2.28 agents and modern images.

5. **Verify is a hard gate.** Before a handoff starts, relay runs the agent's real bounded probe **inside the container as configured** and **refuses on failure**, mapping known stderr signatures to actionable guidance (table below). relay never launches an agent it has not confirmed can run.

6. **Exec as a non-root uid = the host file-owner uid by default** (`user:` overridable). Empirically required: matches bound-file ownership and clears agent root guards.

7. **Headless handoffs neutralize the agent's hooks/plugins/MCP** by default, so they do not shell out to the container's (possibly stale) toolchain.

8. **A planner (`relay container plan`)** resolves the closure on the host, probes the container (glibc, node, default user, agent presence), and emits either a ready-to-paste `expose:`/`user:` block **or** a precise "won't run because … → do X."

9. **Non-goal: relay never builds or patches images.** Provisioning is a **declarative per-agent `provision:` command** (data, not built-in per-agent install logic). When the only fix is a newer image, relay says so and stops.

## Host profile schema (`containers:`)

```yaml
# ~/.config/relay/host.yaml  (authoritative on the host)
containers:
  - name: beholder                 # relay handle → `--container beholder`
    runtime: docker                # docker (default)
    container: beholder-run        # docker ps name/id — target for exec-into-existing
    image: myrepo/beholder:latest  # optional — used by `relay container up` (relay-managed run)
    user: "1005"                   # exec uid[:gid]; default = host file-owner uid (non-root)
    default_cwd: /workspace        # fallback cwd when no path_map match
    path_map:                      # container-filesystem paths (own namespace)
      - match: beholder
        remote_cwd: /workspace/beholder
    toolchain: auto                # auto | bind | in-container
    expose:                        # host paths → container (docker -v syntax; default = mirror path)
      - ~/.claude.json             #   minimal auth only — NOT the whole ~/.claude (hooks/plugins break)
      - /data/beholder:/workspace/beholder
    env: [ANTHROPIC_API_KEY]       # optional env passthrough (-e)
    hooks: off                     # neutralize agent hooks/plugins for headless handoff (default: off)
    agents:                        # optional per-agent overrides (toolchain/provision/expose)
      codex:
        toolchain: in-container
        provision: "npm i -g @openai/codex@latest"   # run ONCE in-container if codex missing
    run:                           # optional extra args for `relay container up`
      gpus: "device=6,7"
```

Notes:
- When `toolchain: bind`, relay **computes the closure binds itself** (resolver, below); `expose:` is only for creds + data + extras.
- `expose:` entries default to mirror paths (`HOST` → same absolute path in-container); `HOST:CONTAINER` overrides, exactly like `docker -v`.
- Local read-through cache as today: `~/.local/state/relay/hosts/<host>.json` (remote wins).

## Resolve → Apply → Verify

**Resolve (host side).** For the chosen `--agent`, resolve its closure generically: `readlink -f "$(command -v <cmd>)"`, then classify — native self-contained (`~/.local/share/<agent>/versions/…` + `~/.local/bin` shim) vs node (walk up to the enclosing `bin/node` prefix). No per-agent table; the resolver is the only "smart" piece and it is a generic heuristic with a `toolchain:`/`provision:` escape hatch.

**Apply — Strategy A (bind).** `docker run … -v <closure>:…:ro -v <creds>:…:ro -v <data> … --user <uid>` (relay-managed), or `docker cp` the closure + creds into a pre-existing running container (idempotent, cached), then `docker exec -u <uid> -e PATH=… -e HOME=…`.

**Apply — Strategy B (in-container).** Nothing bound but creds + data; the agent is the container's own. If absent and `provision:` is declared, run it once in-container, then proceed.

**Auto-selection.** `toolchain: auto` decides by probe: if the container already has the agent (or `provision:` is declared) → prefer **B** (avoids the host-glibc gamble); else try **A**. Either way, **Verify** is the arbiter.

**Verify (hard gate).** Run the agent's real bounded invocation inside the container as configured (e.g. `<agent> --version` then a lightweight auth ping). Success → proceed to the handoff. Failure → abort and surface the captured stderr mapped to guidance:

| stderr signature | Guidance |
|---|---|
| `GLIBC_x.y not found` | Host toolchain too new for this container's libc. Use a self-contained agent that runs here, or a newer base image; node ≥18 needs glibc ≥2.28. |
| `… cannot be used with root/sudo …` | Set a non-root `user:` (default is the host owner uid). |
| `Permission denied` on the binary | `user:` uid must match the owner of the bound files (host uid N). |
| hook / plugin `SyntaxError` or node error | Set `hooks: off` (default) — the container's node is too old for the host's hooks/plugins. |
| `<agent>: command not found` | Strategy B and the agent is not in the image; declare a `provision:` command or bind (Strategy A). |
| `EROFS: read-only file system` under `$HOME` | The agent's state dir was bound read-only; bind only the auth credential ro and leave `$HOME/.claude` writable + container-local. |

## Commands

```text
relay agent start HOST A --container NAME [--repo R] -- "…"
                                        # handoff into a container (resolves → applies → verifies → starts)
relay session create -H HOST --container NAME [--repo R]      # ad-hoc container session
relay session exec|capture|send sess-…  # unchanged — container read from the session, wrapped automatically
relay host discover -H HOST [--probe-containers[=NAME]]
                                        # lists `docker ps` as containers: proposals;
                                        # --probe-containers reports glibc/node/user + per-agent verdict
relay container plan -H HOST --container NAME --agent A
                                        # planner: resolved expose:/user: block + run/won't-run verdict
relay container up -H HOST --container NAME
                                        # relay-managed `docker run` from image + expose + run
```

`--container` is supplied only at create/start; it is **persisted on the session**, so every downstream verb reads it. `discover` never writes; `plan` never writes (prints paste-ready config).

## Data model

- `HostProfile.Containers []ContainerSpec` — `{Name, Runtime, Container, Image, User, DefaultCWD, PathMap, Toolchain, Expose, Env, Hooks, Agents map[string]AgentOverride, Run}`.
- `Session.Container *ContainerRef` — `{Runtime, Ref, CWD, User, Toolchain}` (resolved at create; carried for all downstream ops).
- New: closure **resolver**, **verify** probe, and the `docker exec` **wrap** (one function: `-it` for the tmux pane, `-i` for ad-hoc; `-u`, `-e PATH/HOME`, `bash -lc 'cd <cwd>; exec <inner>'`).

## What does NOT change

`Persistence` (tmux) stays container-agnostic — the wrap is built in `SessionService.Create` / the agent-start path and passed as the session command. Sensors, `capture`, `send`, `attach`, `viz present`, `relayd` and the `agent wait → next` loop all operate on the host tmux and are untouched. `ReadFile`/`WriteFile` (host.yaml, relayd bootstrap) stay host-level.

## Security

- Prefer **read-only** binds; bind **minimal auth** (`~/.claude.json`) not the whole `~/.claude`. A whole-`~/.claude` **read-only** bind *breaks the agent*: claude writes runtime state to `~/.claude/session-env/` → `EROFS: read-only file system` (verified 2026-07-29, live `relay agent start --container`). Rule: bind the auth credential read-only; keep the agent's state dir (`$HOME/.claude`) **writable and container-local** (a fresh dir the agent owns). This is the same reason to avoid the whole-dir bind as the hook-crash finding — the dir carries host hooks/plugins too.
- `docker cp` of creds leaves them in the container FS until it dies — gated behind an explicit flag + warning; bind-mode keeps creds out of the writable FS.
- Never log credential contents. `host.yaml` references paths and env **names**, never secret values.

## Non-goals / follow-ups

- podman / apptainer runtimes (podman ≈ swap the exec verb).
- Image building/patching; `auth login` *into* a container; tmux-*inside*-container mode.
- `provision:` is run-once best-effort; relay does not manage in-image dependency lifecycles.

## Testing

- **Unit:** the `docker exec` wrap builder (tty/no-tty, cwd, uid/env, quoting + injection safety); `host.yaml` parse of `containers:`/`expose:`/`agents:`; closure resolver (native vs node); `docker ps` parse; verify-signature → guidance mapping.
- **Integration (gated, mirrors the hamburg evidence):** on a real host, `discover --probe-containers`; a bind-mode handoff of a self-contained agent into an old-glibc container (expect pass); a node agent into the same (expect a mapped `GLIBC` refusal, not a launch); an in-container-strategy handoff into a modern container; `user:` mismatch → mapped `Permission denied` refusal; `docker restart` of the container → pane persists.

## Appendix — evidence commands (hamburg, 2026-07-29)

`~` below is shorthand for the **absolute** host path mirrored to the identical absolute path in-container (docker `-v` does not expand `~`; the real runs used `/home/jingyulee/...` on both sides).

```bash
# Recon: docker, images, agent closure
ssh hamburg 'bash -ilc "docker ps --format {{.Names}}|{{.Image}}|{{.Status}};
  readlink -f \"\$(command -v claude)\"; readlink -f \"\$(command -v node)\""'

# Target glibc (read-only) + bind-mode acid test on the real image
docker exec condalab bash -lc 'ldd --version | head -1; grep PRETTY /etc/os-release'
docker run --rm \
  -v ~/.nvm/versions/node/v20.19.5:~/.nvm/versions/node/v20.19.5:ro \
  -v ~/.local/bin:~/.local/bin:ro -v ~/.local/share/claude:~/.local/share/claude:ro \
  -e HOME=$HOME ohsai/deepo bash -lc \
  'export PATH=~/.local/bin:~/.nvm/versions/node/v20.19.5/bin:$PATH; node -v; codex --version; claude --version'
#   → node/codex: GLIBC_2.28 not found ;  claude: 2.1.220 (works)

# claude end-to-end, non-root host uid, minimal auth bound
docker run --rm --user 1005:1005 \
  -v ~/.local/bin:~/.local/bin:ro -v ~/.local/share/claude:~/.local/share/claude:ro \
  -v ~/.claude.json:~/.claude.json:ro -e HOME=$HOME ohsai/deepo bash -lc \
  'export PATH=~/.local/bin:$PATH; claude -p PONG --model haiku'
#   → runs & answers; whole-~/.claude bind additionally breaks SessionEnd hook under container node v12
```
