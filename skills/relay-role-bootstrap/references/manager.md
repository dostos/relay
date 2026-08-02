# Project root or manager

A manager owns only its immediate children. A project root reports to the human
unless explicitly enrolled under an apex; an intermediate manager must have an
explicit parent session.

## Sequence

1. Search for a matching registered parent by stable session identity and repo
   scope.
2. For an existing human-facing surface, use `relay parent register` or
   `relay parent bind`; do not launch an agent merely to manufacture a root.
3. For an agent manager, start a concrete management goal with
   `relay agent start HOST AGENT --parent PARENT --name NAME -- GOAL` when it is
   intermediate, or without `--parent` when it is intentionally a root.
4. Verify the session role, repository scope, readiness, and immediate-parent
   lineage. Verify `relay parent status SESSION` before assigning children.
5. Run `relay root enroll SESSION` only when the user explicitly requests
   governance by the current apex. A missing rules file is a hold, not consent.

The goal must state the manager's scope, what it may decide, what it must
escalate, and that it may not contact grandchildren directly.
