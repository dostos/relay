# Goal contract

Write the shortest goal that preserves intent and evaluation integrity.

Include:

- **Outcome:** observable end state, not a list of keystrokes.
- **Context:** only facts the child cannot cheaply discover.
- **Scope:** writable repositories, data, hosts, and allowed external actions.
- **Constraints:** safety, authority, compatibility, benchmark, and publication
  boundaries.
- **Evidence:** tests, live checks, receipts, or metrics required for acceptance.
- **Reporting:** milestone events and the exact information needed at completion.
- **Terminal conditions:** done, bounded rejection limit, or a genuine ask.

## Avoid micromanagement

- Do not prescribe file names, function shapes, command sequences, or an
  implementation plan unless correctness or safety depends on them.
- Do not require frequent status narration. Ask for milestone progress only
  when it changes a manager decision.
- Do not send a replacement plan because the child chose a different sound
  approach.
- Do not broaden a goal after start. Start a new handoff or obtain authority
  when intent materially changes.
- Make autonomy explicit: the child may investigate, implement, test, and
  revise within scope without asking for ordinary tactical choices.

A useful goal tells the child what must be true and where it must stop, while
leaving how to achieve it to the child.

Use `relay signal progress|note` for durable telemetry that should not wake the
manager, `relay ask` for a real decision, and explicit `relay signal result
--text ...` for a manager-visible milestone or conclusion. Provider Stop hooks
are receipts and do not substitute for an explicit result.
