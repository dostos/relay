# Host Ensure Account Agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `relay host ensure -H HOST [--apply] [--json]` that probes deps, proposes additive `ccs:*` / `codex:*` agents from live lists, optionally merges into remote `host.yaml`, and prints live auth-help — without storing runtime facts.

**Architecture:** New `EnsureService` in `internal/core` reuses `listCCSProfiles`, `listCodexMultiAuthAccounts`, `probeOneAgent`, `LoginCommand`, and profile fetch/write. CLI wires under `relay host ensure`. No new config files.

**Tech Stack:** Go, existing `ports.Transport`, yaml.v3, fake/`matchTransport` tests.

## Global Constraints

- Never write remaining %, pin, or “current” identity into yaml.
- Never call `codex-multi-auth switch`.
- Additive merge only; skip existing agent names; no rename/delete in v1.
- No auto-install of tools; missing deps → `ok:false` + hint.
- `--apply` requires existing remote `host.yaml` (else point to `host init`).
- Selectors: CCS profile name; Codex email-from-label else 1-based index.

---

### Task 1: Propose + merge helpers (TDD)

**Files:**
- Create: `internal/core/ensure.go` (helpers + types)
- Create: `internal/core/ensure_test.go`

**Interfaces:**
- Produces: `proposedAccountAgents(ctx, t, existing []AgentSpec) (proposed, skipped []AgentSpec)`
- Produces: `mergeAccountAgents(p *HostProfile, proposed []AgentSpec) (merged *HostProfile, added int)`

- [ ] **Step 1:** Failing tests — stubbed lists → proposed `ccs:hcs`, `codex:a@example.com` with `--account`; existing name skipped; merge preserves path_map/defaults.
- [ ] **Step 2:** Implement helpers (no CLI yet).
- [ ] **Step 3:** `go test ./internal/core/ -run Ensure -count=1`
- [ ] **Step 4:** Commit `[relay] feat: propose and merge account agents for ensure`

---

### Task 2: EnsureService end-to-end

**Files:**
- Modify: `internal/core/ensure.go`
- Modify: `internal/core/ensure_test.go`

**Interfaces:**
- Produces: `EnsureService.Ensure(ctx, hostID, EnsureOptions) (*EnsureResult, error)`
- `EnsureOptions{Apply bool}`
- `EnsureResult` with `ok`, `deps`, `proposed_agents`, `skipped_agents`, `applied`, `wrote_profile`, `auth`, `next`, `argv`, `detail`

- [ ] **Step 1:** Failing tests — missing `codex-multi-auth-codex` with accounts → `ok:false`; dry-run no WriteFile; `--apply` writes merged yaml and sets `wrote_profile`.
- [ ] **Step 2:** Implement deps probe (`ccs`, `codex-multi-auth`, `codex-multi-auth-codex`, rotation status substring `enabled`), auth rows for proposed+existing account agents, apply via `WriteFile` + `Profiles.Fetch`.
- [ ] **Step 3:** `go test ./internal/core/ -count=1`
- [ ] **Step 4:** Commit `[relay] feat: EnsureService deps normalize auth-help`

---

### Task 3: CLI + help

**Files:**
- Modify: `internal/cli/app.go` (`host` switch + usage text)
- Optional: README one-liner under host onboarding

- [ ] **Step 1:** Wire `case "ensure"` parsing `--apply`, call service, text + JSON output, exit 1 if `!ok`.
- [ ] **Step 2:** Manual: `relay host ensure -H local` or a reachable host dry-run.
- [ ] **Step 3:** Commit `[relay] feat: host ensure CLI`
- [ ] **Step 4:** Push branch + open PR covering launch-agent-accounts + ensure.
