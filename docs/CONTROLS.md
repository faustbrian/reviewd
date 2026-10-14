# Personal holds

These commands accompany the foreground watcher. The older `f73eab6` CI
snapshots do not include them; snapshot capabilities follow their source commit.

```sh
reviewd pause shipitfi/api
reviewd resume shipitfi/api
reviewd snooze <item-id> --for 2h
reviewd snooze <item-id> --until-head
reviewd unsnooze <item-id>
reviewd inbox --json
```

## Project pause

Pause applies to the canonical host/owner/repository in the selected profile's
configured allowlist. It suspends scheduled admission, automatic publication
and notifications. Discovery continues, and inbox items, pending decisions,
prepared results and actual publication outcomes remain visible. Resume
returns to existing policy and decisions; it does not approve a pending ask or
release the initial automatic backlog. Repository preferences cannot change
these local holds.

A private review already running can finish preparation. A publication attempt
already claimed cannot be withdrawn; inspect its actual outcome. Manual
`review --private` and explicitly decided `publish` remain available while a
project is paused. `start` can retain an explicit private decision, but the
watcher waits until the project resumes.

## Item snooze

Snooze names exactly one inbox item/category. Snoozing reviews owed does not
snooze independent incoming feedback on the same PR. It preserves the underlying
state, event, decision and run instead of hiding them or turning them into a new
job. New discovery cannot overwrite a hold.

Choose exactly one of a positive `--for` duration or `--until-head`. Duration
holds end at their UTC deadline, including across restarts and newer events.
Head holds release permanently on the first observed different PR head, even if
the category is temporarily absent. A same-head re-request, title/body edit,
base/context change or policy reload does not release one. Reverting to a
previous head does not resurrect an old hold. `unsnooze` releases a named hold
without granting a start or publication decision. Explicit `start` or `skip`
clears that item's snooze, but not a project pause.

Snoozed scheduled work and automatic publication wait. A running private review
may finish preparation. Snooze rejects an already in-flight publication rather
than claiming to cancel it. Held notifications remain pending for release;
ordinary grouping, quiet hours and current channel settings still apply then.

## Persistence and claim boundaries

Plain inbox output labels retained holds; JSON adds a `controls` object with
`paused_projects` and active `snoozes`, without replacing item/run lifecycle
states. A hold accepted before a scheduled-work, automatic-publication or
notification claim prevents that claim. It cannot retract an external operation
already claimed or dispatched. Automatic runs retain their originating inbox-category identity after new events,
so their retained notices remain under that category's hold. Independent manual
reviews and other categories are not muted by it. OS delivery remains best-effort.

SQLite schema 4 adds the hold records and observed head identity. Upgrades are
transactional, and concurrent clients serialize schema-version inspection and
changes with SQLite's
[immediate transaction](https://www.sqlite.org/lang_transaction.html).
Historical runs, decisions, attempts and notifications are retained. Older
binaries reject the newer schema: upgrade reviewd rather than replacing the
database or attempting a downgrade. Profiles are independent state directories.

[Scoped cancellation](CANCELLATION.md) is a separate local command. The
[optional terminal inbox](TERMINAL.md) exposes the same holds and browser
navigation; holds never mean a remote review was removed.
