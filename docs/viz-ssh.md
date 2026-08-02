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

The owner can perform that explicit enrollment with:

```bash
relayd viz authorize --service relay-viz-mac --public-key-file ./viz.pub
```

The command validates the key fingerprint, locks and atomically updates
`~/.ssh/authorized_keys`, and installs only the restricted broker command. It
refuses symlinks, unsafe permissions, malformed keys, and any existing copy of
the key with different or unrestricted authorization. It is idempotent when
the exact restricted entry already exists; it never guesses which key to use.
