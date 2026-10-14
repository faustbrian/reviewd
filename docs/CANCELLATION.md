# Scoped private-review cancellation

This command accompanies the watcher/control workflow. The older `f73eab6`
CI snapshots do not include it; use a snapshot built from a containing commit.

```sh
reviewd cancel <run-id>
reviewd cancel <run-id> --json
reviewd show <run-id> --json
```

Cancellation names one exact run in the selected local profile, not a PR's
latest revision or every run for a repository. It requires no GitHub connection
and performs no remote write. Use `skip <item-id>` for a pending inbox decision
that has not created a run.

Queued and waiting-for-checks runs have no model turn to stop. They become
`cancelled` immediately and cannot later be claimed by a worker. Active runs
retain `reviewing` with `cancellation_requested: true`: success from `cancel`
means the request was recorded, not that the model process has already exited.
Plain `show`/`inbox` output also labels this pending request. Inspect the run
again for its actual terminal state.

The shared review service observes local cancellation independently of GitHub
polling. This works for a manual private review and foreground headless watching,
including `--once`. The owning service interrupts its private worker; the
existing runtime owns scoped turn interruption and process-group cleanup.
Available thread/turn/model evidence is retained. Source and captured-skill
cleanup runs before the service records its terminal cancellation. A worker's
late successful response cannot turn an acknowledged cancellation into a
prepared review. Cleanup or persistence errors remain visible rather than
being reported as a clean review.

An accepted cancellation takes precedence over a concurrent worker failure
when the owner records completion. The underlying failure diagnostic is retained;
the run does not remain marked as an unconfirmed request after its owner exits.

A controlled native macOS check exercised a second CLI cancelling an actual
Codex turn, retained its thread/turn identity, and removed owned runtime/source
state. This does not prove orphan recovery or every platform's process behaviour.

A stopped or crashed owner cannot acknowledge a live request. The request
remains visible in durable state until the owner exits normally or recovery
classifies the interrupted run; this command does not claim to have killed an
unreachable process. The separate local [owner/resource recovery](RECOVERY.md)
classifies positively identified lost owners while preserving live or ambiguous
resources. Material boundary changes use a distinct supersession marker and
settle as stale; an accepted explicit cancellation still takes precedence.

Prepared reviews are not active worker work: inspect their exact preview and
make the separate publication decision, or skip the linked inbox event to stop
its automatic publication. `publishing`, `published`, `publication_unknown`
and `partial` are also not cancellable. They retain their actual publication
state and require the ordinary inspection/reconciliation workflow; cancellation
does not retract a submitted GitHub review. Failed/stale runs likewise retain
their evidence. Repeating cancellation for an already cancelled run is harmless.

Project pause and item snooze are different: they retain scheduled work and
allow an already-running private review to finish. See [personal holds](CONTROLS.md).
