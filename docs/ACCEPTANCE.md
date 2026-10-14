# Acceptance tracking

This mapping preserves every original numbered acceptance requirement. Partial
SDK evidence does not imply that the full application workflow is complete.
The exact numbering refers to section 16 of the specification.

| ID | Required outcome | Current evidence |
| --- | --- | --- |
| 01 | API notify-only never starts a reviewer | Owned headless CLI journey imports notify-only work without source acquisition or a model turn; live acceptance outstanding |
| 02 | Bill automatic flow prepares and verifies publication | Owned Git/HTTP/model watcher journey waits for CI, prepares, publishes, verifies readback and does not repost after restart; live GitHub acceptance outstanding |
| 03 | Duplicate observations do not duplicate jobs | Owned watcher restarts preserve event/run identity and do not repeat model turns or publication in the exercised journey |
| 04 | New commits invalidate old publication consent | Local watcher/service/store journeys invalidate linked waiting/prepared evidence and stop active work as stale, reject late preparation and old publication decisions, and preserve publication already claimed; live GitHub acceptance outstanding |
| 05 | Explicit same-head re-request creates distinct work | Local owned watcher journey dispatches a distinct prepared run for an appended request at the same base/head, preserves the prior run and does not repeat after another restart; live GitHub acceptance outstanding |
| 06 | Material context invalidates cached review evidence | Local journeys exercise changed requirements, including a simultaneous same-head re-request, across active/waiting/prepared/manual final validation; pure re-requests retain distinct valid evidence; live acceptance outstanding |
| 07 | Restart after submit does not repost blindly | Partial: durable attempts and repeated publication/reconciliation with reopened SQLite clients; live GitHub acceptance outstanding |
| 08 | Lost responses reconcile or remain explicitly unknown | Owned HTTP transport-loss journeys verify both canonical recovery and durable unknown/no-repost for reviews and repairs; live acceptance outstanding |
| 09 | Partial publication avoids duplicate comments | Explicit repair journey posts only the missing finding, retains the original review and reconciles lost repair responses; live acceptance outstanding |
| 10 | Ask remains pending without an attached terminal | Durable pending/start/skip decisions verified through ordinary CLI and reopened SQLite; native PTY dismissal/interrupt grants no decision |
| 11 | Notification expiry never authorizes action | Local headless/SQLite notification checks preserve pending start and publication decisions; no click/expiry action grants authority; visible OS acceptance outstanding |
| 12 | Initial sync does not process the backlog automatically | Owned CLI/HTTP/SQLite watcher journey holds initial automatic requests; explicit item-scoped start releases a private decision |
| 13 | Proposed repository config cannot expand authority | Target-base private flow verified with owned Git/HTTP source and real Codex; the owned automatic watcher also ignores proposed head model/publication grants while using trusted base preferences |
| 14 | Field-level override/provenance resolution | Resolver/explanation and frozen manual-run policy verified; owned automatic watcher uses an exact-project effort override over repository preferences and retains project/base provenance |
| 15 | Invalid reload preserves the last valid config | Owned continuous headless/JSON watcher retains successive notify/ask policies across invalid reloads, recovers on valid edits, and starts no reviewer; frozen waiting-run model is separately verified after configuration change |
| 16 | Missing prerequisites produce actionable errors | Setup/doctor verified with fixtures and hands-on real account metadata; actual review/provider failures remain a separate boundary |
| 17 | Mandatory check states classified accurately | Required context/head/integration state fixtures and end-of-preparation refresh exercise the supported check states; unresolved reusable-workflow source identity remains an explicit unavailable readiness gap, not a passing check |
| 18 | Interrupted/malformed workers never become clean reviews | Partial: protocol EOF/malformed-response tests plus cross-client cancellation, rejected late preparation and a worker returning a late successful response; active primary identity/status persists before completion, and a progress-persistence failure stops the turn; local clarification journeys retain exact questions, require an explicit second-client answer, and fail on unanswered clear/cancellation/EOF |
| 19 | Own-PR feedback grants no fix authority | Discovery imports human feedback as attention-only and never starts a fixer; local notifications group feedback with other attention for the same PR; visible desktop acceptance outstanding |
| 20 | Self-review uses the permitted GitHub event | Prepared-result and publisher fixtures verify separate internal opinion and head-pinned COMMENT; real publication outstanding |
| 21 | Closing a PR cancels queued work | Owned CLI journey verifies closure before a queued explicit decision can acquire source or create a run; it does not establish native active-worker closure behavior |
| 22 | Debounce coalesces code updates | Local owned Git/HTTP watcher burst holds intermediate heads without source/model work, then acquires and prepares only the final revision once; live/native acceptance remains outstanding |
| 23 | Quiet hours/snooze/grouping preserve inbox state | Local SQLite/CLI checks cover grouping, quiet-hour/DST digests, restart deduplication, reconnect summaries and nonfatal desktop failures; personal hold checks cover durable duration/head snoozes and project pause/resume; visible OS acceptance remains outstanding |
| 24 | Concurrent clients preserve scheduler invariants | Partial: two SQLite clients, exclusive watcher lease and shared model slots verified across processes, including lowered caps and process exit; cancellation versus preparation has one transactional winner; local scheduler admission exercises fresh-request preference and older optional progress through repeated refills/restarts; native Linux runtime remains outstanding |
| 25 | Model and effort reach actual Codex invocation | Partial: subprocess wire tests, opt-in live fixture and an actual primary/independent leaf acceptance using gpt-6.1-sol/high; the leaf evidence capability and distinct sessions were exercised |
| 26 | Worker cannot bypass publication policy | Partial: sealed broker, restricted runtime and excluded credentials; publisher remains an application-only capability |
| 27 | Owned snapshots/process trees clean up safely | Partial: runtime checks, native bare-source success/failure cleanup and private acceptance; cross-client cancellation journeys remove actual immutable snapshots; controlled native macOS cancellation of an actual Codex turn retains identity and removes owned runtime/source state; local owner/resource recovery also covers loss, ambiguity, rollback and bounded progress; actual Codex hard-crash and native Linux runtime remain unverified |
| 28 | TUI resize/keyboard/native selection/restoration | Partial: optional attachment, Unicode/cell layout and exact-preview checks; native macOS PTY journeys verify confirmed overrides, dismissal/interrupt, resizing, restored operational terminal modes and no mouse-reporting activation; native macOS clarification editor also verifies typed secret-masked answers, dismissal, stale-target rejection and interruption; actual unmodified drag selection and native Linux UX remain unverified |
| 29 | Headless and JSON expose the same workflow states | Partial: keyboard attachment reuses ordinary commands for decisions, controls, cancellation and publication; headless/plain/JSON state checks and redirected-output fallback exercised; local clarification plain/JSON answering and secret-masked terminal editor share the exact input ID; final cross-surface/native terminal acceptance remains outstanding |
| 30 | Platform builds and native runtime match claims | Partial: distribution f73eab6 passed all four native quality/package jobs and produced four verified downloadable artifacts; later local watcher/notification/control changes and platform UX remain pending |

Named commands, discovery completeness, full review evidence, selected review
skill quality, publication preflight, configuration validation, retry budgets,
retention acceptance, installation and distribution documentation remain in scope beyond
this table. They must be audited before declaring the complete goal achieved.

The current CLI's hands-on acceptance used task-owned configuration/binary
storage, the existing Codex account, and read-only GitHub access to
`faustbrian/reviewd`. Initialization, doctor, online/offline explanation, safe
repeat initialization and bash/zsh/fish completion were exercised. This did
not start a review or publish anything; it is not full vertical-slice acceptance.

## Local clarification boundary

The later local input batch adds durable textual clarification; see
[sessions](SESSIONS.md). The owned manual/headless stdio journeys observe a
pending question before a second CLI answers, continue to deny execution and
permission requests, then prepare privately. Competing selections, stale targets,
reopened histories, native/dynamic server clears and blocked writes have focused
coverage. Actual authenticated Codex/model acceptance asked and consumed an
independently generated answer in an owned private fixture. A native macOS headless-watcher run also consumes an explicit
start decision, exposes the active thread/turn and pending input through another
plain/JSON CLI client, consumes its answer, prepares privately, and retains the
same run/thread/turn/payload after a second watcher invocation. It confirms owned
private storage removal. This uses the fixture skill and owned Git/HTTP, not real
GitHub publication or staff-review-quality acceptance.

The synthetic-provider fixture uses the real pinned host and normal application
storage/answer commands but a test worker adapter skips personal-account
metadata: custom-provider metadata reports no account and does not prove a
personal login. The separate real-account fixture uses the default private CLI
worker without that substitution. Neither test posts to GitHub. Native input
availability in Default mode is not claimed; the product uses its restricted
application-owned clarification tool. The synthetic provider omits a top-level tools field: its functional oracle is
the actual host invocation, explicit CLI answer and exact next-provider result,
not a guessed request layout or history-text search. Full native crash recovery,
native Linux UX and visible desktop acceptance remain outstanding. Local
[private-history retention](RETENTION.md) has separate verification boundaries;
it does not identify or reclaim crash-left processes or storage.
Native macOS PTY input-editor journeys exercise explicit answers, dismissal,
interruption, stale-target rejection, resizing and terminal restoration; they do
not prove real unmodified drag selection or native Linux behavior.

## Local private-owner recovery boundary

The [owner/resource recovery](RECOVERY.md) addition covers manual and scheduled
private scopes. Local application/SQLite fixtures exercise owner-process exit,
live-child/ambiguous-launch preservation, rollback, concurrent clients,
interrupted cleanup, replaced storage/leases and bounded scan progress. These
do not establish native Linux behavior, live GitHub publication or actual
Codex hard-crash acceptance. Source checks do not prove remote delivery.

## Local retention boundary

Private-text expiry is wired through ordinary `cleanup` and the headless/attached
watcher, using `state.retention_days`. Focused application/SQLite checks exercise
dry-run preservation, protected pending/ambiguous work, per-run rollback,
concurrent expiry, age/batch boundaries, compact publication identities,
notification deduplication, and following/unfollowing after preview expiry.
The archived summary is not reusable source evidence. This does not provide
orphan/resource recovery or certify native Linux behavior, live GitHub
publication, or delivery of a later source revision.

## Native installed-skill watcher boundary

A separate current native macOS check uses the actual authorized Codex account
and the operator-selected installed `requesting-code-review` package, not the
clarification fixture skill. An owned immutable percentage-discount change
removes the division by 100 while its caller still requires 75 for a 25-percent
discount on 100. The watcher retains a private canonical blocker on the changed
line, the selected skill digest and primary execution identities, and confirms
owned storage removal. Ordinary show/inbox clients can inspect the result.
This proves that seeded source-review composition only; it is not live GitHub
publication, general review-quality certification, or native Linux/visible UX.
