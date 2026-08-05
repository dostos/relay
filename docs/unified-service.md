# Unified Relay service

Relay ships one primary executable with three runtime roles:

```text
home:     relay service run
viz:      relay viz serve
edge:     relay service event run
clients:  relay <command>
```

The home process is the only durable authority for hierarchy, handoffs,
messages, policy decisions, routing, and canonical presentation intent. Its
event coordinator, authenticated command boundary/forwarders, and watcher
reconciler are independently supervised. `relay service status` reads the
durable health receipt and is successful only when every component is live,
ready, on the process build, and has verified durable-effect capability.
The watcher tick also reconciles terminal pane receipts before computing the
watch set, so direct jobs finish even when their last observable effect is a
terminal receipt rather than a new event.

Each stateless command carries a random invocation ID. The home boundary binds
that ID to authenticated identity and exact argv, records one authority
decision, and durably stores the completed response. A retry returns that
response without repeating the effect. A receipt left pending by an interrupted
execution is reported as ambiguous and is never silently replayed.

Viz is a projection consumer. It fetches a fresh authoritative snapshot,
reconciles local cmux surfaces, resumes from a durable cursor, rejects stale or
duplicate acknowledgements, and returns idempotent receipts. Starting Viz
retires local authority state and creates the projection-only role marker; it
cannot mutate the home registry. Stateless authority commands on that client
use the `command.host` OpenSSH alias from `viz.json` and execute against the
home command boundary; the restricted Viz control key is never widened.

## Installation and migration

`install.sh` builds only `relay`. During the compatibility window it installs
`relayd` as a symlink to the same executable; the argv-0 shim accepts only the
legacy event, service, Viz follower, forced-broker, version, and build routes
and emits a deprecation diagnostic.

Before the unit migration, the three old entrypoints route narrowly to the
event, command-boundary, and watcher components respectively. This keeps an
unexpected legacy-unit restart safe after binary installation without allowing
those split compatibility roles to become the final topology.

Installing a binary does not authorize stopping a system-owned service. If a
root/system unit owns any old process, installation preserves it and prints the
exact administrator command. User-owned units migrate only when the caller
sets `RELAY_MIGRATE_SERVICE=1`. Every install writes a migration receipt under
the Relay state directory naming the installed build, canonical event socket,
legacy PIDs, system-owned units, and rollback action.

Before migration:

1. Run the new service with disposable `RELAY_STATE_DIR` and `RELAYD_SOCK`.
2. Verify `relay service status`, event replay, watcher adoption, command
   authorization, component restart isolation, and shutdown.
3. Install the binary without changing live system-owned units.
4. Review `relay doctor` and the migration receipt.
5. Make the explicit service-owner decision. For user units, rerun with
   `RELAY_MIGRATE_SERVICE=1`; for system units, use the printed `sudo systemctl`
   command yourself. The installer writes a proposed system unit with the
   current non-root user, group, home, socket, and PATH rather than allowing
   systemd `%h` to resolve to root's home.
6. Read back that one `relay service run` process owns the event and command
   sockets and that all old units are inactive. Keep the receipt for rollback.
7. Only after that read-back, disable the preserved legacy units. If the new
   service fails acceptance, stop it and restart those legacy units immediately.

Rollback restores the prior `relay` binary and re-enables the units recorded
in the receipt. Do not delete old units until the read-back succeeds.

## Compatibility removal condition

The `relayd` symlink/argv-0 shim may be removed only when both conditions are
proven from deployment inventory, not merely after a date:

- every installed service unit, SSH forced command, tmux sensor, and managed
  client uses `relay service …`, `relay viz serve`, or `relay viz-broker`; and
- the minimum connected client build is at or above the first build that emits
  no `relayd` command paths.

Until both are true, compatibility remains narrow and deprecation is visible.

The executable acceptance checklist and live-gate status are maintained in
[`unified-service-acceptance.md`](unified-service-acceptance.md).
