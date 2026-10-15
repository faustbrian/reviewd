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

### Linux feedback command completion

Feedback command hosts validate the selected native Codex **0.162.1** before
dispatching checkout commands and explicitly disable legacy Landlock mode.
Restricted Linux execution uses its PID-namespace backend. Each command has a
connection-scoped control identity; cancellation requests termination but keeps
the original execution response correlated until its terminal exit/output facts
arrive. A terminate acknowledgement alone does not release capture or cleanup.
Nonzero exit remains a completed execution, not a passing verification.

Before dispatch, Linux commands reserve an unresolved slot in the existing
private owner. Only canonical completion clears it. Missing, malformed or lost
terminal facts retain the runtime, workspace and owner across restart, even when
the app-server process group is absent. Recovery reports `resources_pending`
and preserves feedback effects rather than permitting a replacement modifier.
This requires no new state migration; older clients also retain unresolved slots.

An unacknowledged command cannot currently be reconciled merely by observing or
stopping the old server group. Do not delete its private scope or treat another
recovery pass as proof of completion. Automatic reclamation of that ambiguous
command scope is not implemented. A feedback run with that ambiguity remains
held; neither explicit continuation nor observing an absent server authorizes
replacement or deletion. Read-only reviewers gain no repository-command grants.

### Daemon-owned container resources

The existing private owner can also reserve a tagged Docker container resource
before creation. Binding its immutable container ID requires a matching daemon,
application-generated name and owner/resource labels. It cannot be rebound to
another container. Native process entries and container entries share the same
bounded resource inventory and closing barrier; there is no second lease or
effect journal. Older clients cannot decode tagged entries and retain storage.

Container inspection uses a local Unix socket and Docker Engine API 1.44, only
when the daemon advertises that API and a supported Linux architecture. Remote
endpoints, redirects and proxy-environment routing are not used. A known ID is
absent only after a canonical not-found response bracketed by matching daemon
identity checks. Missing metadata, a replaced daemon and an unbound create
intent remain uncertain. A still-present container, even if exited, does not
authorize deletion of private storage. Recovery does not start, stop, remove or
replay a container operation.

The internal command provider can create one owned container per command. It
requires a locally present immutable Linux image matching the daemon architecture;
it never pulls an image, starts a runtime, or falls back to host execution. The
operator-selected image supplies its installed toolchain search path, not the
host environment. Ambiguous paths and writable workspace/scratch search paths
are rejected during image preflight.

Commands receive their private checkout bind mount, read-only or writable as
requested, a read-only static Linux helper and private output scratch under the
same owner. Root host identities are rejected before command creation; the guest
retains the non-root owner's UID/GID instead of selecting an elevated user. Image root is read-only; temporary scratch is bounded. Network
access, privilege escalation, additional capabilities and daemon log retention
are disabled. The daemon socket is control-plane access for reviewd, never a
command mount. These Linux commands cannot establish that a required macOS-only
check passed. Tools and offline dependencies must already be available in the
selected image or private checkout.

The helper is the container's PID 1. It disables dumpability before launching
repository code, excluding its per-command authentication key from the child's
environment and inherited descriptors. The key remains a private owner input
outside container mounts. The helper authenticates its own bounded stdout/stderr
pipe observations and foreground exit, rather than trusting files the command
can rewrite. A key-free trusted child applies Landlock write restrictions on a
locked OS thread before replacing itself with repository code. Writes, creates,
removals, truncation and cross-directory links are restricted to the checkout,
bounded temporary scratch and normal device outputs, excluding capture storage.
The checkout's read-only mount still controls read-only commands. The kernel and
runtime must permit Landlock ABI 3 or newer; unsupported enforcement fails before
repository execution, without a native fallback or additional-capability grant.
Capture authentication remains independent of filesystem permissions; metadata
permission tampering can deny completion, not forge it.

No attachment stream is used as output authority. After a canonical terminal
wait, the provider removes the exact owned container and proves its absence
before reading capture files. Separate streams are limited to 1 MiB each; exact
limits are accepted, overflow is incomplete. Readback rejects missing, malformed,
truncated, oversized, symlinked or special-file output and requires authenticated
request identity, byte counts, digests and the matching explicit exit code.
Invalid UTF-8 is unsupported and incomplete, never silently normalized.

Start acknowledgement, foreground exit, complete output and proven container
removal remain different facts. The tagged container reservation is the sole
lifetime authority for these commands. An uncertain create or unproven removal
retains that resource and blocks replacement modification. After confirmed
removal, missing or invalid output remains a failed command, not an unproven
container lifetime. It cannot satisfy verification or trigger an implicit retry;
unfinished feedback keeps its original workspace and recorded effects for
explicit same-owner continuation. Cancellation cannot turn unknown output into
success. Historical unidentified numeric completion slots remain preserved;
there is no automatic migration or reclamation of those older ambiguous records.

After successful restriction setup, a proven missing executable returns an
authenticated exit 127 with a fixed diagnostic, so an agent may choose a different
available command. Unsupported restriction setup and other unproven dispatch
failures remain incomplete; neither path permits host execution as a fallback.

Initial macOS checkout now selects this provider using frozen operator-only
endpoint/image settings and the paired installed helper. Git trusts only the
exact private `/workspace` mount in the command environment, accommodating guest
ownership translation without global settings or a wildcard. Later command
phases can reopen the same backend with read-only grants only after known prior
shutdown; uncertainty prevents replacement.

Checkout must preserve the exact branch, head and complete index tree. After
command-host shutdown, the application verifies every worktree blob byte, mode,
symlink target and exact pathname against the immutable full manifest, including
binary and unchanged source omitted from text review. Collapsed case/Unicode
names, transformed checkout bytes, unsupported paths/submodules, and trees over
1 GiB are rejected rather than silently certified. Complete-object export has
its separate existing 1-GiB bound. Native Linux checkout retains its backend and
the same fidelity checks.

The separate feedback service uses this checkout/command provider and captured
installed instructions. Ordinary read-only review gains no command grants.

### Explicit retained-feedback continuation

Use `reviewd feedback-resume <run-id>` to inspect the existing run, then its
displayed `--confirm` command to continue exactly that stored outcome. JSON and
the terminal inbox use the same operation. Recovery itself does not run models,
commands or remote mutations. The original owner, complete source, captured
instructions and staged work remain; configuration/model changes are rejected.
Setup failures before implementation retain the captured instructions and any
successfully acquired immutable source. Explicit continuation completes missing
acquisition or workspace preparation in the same owner; it does not rebuild an
already adopted workspace or retry a command with unresolved shutdown.

Before delivery, continuation uses a fresh primary agent and fresh verification
of retained work. The original bounded planning inputs stay historical. After
an attempted delivery, the original reviewed projection and effect journal
remain authoritative: canonical remote reads may identify an accepted effect,
but uncertain mutations are never repeated. Current-head checks can become
ready without another commit/push; replies and reviewer requests not previously
attempted can then finish. A fresh post-delivery audit precedes completion.
New human feedback, unrelated source/local changes, altered retained evidence
or unavailable canonical facts hold the same run for operator attention.

Feedback execution journals are excluded from ordinary review-history expiry.
Their recorded effects and handled revisions remain available for reconciliation
and duplicate suppression, including failed/cancelled runs.
Update and restart every client sharing a feedback profile before enabling this
executor, or use a fresh profile. Older same-schema binaries do not contain this
expiry guard and can erase failed/cancelled feedback facts; filesystem-resource
retention alone does not protect those journals. Mixed-version operation and
downgrades of an active feedback profile are not supported.

## Classification

| Report state | Meaning and effect |
| --- | --- |
| `live` | The stable lease is held; leave its work and resources alone. This may be another maintenance contender, not proof of an active model turn. |
| `recovered` | Proven owner loss or acknowledged shutdown; interrupted private work becomes failed/cancelled/stale according to its retained stop cause, and safely stopped owned storage is reclaimed. In dry-run output this is the proposed action only. |
| `already_recovered` | Another client completed the observed cleanup; no new action is needed. |
| `resources_pending` | The owner stopped, but a child group is live, a launch or command completion is unresolved, or inspection is unavailable. Retain the scope; do not signal the saved group. |
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
