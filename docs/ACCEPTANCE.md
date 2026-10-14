# Acceptance tracking

This mapping preserves every original numbered acceptance requirement. Partial
SDK evidence does not imply that the full application workflow is complete.
The exact numbering refers to section 16 of the specification.

## Current delivered boundary

The integrated workflow is committed and pushed at
`dd4d595e5c7fc176e23a0c9191771063828c8ecf` (tree
`399ba084a69692c32536514106021d38d336a400`). All four native quality and all
four native packaging jobs passed in
[CI run 38590705761](https://github.com/faustbrian/reviewd/actions/runs/38590705761).
Downloaded archives for all four targets passed checksum/provenance/license
inspection; the macOS arm64 download also passed local version/help/completion
smoke. This includes the watcher, notifications, terminal controls, clarification,
owner recovery, retention and durable GitHub cooldowns. It is a development
snapshot, not a formal release or whole-goal completion.

Real Codex execution, owned Git/HTTP journeys, installed-host capability checks,
native GUI observations and live GitHub publication are separate boundaries.
No test review has been posted to GitHub.

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
| 11 | Notification expiry never authorizes action | Local headless/SQLite notification checks preserve pending start and publication decisions; no click/expiry action grants authority; native macOS adapter dispatch/AX observation is separate from these decision checks; Linux desktop acceptance outstanding |
| 12 | Initial sync does not process the backlog automatically | Owned CLI/HTTP/SQLite watcher journey holds initial automatic requests; explicit item-scoped start releases a private decision |
| 13 | Proposed repository config cannot expand authority | Target-base private flow verified with owned Git/HTTP source and real Codex; the owned automatic watcher also ignores proposed head model/publication grants while using trusted base preferences |
| 14 | Field-level override/provenance resolution | Resolver/explanation and frozen manual-run policy verified; owned automatic watcher uses an exact-project effort override over repository preferences and retains project/base provenance |
| 15 | Invalid reload preserves the last valid config | Owned continuous headless/JSON watcher retains successive notify/ask policies across invalid reloads, recovers on valid edits, and starts no reviewer; frozen waiting-run model is separately verified after configuration change |
| 16 | Missing prerequisites produce actionable errors | Setup/doctor verified with fixtures and hands-on real account metadata; actual review/provider failures remain a separate boundary |
| 17 | Mandatory check states classified accurately | Required context/head/integration state fixtures and end-of-preparation refresh exercise the supported check states; unresolved reusable-workflow source identity remains an explicit unavailable readiness gap, not a passing check |
| 18 | Interrupted/malformed workers never become clean reviews | Partial: protocol EOF/malformed-response tests plus cross-client cancellation, rejected late preparation and a worker returning a late successful response; active primary identity/status persists before completion, and a progress-persistence failure stops the turn; local clarification journeys retain exact questions, require an explicit second-client answer, and fail on unanswered clear/cancellation/EOF |
| 19 | Own-PR feedback grants no fix authority | Discovery imports human feedback as attention-only and never starts a fixer; local notifications group feedback with other attention for the same PR; native macOS adapter observation does not establish real feedback delivery; live feedback/Linux desktop acceptance outstanding |
| 20 | Self-review uses the permitted GitHub event | Prepared-result and publisher fixtures verify separate internal opinion and head-pinned COMMENT; real publication outstanding |
| 21 | Closing a PR cancels queued work | Owned CLI journey verifies closure before a queued explicit decision can acquire source or create a run; it does not establish native active-worker closure behavior |
| 22 | Debounce coalesces code updates | Local owned Git/HTTP watcher burst holds intermediate heads without source/model work, then acquires and prepares only the final revision once; live/native acceptance remains outstanding |
| 23 | Quiet hours/snooze/grouping preserve inbox state | Local SQLite/CLI checks cover grouping, quiet-hour/DST digests, restart deduplication, reconnect summaries and nonfatal desktop failures; personal hold checks cover durable duration/head snoozes and project pause/resume; native macOS adapter observation is separate; Linux desktop acceptance remains outstanding |
| 24 | Concurrent clients preserve scheduler invariants | Partial: two SQLite clients, exclusive watcher lease and shared model slots verified across processes, including lowered caps and process exit; cancellation versus preparation has one transactional winner; local scheduler admission exercises fresh-request preference and older optional progress through repeated refills/restarts; native Linux automated runtime passed in the delivered CI; the native Linux terminal artifact observation below covers basic interaction/restoration; Linux graphical UX remains outstanding |
| 25 | Model and effort reach actual Codex invocation | Partial: subprocess wire tests, opt-in live fixture and an actual primary/independent leaf acceptance using gpt-6.1-sol/high; the leaf evidence capability and distinct sessions were exercised |
| 26 | Worker cannot bypass publication policy | Partial: sealed broker, restricted runtime and excluded credentials; publisher remains an application-only capability |
| 27 | Owned snapshots/process trees clean up safely | Partial: runtime checks, native bare-source success/failure cleanup and private acceptance; cross-client cancellation journeys remove actual immutable snapshots; controlled native macOS cancellation of an actual Codex turn retains identity and removes owned runtime/source state; local owner/resource recovery also covers loss, ambiguity, rollback and bounded progress; native actual-model owner-exit recovery is separately verified below; worker SIGKILL/power-loss and hands-on Linux owner-recovery/graphical acceptance are not established |
| 28 | TUI resize/keyboard/native selection/restoration | Partial: optional attachment, Unicode/cell layout and exact-preview checks; native macOS PTY journeys verify confirmed overrides, dismissal/interrupt, resizing, restored operational terminal modes and no mouse-reporting activation; native macOS clarification editor also verifies typed secret-masked answers, dismissal, stale-target rejection and interruption; ordinary unmodified header drag selection passed in macOS Terminal using the delivered binary; native Linux basic terminal interaction/restoration is verified below; graphical selection remains unverified |
| 29 | Headless and JSON expose the same workflow states | Partial: keyboard attachment reuses ordinary commands for decisions, controls, cancellation and publication; headless/plain/JSON state checks and redirected-output fallback exercised; local clarification plain/JSON answering and secret-masked terminal editor share the exact input ID; idle watcher/inbox detachment preserves ownership and pending decisions as detailed below; remaining live/Linux boundaries stay separate |
| 30 | Platform builds and native runtime match claims | Delivered dd4d595 passed four native quality and four packaging jobs, including installed-host capability/clarification checks and native CLI/PTY tests; four hosted artifacts verified; remaining manual platform boundaries are stated below |

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
not a guessed request layout or history-text search. The bounded actual-model
owner-exit and macOS desktop-adapter observations below do not establish
power-loss recovery, pixel visibility or hands-on Linux UX. Local
[private-history retention](RETENTION.md) has separate verification boundaries;
it does not identify or reclaim crash-left processes or storage.
Native macOS PTY input-editor journeys exercise explicit answers, dismissal,
interruption, stale-target rejection, resizing and terminal restoration; the
separate native Terminal observation below proves the tested unmodified header
drag, not every emulator or hands-on Linux behavior.

## Local private-owner recovery boundary

The [owner/resource recovery](RECOVERY.md) addition covers manual and scheduled
private scopes. Local application/SQLite fixtures exercise owner-process exit,
live-child/ambiguous-launch preservation, rollback, concurrent clients,
interrupted cleanup, replaced storage/leases and bounded scan progress. These
do not establish live GitHub publication or arbitrary hard-crash behavior.
The separate actual-model owner-exit observation below covers one native macOS
termination path. Hosted native Linux automated checks are distinct from manual UX.

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

## Native macOS observations at the delivered revision

A real authenticated `gpt-6.1-sol`/high clarification fixture reached durable
pending input. Another ordinary CLI preserved the live owner. The application
owner then exited abruptly with `os.Exit`, bypassing Go defers; its App Server
received EOF. Offline `recover --dry-run` left state/storage intact; ordinary
`recover` failed the interrupted run, closed the pending input, retained its
run/thread/turn identity and reclaimed the owned private root. Repetition left
the updated timestamp and run count unchanged and performed no model or GitHub work.
This used a test-only overlay and owned Git/HTTP, not product source changes.
It proves that owner-exit/EOF path, not worker SIGKILL, power loss or every
process-tree failure mode.

A downloaded darwin/arm64 artifact ran `inbox --interactive` in macOS Terminal.
Native left-button down/drag/up events with all modifier flags zero selected
the expected actual inbox header, confirmed through native selected-text state.
Only the owned window/process/files were removed; two other Terminal windows
remained. This is the tested header/Terminal configuration, not every emulator
or Linux environment.

The real macOS desktop sink dispatched one uniquely titled benign test message.
Notification Center then exposed matching accessibility content with nonzero
geometry. No OS notification settings or GitHub state changed. This proves
adapter dispatch and native AX surfacing, not screenshot/pixel visibility,
human readability, scheduler-to-popup composition, or a Linux desktop. No
owner-scoped dismissal was available; the benign entry may remain in OS history.
The test binary, private fixtures and Go caches were removed. Independent
read-only audits accepted each of these three bounded observations separately.

A separate native macOS PTY journey covers idle watcher ownership. With one
pending ask item and a durable cooldown preventing remote/model admission,
attached `watch` held its kernel lease before `q` or Ctrl-C; command exit
released the lease and restored operational terminal modes. Detaching a separate
`inbox --interactive` left its headless watcher process alive and lease held.
Explicit interruption then stopped that owned watcher. The item stayed pending
with no decision or run. This proves idle exit/ownership and decision
preservation, not graceful joining during active model work or resize geometry.

## Native Linux terminal artifact observation

The downloaded `dd4d595` linux/arm64 binary ran in an isolated arm64 Linux
container on the local Linux VM, without network, provider credentials or a
host checkout mount. At 74 columns by 18 rows, the actual interactive inbox
showed its categories and private/publication distinction. Explicit help,
Escape/back, `q` and Ctrl-C were exercised; exit codes were 0/130 and Linux
termios matched before/after both exits. This was an empty local inbox, not a
model/publication journey or desktop session. It does not establish native
Linux graphical drag selection, desktop notification delivery or every distro.
The owned container, newly pulled image and fixture files were removed; existing
images and containers were retained.
