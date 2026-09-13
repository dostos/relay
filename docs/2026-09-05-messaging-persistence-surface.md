# relay — the kept surface: session, message, surface

> **Retired 2026-09-13.** The delegation verbs this document describes (`agent`, `handoff`, `parent`, `msg`, `board`, `ask`/`resolve`, `policy`, `root`, `events`, `gc`, `history`, `mcp`) were removed as one unit; see the workspace's one-owner-per-axis decision (`dostos-workspace/docs/superpowers/specs/2026-09-13-agent-infra-one-owner-per-axis.md`). Kept for the record.

Date: 2026-09-05
Scope: a map of what CLI verbs and code paths exist today, so the kept surface
can be consolidated around three nouns: **session** (tmux, attach, detach),
**message**, **surface** (viz). This is not the retirement plan — that is
`docs/2026-09-05-retire-hierarchy-plan.md`, which tracks what has been deleted
and why. This document only describes what remains and how it fits together.

This is a snapshot of a fast-moving target. HEAD at write time is `b23e2bd`.
The retirement plan records three of four planned passes as landed
(`fa9b02b`, `4ed068a`, `d25a08c`); the fourth ("apex", `root.go` and
`ancestor.go`) is explicitly not started. Every claim below is cited
`file:line` against that commit. If you read this later, re-check the cited
lines before trusting them.

## 1. Every CLI verb, counted

Source: `internal/cli/app.go`. Top-level dispatch is `Run()` at
`internal/cli/app.go:437-497`; each entry below is one `func (a *App) cmdX`
and its `case` arms (`grep -n '^\tcase "'` inside each function's line
range). A function with 0 case arms is a single verb whose behavior is
selected by flags, not by a sub-verb switch.

### Session / persistence — 22 verbs across 4 command groups

| Group | Verb | Cite |
|---|---|---|
| `session`/`sess` | list | app.go:1248 |
| | get | app.go:1257 |
| | rename | app.go:1266 |
| | bridge | app.go:1292 |
| | create | app.go:1301 |
| | adopt | app.go:1355 |
| | capture | app.go:1391 |
| | send | app.go:1421 |
| | exec | app.go:1434 |
| | resize | app.go:1455 |
| | attach | app.go:1463 |
| | destroy | app.go:1471 |
| | cleanup | app.go:1497 |
| | sensors | app.go:1509 |
| `resume` (bare, no subverb) | attach-or-reconnect | app.go:3644 |
| `resume` | list | app.go:3645 |
| `resume` | reap | app.go:3668 |
| `resume` | prune | app.go:3698 |
| `history` | (single verb, no sub-switch) | app.go:1227 |
| `gc` | (single verb, flags `--dry-run`/`--channel-ttl`) | app.go:1773 |
| `doctor` | (single verb, no sub-switch) | app.go:3914 |
| `install-cmux-restore` | (single verb) | app.go:3901 |

`session destroy` no longer refuses on hierarchy grounds. Before commit
`d25a08c` it refused a local parent (pointing callers at `relay parent
retire`, itself since deleted) and refused a session with live children. Both
refusals are gone; `Destroy` (internal/core/session.go:653-681) now only does
transport teardown (`Persist.Destroy`), bridge-identity clearing, and
resume-registry bookkeeping (`RememberResume` on `--keep-remote`, otherwise
none — teardown is treated as intentional and not resumable).

### Messaging — 30 verbs across 6 command groups

| Group | Verb | Cite |
|---|---|---|
| `msg` | send | app.go:1822 |
| | rm | app.go:1874 |
| | read | app.go:1897 |
| | wait | app.go:1935 |
| `parent` | send | app.go:2004 |
| | heartbeat | app.go:2038 |
| | register (incl. `--headless`) | app.go:2052 |
| | bind | app.go:2128 |
| | list | app.go:2149 |
| | inbox | app.go:2179 |
| | redeliver | app.go:2206 |
| | log | app.go:2219 |
| | sweep | app.go:2264 |
| | reply | app.go:2277 |
| | ack | app.go:2292 |
| | state \| active \| idle \| complete | app.go:2310 |
| | watch | app.go:2326 |
| `resolve` | (single verb, sugar) | app.go:2335 |
| `policy` | list | app.go:2458 |
| | remove \| rm | app.go:2467 |
| | add | app.go:2475 |
| | check | app.go:2532 |
| `signal`/`hook` (mode arg, flag-selected kind) | (single dispatch fn) | app.go:2934 |
| `ask` | (single verb, sugar) | app.go:3039 |
| `board` | post | app.go:2809 |
| | query | app.go:2818 |
| | watch | app.go:2831 |
| `events` | emit | app.go:3064 |
| | tail | app.go:3090 |
| `log` | (single verb — communication log dump) | app.go:2354 |

`parent`'s 13 case arms match the retirement-plan commit message
(`d25a08c`) exactly: send, heartbeat, register, bind, list, inbox, redeliver,
log, sweep, reply, ack, state-group, watch. `link`, `adopt`, `move`,
`reparent`, `status`, `retire` are gone — confirmed by `grep -c` returning 0
for those strings inside the current `cmdParent` body (app.go:1997-2331).

### Viz / surface — 10 verbs

| Verb | Cite |
|---|---|
| retire-control | app.go:3452 |
| list | app.go:3463 |
| rename | app.go:3476 |
| layout | app.go:3505 |
| present | app.go:3512 |
| brand | app.go:3561 |
| focus | app.go:3570 |
| close | app.go:3583 |
| save | app.go:3595 |
| restore | app.go:3601 |

Dispatched as both `viz` and `pane` at the top level (app.go:280, 480) — two
names for one command group, not two surfaces.

### Host / profile — 8 verbs

`host`: example, show\|fetch (fetch is a documented alias, app.go:891),
cache, probe, bootstrap, discover, init, ensure. Cites: app.go:885-1021.

### Container — 4 verbs + `session create --container`

`container`: open, up, down, status (app.go:4328-4334). `container open`'s
body is factored into `cmdContainerOpen` (app.go:4371), reused by both
`container open` and (per the retirement plan) named as "the launch
primitive" for a future presenter. `session create` also takes `--container`
(app.go:1316) — a second entry point into the same container-exec wrapping
path in `session.go`'s `Create`.

### Handoff, agent, auth, client, root, supervise — the rest

- `handoff`: list, get, finalize, reconcile (app.go:1582-1636), plus bare
  create via flags. 4 named + 1 implicit.
- `agent`: protocol\|help, pick, start, restart, wait, send, capture, done,
  status — 9 verbs (app.go:3135-3414).
- `auth`: status, login, url, copy, darwin, linux — 6 case arms
  (app.go:744-870), function body app.go:738-877 (`example` at app.go:885
  belongs to `cmdHost`, not `cmdAuth` — corrected from an earlier
  miscount).
- `client`: list, update, update-status\|status — 3 verbs (app.go:512-544).
- `root`: adopt, release, enroll\|unenroll, status, rules, digest — 6 verbs
  (app.go:2647-2722). This is the apex-governance surface named as the
  fourth, not-yet-started retirement pass in the retirement plan. It is
  entirely hierarchy-built (see section 2) and out of scope for the
  session/message/surface consolidation as it stands today.
- `supervise`: single verb, `--check` flag only (app.go:2868).
- `targets`, `build`, `version`, `help`: static/info verbs, no sub-switch.

### Total

Counting every distinct `case` string plus every flag-only single-verb
command function: **roughly 22 session/persistence, 30 messaging, 10
viz/surface, 8 host/profile, 4+1 container, and ~25 "other"** (handoff 5,
agent 9, auth 6, client 3, root 6, supervise 1, targets/build/version/help
~4 static). Total distinct CLI-reachable behaviors: **on the order of 100**.
This is a verb count, not a design recommendation — many of these are
one-line aliases or thin wrappers (see section 5).

## 2. Messaging: the real delivery path

**Answer first:** a relayd event becomes a human-readable message by walking
one function chain — `routeChildEvent` classifies and filters it, resolves
exactly one candidate mailbox (the handoff's immediate manager session, no
ancestor walk), deduplicates against anything already pending, applies a
YAML/builtin policy that may auto-resolve it, and delivers it once (with one
same-target retry) into that manager's pane or its durable inbox file. There
is no failover to a different session anymore; an unreachable manager just
leaves the envelope pending for the manager who owns it.

Call chain, current state:

1. `ParentService.RouteChildEvent` / `RouteLaunchFailure` (internal/core/parent.go:1350-1358) both forward into `routeChildEvent(ctx, ho, ev, allowTerminal)` (parent.go:1360).
2. `routeChildEvent` classifies the event (`classifySecurityEvent`), filters with `eventWakesManager`/`attentionKind`, and on a wake calls `deliveryCandidates(ho)` (parent.go:1307-1315).
3. `deliveryCandidates` **(rewritten in pass 2, `4ed068a`)**: if `ho.SourceSessionID == ""` returns `nil`; otherwise looks up that one session via `Reg.GetSession` and returns a single-element `[]*Session{immediate}`. There is no ancestor walk left. (parent.go:1307-1315)
4. A replay guard checks `FindMessage(id)` (parent.go:684-712) before creating a duplicate. `FindMessage` still scans every parent-mailbox directory — not a hierarchy leftover, but a genuine lookup-by-ID problem: a message's owning mailbox is not known to a caller holding only the ID (e.g. `relay resolve MESSAGE_ID`).
5. `pendingAttention(parentID, handoffID)` (parent.go:835-847) checks for an existing unresolved attention message before creating a new one. Its body is `for _, holder := range []string{parentID}` — a single-element slice literal, a visible leftover shape from when this iterated an ancestor chain.
6. `createParentMessage` (parent.go:854-899) does the durable write: exclusive-create semantics under a file lock (`lockAuthorityWrite`), duplicate suppression against every existing owner directory (for identical message IDs) and, for attention-class messages, against the *one* addressed holder's pending queue via the same `for _, holder := range []string{msg.ParentSessionID}` single-element-loop shape (parent.go:872).
7. `applyPolicy` is called next (not re-read in full this session, but confirmed to check `applyAgentChildWorkspaceTrust` first). That builtin — distinct from the YAML-driven `PolicyService` in `policy.go` — **still has a hard, unresolved hierarchy dependency**: it matches on `ho.SourceSessionID != "" && msg.ParentSessionID == ho.SourceSessionID`, looks up the parent session's `agent`/`ApexLabel` labels, and checks `child.SourceSessionID != parent.ID`. `PolicyService.Decide` (policy.go) itself is hierarchy-free — it matches only on kind/source_kind/agent/host/text/command.
8. `deliverEscalation(ctx, candidates, ho, msg)` (parent.go:1327-1345) delivers to `candidates[0]`. **Its doc-comment is now stale relative to its own body** (see section 5): the comment above it (parent.go:1317-1325) still describes "failing over to the nearest ancestor" and "keep[ing] the management tree intact," but the body (rewritten in `d25a08c`) gives the intended manager exactly one retry and then, in its own trailing comment, states plainly: "There is no chain to fail over to any more: one handoff addresses one mailbox." (parent.go:1341-1344)
9. `deliverMessage` (not re-read line-by-line this session; carried from the pre-flux read) branches on whether the target is the root-to-human path, a headless parent (writes to the durable inbox file only), or an interactive pane (injects text via the persistence adapter).
10. `ListMessages(parentID, pendingOnly)` (parent.go:648-680) **now reads a single `parentMessageDir(parentID)`** — no longer scans all parent directories. This is the "list my inbox" read path used by `parent list`/`parent inbox`.

What is already flat (hierarchy-free) as of `b23e2bd`:
- `deliveryCandidates`, `pendingAttention`, `createParentMessage`'s targeting, `ListMessages`, `deliverEscalation`'s actual delivery logic, and the entire CLI verb set (`link`/`adopt`/`move`/`retire`/`status` all deleted, `d25a08c`).
- `authority_policy.go` — the entire subtree-scoped bridge-authorization boundary — is deleted outright (`fa9b02b`). It does not exist on disk.

What still needs a flat replacement:
- `applyAgentChildWorkspaceTrust` (parent.go, gated on `SourceSessionID`/`ParentSessionID` matching) — a builtin auto-approval rule, separate from `PolicyService`.
- `board.go`'s `resolveBoard` (board.go:59-79) walks `sess.SourceSessionID` to find the owning manager for a peer-coordination channel (`board.go:71,74`); `QuerySubtree` (board.go:151-169) builds a full children-map keyed on `SourceSessionID` to answer a subtree query (board.go:168-169). Neither was touched by any of the three landed passes and neither is named in the retirement plan's remaining-work table — this is a gap the plan does not currently scope.
- `root.go`'s entire `RootService` (Apex/Adopt/Release/Enroll/Unenroll/Governed/Digest) is explicitly the "apex" pass 4, not started. `ancestor.go`'s `AncestorChain` (still exported, walks `SourceSessionID` upward) has exactly one production caller left: `root.go:319` inside `Enroll`'s cycle guard.

The channel bus underneath `relay msg` (`internal/core/msg.go`, not re-read this session but carried from prior context) is a separate, always-flat primitive: named `chan.<channel>` streams over `ports.Coord`, with per-channel sequence cursors. It shares the low-level event-consumption loop (`streamEvents`, internal/core/eventstream.go) with `board.Watch` and `parent.Watch`.

## 3. Persistence: what survives disconnect, and what `resume` does

**Answer first:** the tmux server process on the remote host is the thing
that survives. relay's job is (a) reconnecting a client to that server
without ever touching the pane, and (b) remembering, in a durable local
registry, whether a given named session is still resumable or was
deliberately torn down, so a later `resume` doesn't recreate work that was
intentionally destroyed.

- `internal/persist/tmux/tmux.go`: `AttachCommand` builds `tmux new-session
  -A -s NAME` — attach-or-create in one shell invocation, so the same command
  works whether the remote tmux session is alive or needs recreating.
  `InstallSensors` wires `monitor-silence`/`pane-died`/`alert-silence` tmux
  hooks plus `remain-on-exit on`, giving idle/exit telemetry without polling
  the pane. `Send`'s submit path includes a multi-attempt loop that confirms
  the composer actually accepted text (with special handling for Codex's
  paste-placeholder UI), and `Launch` uses a one-shot, non-retryable Enter
  plus a tmux-option-based acknowledgment token, specifically to avoid
  double-submitting into a security gate.
- `internal/core/resume_registry.go`: a durable JSON file at
  `StateRoot()/resume-registry.json` (resume_registry.go:44-46) mapping tmux
  persist name → `ResumeEntry{HostID, SessionID, RemoteCWD, RepoRef, State,
  Reason, UpdatedAt}`. `State` is exactly two values: `resumable` (drop from
  transport/cmux — the remote work may still be alive, resume_registry.go:20)
  or `cleaned` (intentional destroy/finalize — do not resume, do not
  recreate, resume_registry.go:22). `ClassifyResume` (resume_registry.go:247-264)
  layers a live local session record on top (`PresenceLive` wins over
  anything in the registry) then falls through to
  `disconnected`/`cleaned`/`unknown`. `ResolveResumeTarget`
  (resume_registry.go:268-287) is the single dispatch point `resume` calls:
  it refuses outright with `ErrResumeCleaned` for a cleaned entry
  (resume_registry.go:271-276), returns the live session's own handle for a
  live one, and for `disconnected` synthesizes a fresh `ports.PersistHandle{Kind:
  "tmux", Name: persistName}` to hand to `AttachCommand`. Every save
  (`saveResumeRegistryLocked`, resume_registry.go:76-92) prunes entries older
  than 60 days; `PruneResume` (resume_registry.go:191-213) is the
  operator-driven equivalent exposed as `relay resume prune`.
- `internal/core/resume.go` (`ResumeOpts`, not re-cited line-by-line this
  session but carried from the earlier full read): freezes the resolved
  connection identity **before** entering the reconnect loop — it never
  re-resolves mid-loop — retries only on a non-clean-exit / non-`SIGINT`
  code, with a fixed retry delay (default 3s, overridable via
  `RELAY_RECONNECT_DELAY`), and can be disabled entirely with
  `--no-reconnect` or `RELAY_AUTO_RECONNECT=0`.

`session.go`'s `Create` (internal/core/session.go:134-247) is the write side
of this: it calls `RememberResume(sess)` (resume_registry.go:114-116) right
after registering the new session, so every created session is resumable by
default from the moment it exists. `Destroy` calls
`MarkResumeCleaned`/relies on the intentional-teardown path when the remote
is actually torn down (session.go:653-681, see section 1) — it no longer has
any hierarchy-shaped refusal blocking that call.

## 4. Viz: the presenter contract

**Answer first:** the presenter contract is nine methods
(`internal/ports/ports.go:200-216`): `Kind`, `Available`, `Present`, `Focus`,
`Close`, `Layout`, `SaveRestorable`, `RestoreSaved`, `BrandLabels`. `cmux`
(`internal/viz/cmux/cmux.go`) is the only implementation today, and it
satisfies the contract by shelling out to the external `cmux` CLI binary and
keeping a local JSON binding cache (surface/pane/workspace/attach-cmd) per
session so that a second `Present` call from a fresh CLI process is
idempotent rather than re-opening a duplicate pane.

- `Present` (cmux.go:276-410) is the main entry point: resolve or create a
  workspace, resolve or create a pane inside it, apply chrome, and persist
  the resulting binding. Its sibling/parent placement logic
  (`latestLiveChild`/`childLayout`/`parentChildLayout`) keys off
  `layout.SourceSessionID` — this **is** a hierarchy read, but a soft one: it
  is cosmetic pane-stacking convenience that degrades gracefully to a plain
  open when the field is empty. It is not a structural dependency like the
  messaging-side ones in section 2.
- `Focus`/`Close`/`ClosePersist`/`closeSurface`/`Layout`/`activeWorkspace`
  (cmux.go:1026-1265) are the read/mutate side of the same binding cache.
- `SaveRestorable`/`RestoreSaved` (cmux.go:1286-1428) integrate with cmux's
  own built-in "surface resume" checkpoint mechanism, so a cmux restart (not
  a relay-side event) can recover pane bindings.
- `internal/viz/cmux/service.go`, `ack.go`, `inject.go`,
  `projection_snapshot.go` back a secondary path — queued presentation and
  authority-snapshot reconciliation for `relay viz present` on a
  non-authoritative ("projection-only") desktop. Symbol-listed but not
  read in depth this session; the primary contract above is fully captured
  via `ports.go` + `cmux.go` directly.

Per the newer `docs/2026-09-05-ghostty-native-app-design.md` (written
alongside this document, not independently re-verified here): the plan names
two concrete blockers to a second presenter existing at all — `relay viz
list`'s current return type is asserted as cmux-specific with no selector
for a different implementation. That is a forward-looking design note, not a
claim this document re-derived.

## 5. Redundancy and vestigial findings

Concrete, cited, no speculation about intent:

1. **Stale doc-comment vs. rewritten body, `deliverEscalation`.**
   parent.go:1317-1325's doc-comment says "failing over to the nearest
   ancestor that can actually receive it" and "Three rules keep the
   management tree intact." The body beneath it (parent.go:1327-1345) gives
   exactly one same-target retry and its own trailing comment
   (parent.go:1341-1344) says the opposite: "There is no chain to fail over
   to any more: one handoff addresses one mailbox." The outer doc-comment was
   not updated when the body was rewritten in `d25a08c`.

2. **Vestigial single-element slice-literal loops.** `pendingAttention`
   (parent.go:836, `for _, holder := range []string{parentID}`) and
   `createParentMessage` (parent.go:872, `for _, holder := range
   []string{msg.ParentSessionID}`) both iterate a one-element slice built
   from a slice literal. This is the visible shape of code that used to loop
   over an ancestor chain and now loops over exactly one thing. Functionally
   harmless; a plain `if` would say the same thing with less surface area
   for a future reader to misread as "there might be more than one."

3. **`viz` and `pane` are the same command, twice.** Top-level `Run()`
   dispatches both names to `cmdViz` (app.go:280, 480). Two names, zero
   behavioral difference.

4. **`host show` / `host fetch` are a declared alias, not two behaviors.**
   app.go:891 — `case "show", "fetch": // fetch is alias of show
   (remote-authoritative pull)`. Fine as documented, but it is one verb
   wearing two names in the inventory count.

5. **`ask` and `resolve` are both sugar over other services.** `cmdAsk`
   (app.go:3039-3058) forwards into `cmdSignal(ctx, "signal", []string{"ask",
   "--text", question})` (app.go:3047) rather than doing anything itself.
   `cmdResolve` (app.go:2335) is the sole agent-facing response operation and
   is a thin wrapper over `ParentService.Reply`. Neither is a distinct
   subsystem; both exist purely as ergonomic top-level verbs over
   `signal`/`parent`.

6. **`board.go` is the one messaging-adjacent file the landed passes did not
   touch, and it is not named in the retirement plan's remaining-work
   table.** `resolveBoard` (board.go:59-79) and `QuerySubtree`
   (board.go:151-169) are both still hard-built on `SourceSessionID`
   (board.go:71,74,168,169). If the plan's pass 4 ("apex") is scoped only to
   `root.go`/`ancestor.go`/`session.go`'s `UnobservableGovernedChildren`/the
   three `ApexLabel` reads in `parent.go`, `supervisor.go`, `authority.go` —
   as its own table states — `board.go`'s hierarchy dependency is currently
   unscoped work, not yet assigned to any pass.

7. **`applyAgentChildWorkspaceTrust` is a second, undocumented hierarchy
   dependency inside the "kept" messaging file.** It lives in `parent.go`
   (the file the retirement plan calls "mostly the messaging engine, not a
   permission system") but it is itself a permission-granting builtin gated
   on `SourceSessionID` matching — the same shape of dependency the plan
   otherwise treats as fully retired with `authority_policy.go`. It was not
   named in the plan's "landed" table for any of the three passes.

8. **`FindMessage`'s all-directories scan looks like a hierarchy leftover but
   isn't.** parent.go:684-712 still walks every parent-mailbox directory,
   which reads the same as the pre-flattening scan-all-ancestors pattern.
   The actual reason is unrelated to hierarchy: a bare message ID (as passed
   to `relay resolve MESSAGE_ID`) does not carry its owning mailbox, so there
   is nothing to index by without a design change (e.g. embedding the
   mailbox name in the ID format). Flagging this so it is not miscategorized
   alongside genuine hierarchy debt in a future cleanup pass.

9. **`gc --channel-ttl` and `resume`'s reap/prune both do bounded,
   time-based cleanup of durable state, from two different commands.** `gc`
   (app.go:1773) takes `--dry-run`/`--channel-ttl`; `resume reap`
   (app.go:3668) and `resume prune` (app.go:3698) do the equivalent for the
   resume registry. Not a bug, but two entry points into "delete old durable
   state," one per subsystem, with no shared verb.

10. **`session create --container` and `container open` reach the same
    container-exec wrapping path from two different top-level command
    groups** (app.go:1316 vs. app.go:4328, both ultimately routing through
    `cmdContainerOpen`, app.go:4371). Whether that is one primitive with two
    front doors (by design) or drift is unclear from the code alone — it is
    not contradicted anywhere, just duplicated.

Framing note on all of the above: this document's section 2 findings on
`deliveryCandidates`/`pendingAttention`/`createParentMessage`/`ListMessages`
being already-flat, and the CLI verb deletions, reflect commits already on
HEAD (`fa9b02b`, `4ed068a`, `d25a08c`) as of `b23e2bd`. Nothing here was
caught mid-edit in an uncommitted working tree — that was true earlier in
this same research pass but the concurrent agent has since committed. Treat
this document, like the retirement plan it complements, as a snapshot dated
2026-09-05, not a standing description.
