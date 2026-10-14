# Private-history retention

This addition follows the downloadable `f73eab6` snapshot and is absent from
that older binary. Snapshot capabilities are tied to their source commit.

`state.retention_days` controls expiry of private text in old, terminal reviews.
The default is 30 days; the supported range is 1–3650. A day is 24 hours, measured
from the run's last lifecycle update. A review exactly at the cutoff is not yet
expired. Retention is global for the selected profile, not a repository preference.

```sh
reviewd cleanup --dry-run
reviewd cleanup --json
reviewd show <run-id>
reviewd inputs <run-id>
```

The headless or attached watcher applies the same local operation at startup and
at most once an hour while running. Manual-only users can run `cleanup` directly.
Each pass inspects at most 25 candidates with a five-second database budget.
Repeat the command for a larger backlog; JSON reports eligible and actually
expired IDs separately, plus whether the candidate batch was full. A concurrent
client can make a candidate ineligible before the expiry transaction wins.

## What expires

For eligible terminal runs, cleanup removes the private prepared preview,
captured context and full frozen configuration, specialist task/report text,
and closed clarification questions and answers. Published review/comment bodies
are removed from the local receipt, not from GitHub.

The run remains inspectable as an archived summary. It retains repository/PR,
posting identity, logical work key, state, original lifecycle times, failure and
cancellation/supersession facts, primary/specialist session identities, available usage,
recorded source boundary, and model/effort identity where available. A captured
source boundary from a failed worker is not labelled a completed review.
Clarification request/participant identities and selection/write/closure facts
remain; their text is marked expired.

Compact publication receipts keep verified status, reviewed/current revisions,
application-recorded review/comment/repair IDs and URLs, and whether an existing
review was adopted. This keeps `open --conversation` useful and prevents expiry
from rediscovering reviewd's own publication as new feedback. The fact that a
valid preview established implicit following also survives; explicit unfollow
still takes precedence.

An archived preview cannot be published, repaired, refreshed, or reused as current
review evidence. Its recorded conversation can be opened, or a fresh review can
be started. Expiry never creates a model run or grants any publication decision.
Historical notifications are not replayed merely because private text expired.

## What remains unresolved

Queued, waiting, reviewing, prepared, publishing, partial, and unknown-publication
runs do not expire. Neither do terminal runs with an unclosed input request, an
unsettled publication attempt, or a pending repair. Verified published/stale
results can expire their private text while retaining the remote receipt.
Invalid private previews or unparseable publication evidence are retained and
reported, not guessed at.

Expiry is atomic per run across its preview and clarification text. Failure leaves
that run's evidence unchanged; earlier successful runs in a batch stay expired.
The operation retains work keys, so repeat observations do not duplicate old work.
It does not move lifecycle timestamps or turn a stale result into a current one.

This is logical deletion, not a secure-erasure promise: SQLite pages, WAL, backups
and retained summaries are not wiped. Cleanup does not delete the profile or
touch unrelated files. Retention itself does not identify or reclaim crash-left
processes/source/runtime directories. The separate local
[owner/resource recovery](RECOVERY.md) performs bounded ownership and shutdown
checks; it does not resume threads or signal stored process numbers.
