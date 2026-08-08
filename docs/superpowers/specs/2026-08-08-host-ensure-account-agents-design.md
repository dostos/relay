# Host ensure — deps check + account-agent normalize

Date: 2026-08-08
Status: Approved for planning
Repos: `relay` (`host ensure`); consumers: Command Center launch (later thin caller), operators

## Problem

Account-aware launch (`ccs:*`, `codex:*` in `host.yaml`) only works when each host has:

1. The right CLIs on login PATH (`ccs`, `codex-multi-auth`, `codex-multi-auth-codex`)
2. Stable account agents merged into that host’s authoritative `host.yaml`
3. A clear next step when an account is present but unauthenticated

Today those steps are manual or scattered (`host discover` / `init`, hand-edited yaml, `auth status` / `login`). Operators need one repeatable verb: **ensure this host is ready for account-agent launch**.

## Constraints

- **Declare vs discover:** `host.yaml` holds declared launch policy (agent name → command/args). Runtime facts (logged-in?, remaining %, pinned index, active account) are **never** written to yaml or a second manifest — always probed live.
- **No duplicate config:** Do not invent `auth-accounts.yaml`, fleet-wide account registries, or ACC-side copies of host account lists. Live `ccs auth list` / `codex-multi-auth list --json` remain the source for what accounts exist; yaml only stores the launch mapping once the operator applies a proposal.
- **Never** call `codex-multi-auth switch` from ensure (or launch).
- Codex per-session force stays `codex-multi-auth-codex --account <selector>`; `--account` requires the rotation proxy on that host (probe/report; do not silently fall back).
- Remote `~/.config/relay/host.yaml` is authoritative; local cache is read-through after apply.
- v1 does **not** auto-install packages (brew/npm); missing deps fail loud with install hints.

## Decision

Add **`relay host ensure -H HOST [--apply] [--json]`** — a thin orchestration command that reuses discover/auth list helpers:

1. **Deps** — presence (and Codex rotation enabled) probes  
2. **Normalize** — propose additive `ccs:*` / `codex:*` agents from live lists  
3. **Auth help** — live status rows + `LoginCommand` strings  

`--apply` merges the proposal into remote `host.yaml` (skip existing names). Without `--apply`, print proposal/diff only (same spirit as `host discover` / `init` dry-run).

`host discover` / `init` remain the new-machine inventory/bootstrap path. `ensure` is the repeatable “account-launch ready” path.

## Architecture

```
relay host ensure -H HOST [--apply]
        │
        ├─► deps probe (login shell: command -v, rotation status)
        │         missing → ok:false + hint (no write)
        │
        ├─► live account lists (ccs / codex-multi-auth)
        │         → proposed AgentSpecs (stable selectors)
        │         → [--apply] merge into remote host.yaml
        │
        └─► auth help rows (present/authed + login argv text)
                  runtime only — never persisted as “current”
```

**Ownership**

| Layer | Owns |
|---|---|
| Relay `host ensure` | Deps probe, proposal, optional yaml merge, auth-help strings |
| Remote `host.yaml` | Declared account-agent launch policy after apply |
| Live CLIs | Runtime account inventory + auth state |
| Command Center | Later: optional button calling ensure; not required for v1 |

## Normalize rules

Proposed agents (additive; skip if `name` already in profile):

| Source | Name | Command / args |
|---|---|---|
| CCS profile | `ccs:<profile>` | `ccs <profile>` |
| multi-auth account | `codex:<selector>` | `codex-multi-auth-codex` + `args: [--account, <selector>]`, `usage_key: codex` |

**Selector preference (Codex):** email extracted from list label when present; else 1-based index string. Do not invent short aliases (`personal-gmail`) in ensure — operators may keep hand-chosen names; ensure never deletes or renames existing entries (v1).

Bare `claude` / `codex` / `cursor-agent` are left alone (discover/init territory). Ensure focuses on account-scoped agents.

## CLI contract

```bash
relay host ensure -H HOST           # probe + propose + auth help (no writes)
relay host ensure -H HOST --apply   # merge additive agents into remote host.yaml
relay host ensure -H HOST --json    # machine-readable EnsureResult
```

**EnsureResult (JSON shape, conceptual):**

- `ok`, `host_id`
- `deps[]`: `{ name, present, detail, hint? }` — includes rotation when relevant
- `proposed_agents[]`: `AgentSpec` not yet in profile
- `skipped_agents[]`: names already present
- `applied`: bool; `wrote_profile` when apply succeeded
- `auth[]`: `{ agent, present, authed, detail, login }` from live probes
- `next` / `argv`: suggested follow-up (e.g. `relay auth login -H HOST --agent …`)

Exit non-zero when unreachable, or when required deps for a present account stack are missing (e.g. multi-auth accounts exist but wrapper missing). Soft: unauthed accounts do not fail `ok` if deps+yaml are fine — they surface in `auth[]` with login commands.

## Error handling

- SSH/transport failure → `ok:false`, reach detail, no apply.
- `--apply` with missing remote profile → refuse; point to `relay host init -H HOST --apply`.
- Partial list parse → propose what parsed; note degraded in detail (do not invent accounts).
- Apply merge is additive YAML rewrite of `agents:` only (preserve other keys: path_map, defaults, containers).

## Testing

- Unit: parse CCS / multi-auth → proposed specs (email vs index); merge skip-if-exists; never emit `switch`.
- Unit: deps probe missing → `ok:false` + hint; rotation disabled noted for Codex path.
- Fake transport: ensure dry-run vs `--apply` writes expected agents only.
- No fixture that stores remaining%/pin into yaml.

## Out of scope (v1)

- Auto-install of `ccs` / `codex-multi-auth`
- Renaming `codex:1` → `codex:email` for existing entries
- Credential copy between hosts
- Command Center UI (may call ensure later)
- Storing usage/remaining or pin state anywhere in Relay config

## Relation to launch-agent account selection

Launch selection (ACC Account dropdown + local/remote mapping) consumes host.yaml agents and live lists. Ensure is how remotes get those agents without hand-editing — complementary, not a second account store.
