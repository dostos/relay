# Viz-only SSH key

The optional Viz host must use a dedicated SSH key whose `authorized_keys`
entry on the control host is restricted to Relay's Viz broker. For a service
named `relay-viz-mac`, the owner-managed entry is:

```text
restrict,command="$HOME/.local/bin/relayd viz-broker --service relay-viz-mac" ssh-ed25519 AAAA... relay-viz-mac
```

`restrict` disables forwarding, PTY allocation, agent forwarding, X11, and
user rc processing. The forced command ignores the requested executable and
accepts only these protocol operations for the configured service:

- follow or replay that service's projection stream;
- append a bounded acknowledgement to that service's ack stream.

Use a separate key from interactive administration. Installing or changing
this entry is an explicit owner permission decision; Relay does not edit
`authorized_keys` automatically.
