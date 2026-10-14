# Private owner and resource recovery

Recovery is included in the delivered `dd4d595` development snapshots; the older
`f73eab6` binary does not contain it. Native automated macOS/Linux checks passed;
local recovery evidence does not establish live GitHub publication or every
hard-crash mode. See the bounded [native owner-exit acceptance](ACCEPTANCE.md#native-macos-observations-at-the-delivered-revision).

```sh
reviewd recover --dry-run
reviewd recover --json
reviewd show <run-id>
```

Recovery is local maintenance. It never resumes a Codex thread, resends an
answer, starts a new review, authorizes publication or signals a saved PID.
Inspect the retained run and explicitly request fresh work when needed.

## Ownership and storage

Each new manual or scheduled preparation holds a kernel lease for its private
scope under `<profile-state>/private-work/<opaque-owner-id>`. Immutable source,
the frozen installed skill and the isolated Codex control/workspace live below
that scope. Provider authentication copied into a control directory remains
private; it is not included in ordinary output, notifications or worker tools.

SQLite records the scope identity, directory and locked-file device/inode, and
process launches.
A launch is recorded before starting the child and its dedicated process group
is then bound to that record. A crash between those operations leaves explicit
launch ambiguity, not an apparently empty scope. Source Git commands, the
short Codex version probe, primary and specialist runtimes share this owner.

Publication and explicit repair acquire source in a separate resource scope
using the same durable ownership mechanism. This scope does not replace the
prepared run's owner or bind it to a new model session. Failed acquisition can
therefore retain source and child metadata even when no snapshot handle was
returned. Recovery reclaims that scope only after shutdown is proven; it leaves
the prepared payload, consent and known remote publication identities intact
and never retries a review or inline submission.

The application owner alone holds its close-on-exec lease. An old timestamp or
stored process number does not prove that it died. A replacement directory,
symlink, missing lease or invalid ownership metadata is preserved for inspection.
Recovery does not glob and delete old system temporary directories or resources
created by an older binary without ownership records.

Normal success, failure and cancellation close private children before removing
their storage. A durable closing barrier prevents late launches and records
shutdown before deletion. Final acknowledgement retains a compact `owner_closed`
fact on the run. Recovery can therefore finish interrupted deletion and a lost
database acknowledgement without guessing that a still-running review died.

During normal execution, the supervisor observes child exit without reaping it:
macOS uses a process-exit kernel event and Linux uses `waitid` with `WNOWAIT`.
The direct child's PID therefore remains reserved until the supervisor finishes
all process-group signals. Only then does the sole waiter reap it. Context
cancellation does not run a competing group-signalling callback. An inspection
or cleanup failure retains private storage rather than claiming clean shutdown.
The shared owner gates service teardown as well: source and frozen skill inputs
are not independently deleted while a primary or specialist child is unresolved.

## Classification

| Report state | Meaning and effect |
| --- | --- |
| `live` | The stable lease is held; leave its work and resources alone. This may be another maintenance contender, not proof of an active model turn. |
| `recovered` | Proven owner loss or acknowledged shutdown; interrupted private work becomes failed/cancelled/stale according to its retained stop cause, and safely stopped owned storage is reclaimed. In dry-run output this is the proposed action only. |
| `already_recovered` | Another client completed the observed cleanup; no new action is needed. |
| `resources_pending` | The owner stopped, but a child group is live, a launch is unresolved or inspection is unavailable. Retain the scope; do not signal the saved group. |
| `unknown` / `unknown_runs` | Ownership/liveness cannot be established, including legacy active runs. Preserve state and storage and require operator attention. |
| `recovery_failed` | Persistence failed; do not interpret the attempted recovery as a completed state transition. |

Kernel-proven group absence needs no full process-table scan. Otherwise,
inspection uses only `ps` process-group/status metadata, not command arguments
or environment. Missing, failed or malformed required inspection retains
resources. On macOS, an individual owned group's check selects only that group,
rather than collecting unrelated processes for each short source command.
Linux full scans ignore unrelated kernel-thread group-zero rows. A zombie
leader marked as multithreaded (`Zl`) remains live/unknown for cleanup purposes;
an exited main thread alone does not prove its siblings stopped.
A reused group number can delay cleanup but cannot cause recovery
to kill an unrelated process. If a child remains, independently identify and
stop only that owned instance using ordinary OS tools, then run `recover`
again. Do not remove a live control directory.

Interrupted private runs retain run/work identity, frozen evidence, thread/turn,
model/usage and input delivery facts. Outstanding input is closed; a previously
sending/sent answer retains an unknown-delivery fact. Answers are never replayed.
Prepared previews and all publication/partial/unknown outcomes are left intact.
A scheduler link alone no longer turns a possibly live manual start into a
failed run. Notifications and ordinary inbox/details consume the resulting
durable state, not an invented clean result.

## Automatic and bounded operation

Manual `review --private` and watcher startup run the same recovery operation.
The attached terminal delegates to the same watcher. A continuous watcher also
checks once per minute while waiting, independently of GitHub polling.

Each pass has a five-second context budget and inspects at most 25 resource
owners plus 25 active records missing ownership. Non-dry passes rotate checked
owners so old live work does not indefinitely hide later abandoned resources.
The active-record scan prioritizes valid acknowledged shutdowns, so preserved
unknown legacy records cannot hide work that can safely become terminal.
An individual owner/run failure stays visible but does not abort independently
recoverable candidates in the same bounded pass. Failed owners and missing-owner
records still rotate, including across client restarts, without changing run
history timestamps. A database that cannot persist scan progress remains an
explicit error; it is not treated as successfully recovered.
Dry-run does not update the scan order, run history, decisions or files. A full
pass reports that another pass may be needed, not that more dead owners are
known to exist. Unknown legacy records can repeat and continue to require
inspection; use inbox/show for their complete retained history.

JSON uses the ordinary versioned output envelope with a `recovery` object.
Exit 1 means an operation failed or unknown/pending resources require attention;
live owners alone are not an error. Partial progress remains visible when a
later operation fails. This maintenance does not grant any execution decision.

Ownership and durable GitHub cooldowns upgrade private SQLite state to schema
version 7. Recovery preserves active host deadlines.
Older binaries reject it rather than downgrade it. Preserve the profile when
switching snapshots; do not replace the database and lose publication history.
Crash-before-registration can leave an empty allocation directory without any
source or copied authentication; unregistered directories are not automatically
deleted. Logical private-text expiry is a separate [retention](RETENTION.md)
operation and never substitutes for liveness classification.
