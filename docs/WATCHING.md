# Foreground watching and durable decisions

```sh
reviewd watch --headless
reviewd watch --headless --once --json
reviewd inbox --json
reviewd start <item-id>
reviewd start <item-id> --now --model gpt-6.1-sol --reasoning-effort high
reviewd skip <item-id>
```

The foreground watcher uses the configured repository allowlist and existing
GitHub/Codex accounts. It installs no service. `--once` synchronizes once and
drains eligible work; the ordinary mode continues until interrupted or five
consecutive failed synchronizations. `watch` attaches the optional keyboard inbox
when input/output are terminals; redirected, JSON, `--once` and explicit
`--headless` operation do not attach it. `inbox --interactive` attaches without
starting another watcher. See [terminal interaction](TERMINAL.md).

Project pause/resume and item snooze preserve decisions while holding scheduled
work and notices; see [personal holds](CONTROLS.md). They do not cancel a running
private review or retract a claimed publication.

Desktop and terminal notices share durable grouping/deduplication and quiet-hour
digests. They never grant consent; see [notifications](NOTIFICATIONS.md).

Notify-only items stay attention-only. Ask decisions survive without a terminal.
The initial automatic backlog is held. `start` explicitly queues one current
event, including a held backlog item, for a private review. It does not start a
hidden watcher or authorize publication. `skip` applies to that event, not a
future request or source/context revision. One-run overrides and `--now` persist
with the start decision.

Use [run-scoped cancellation](CANCELLATION.md) to stop active private work from
another local client. A recorded request is distinct from the worker confirming
that it stopped. This control does not wait for the next GitHub poll.

One kernel-held watcher lease is permitted per state directory. Review model
turns started by the watcher and the manual CLI share process-safe concurrency
slots. Process exit releases leases without deleting lock files. Different
state directories are independent profiles; do not use them to bypass a desired
shared concurrency cap.

Explicit requests take priority over optional discovery; equally prioritized
work is ordered by its durable update time. Code/context events observe
`watch.debounce`. A fresh discovery snapshot is checked before queued dispatch.
Closure/exclusion stops queued work. Material source or controlling-context
changes invalidate the previous linked queued, waiting or prepared evidence and
revoke its publication decisions. Active workers receive a durable stop request
and become `stale` after exit, rather than reporting a user cancellation. A
pure same-head re-request is distinct work, not material invalidation; changes
accompanying that request still invalidate the previous result. An explicit
accepted user cancellation retains precedence. In-flight publication keeps its
actual outcome and is reconciled by the publication owner, never rewritten as
a private stale result. The review service also revalidates its immutable
boundary before preparation, including manual runs and waiting-run resumes.

Pending resolved mandatory checks create a durable waiting run without a model
turn. Subsequent polls wait without acquiring source again. When checks settle,
the same run re-establishes its exact source/context/skill inputs and uses its
frozen model/configuration. Changed inputs require reassessment rather than
silently reusing the old run. Prepared source reviews refresh checks without
another model turn; changed previews revoke old publication consent.

Automatic publication remains a separate application-owned operation. The
watcher displays the posting identity and project automation policy before
enabling it. `actions.publish_review = "auto"` may publish an eligible automatic
run through the publisher's exact-payload preflight/readback. Explicit `start`
decisions remain private. Ask/never publication policies retain the private
preview. Prepared and remotely verified published states remain distinct.
Automatic consent, current event/decision eligibility and its publication
attempt are claimed transactionally. A skip or replacement accepted before
that claim prevents the old automatic write. Once publication is in flight,
decision commands reject revocation; inspect its actual outcome instead.

Valid configuration reloads apply to new dispatches; active jobs keep their
captured inputs. Invalid reloads keep the last valid configuration and report
the error. A state-directory change requires restarting the intended profile.
Identity and current operator publication policy are rechecked before a write.

Acquisition failures before a model turn and prepared-review read/preflight
failures receive at most three consecutive attempts, with 30-second and
60-second delays and any longer GitHub retry interval respected. Prepared
retries retain the same source review and never repeat its model turn; deadlines
and exhausted budgets survive restart and discovery. Exhaustion stays visibly
failed while the prepared preview remains available for inspection or an explicit
publication decision. A successful complete operation clears consecutive read
failures. Failed model turns are not automatically paid for again.
Discovery reconnects back off up to
15 minutes (or a longer configured/server interval), then stop after five
consecutive failures. A successful synchronization resets that counter.

On restart, interrupted linked private runs become failed and retain available
thread/turn identity; they are not silently resumed or reviewed twice. Waiting
and prepared evidence are retained. Restart and successful synchronization
reconcile current linked inbox states with their recorded run outcomes, without
reviving skipped or replaced decisions. Publication attempts/unknown outcomes are
never reset to queued: inspect them with `show` and use `publish` for canonical
reconciliation without blindly reposting. See [publication](PUBLICATION.md).

This is the local scheduler/personal-workflow slice, not the complete product.
Admission prefers fresh explicit requests over head/context updates and optional
discovery. To prevent starvation, ordering compares durable arrival times with
a five-minute priority offset for updates and a ten-minute offset for optional
discovery. Older eligible work can therefore precede newer requests; restarts
do not reset its age. This is ordering, not a guaranteed wall-clock start time:
capacity, debounce, checks, retry deadlines and personal holds still apply.

Owned watcher acceptance covers a head-update burst coalescing to one final
source/model review and an explicit same-head re-request dispatching distinct
work without repeating it after restart. Unknown-publication reconciliation
remains explicit through `publish`, without blindly retrying a write. Native
automated macOS/Linux checks passed for delivered `dd4d595`; bounded manual
macOS observations and remaining platform gaps are in [acceptance](ACCEPTANCE.md).
Local private-text expiry is wired into the watcher; see [retention](RETENTION.md). Retention itself does not
identify or reclaim crash-left processes and storage.
The separate [owner/resource recovery](RECOVERY.md) runs at startup and while
waiting. It preserves live/unknown owners, closes positively identified lost
private work, and reclaims only safely stopped owned storage. It never resumes
an old thread or signals a saved process number.
Owned Git/HTTP/model fixtures do not prove live GitHub publication or native
terminal behavior.

## Durable GitHub cooldown

The scheduler retains the longest server retry deadline per canonical GitHub
host in the profile's SQLite database. It checks that deadline before discovery,
new admission and automatic publication, including after restart and in
`watch --once`. A rate-limited discovery pass stops before another repository
is acquired. Waiting does not consume another item attempt or move the deadline.
At expiry, ordinary discovery and eligible work resume; prepared reviews reuse
their source result rather than paying for another model turn.

Retry guidance includes `Retry-After` seconds/HTTP dates and an exhausted primary
limit's `X-RateLimit-Reset`. A 429 without usable guidance waits at least a minute;
ordinary permission-denied responses do not create a rate-limit hold. These rules
follow [GitHub's retry guidance](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#handle-rate-limit-errors-appropriately).
GraphQL rate-limit error envelopes can arrive with HTTP 200; their headers and
error classification retain the same cooldown rather than becoming incomplete
context gaps. Headerless secondary-limit messages use the one-minute fallback.

`inbox` reports active host deadlines; JSON adds `github_cooldowns` entries with
`host` and `until`. Local inspection, cancellation and personal controls remain
available, and the foreground watcher remains interruptible. Different hosts
and state profiles have independent constraints. Changing configuration or
recovering local work does not erase a saved deadline. Already admitted remote
operations are not atomically recalled; their completions retain factual state.
A throttled publication submission/readback remains outcome-unknown for explicit
reconciliation, never automatically reposted after the hold expires.

This is an automatic scheduler constraint, not a new HTTP-client gate. Explicit
network commands (`sync`, `review`, `refresh`, `publish`) remain operator actions;
they do not clear an existing automatic cooldown. Avoid invoking them repeatedly
while GitHub has asked the account to wait. The implementation upgrades SQLite
to schema 7, preserving existing work and publication history. Older binaries
reject the newer schema; do not delete state to downgrade.

Owned HTTP/SQLite acceptance is not live GitHub publication, native terminal
acceptance, or proof of hosted CI/artifact delivery for another source revision.
