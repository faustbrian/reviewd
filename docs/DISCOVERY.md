# Discovery and the durable inbox

```sh
reviewd sync
reviewd inbox
reviewd inbox --json
reviewd follow <item-id>
reviewd unfollow <item-id>
```

`sync` performs one read-only synchronization of the explicit configured
repository allowlist. It imports reviews owed and human feedback on your own PRs,
exhausts repository pagination, and checks requested-team membership including
child-team membership. Denied, incomplete, or stalled acquisition remains an
explicit failed discovery status. Cached work is retained; failure is not an
empty inbox.

If an absent PR is individually checked and is unexpectedly still open, that
snapshot is incomplete: synchronization fails and retains cached work for a
retry. Only verified closure or complete eligibility evidence retires work.

The first successful synchronization establishes a baseline. Existing automatic
review requests remain pending rather than starting the backlog. Repeated
observations retain the same item and decision. Following is a tracking preference,
not authorization to start held backlog work. Notification-only feedback
grants no code-fixing, publication, or thread-resolution authority.

Valid operator action changes are reconciled even when the PR event is unchanged:
`ignore` removes actionable work and `notify` makes it attention-only. The
original event trigger selects the applicable action; an old code-update event
does not fall back to request policy. Enabling `auto` does not itself make an
existing event fresh or release held backlog. Disabling request watching also
prevents request history from upgrading optional discovery; later enabling it
does not reinterpret already-observed requests as new events.
Following-only tracking does not hide an outstanding code/context event when
its policy changes to attention or a pending decision.

Discovery retains real request-event history separately from current reviewer
applicability, rather than deduplicating by head SHA alone. Unrelated team
activity or joining an already-requested team cannot turn historical requests
into fresh automatic authority. Eligibility changes and request removal do not
release held backlog work; a genuine new request or material code/context change
is required. REST request
metadata is classified through request history rather than counted again as
a changed requirement; complete evidence and publication validation remain
unchanged. GitHub documents these [request timeline event types](https://docs.github.com/en/rest/using-the-rest-api/issue-event-types#review_requested).
Code and controlling-context changes have distinct explanations. An explicit
new request may restore attention to an unfollowed PR. Followed PRs remain
observable after their original request disappears; closed or excluded PRs leave
the actionable inbox. A valid locally prepared review establishes default follow
tracking unless the operator has explicitly unfollowed that PR. Unfollowing
through either category immediately stops owed review work for that PR without
erasing independent incoming feedback. Complete snapshots retire categories
that are no longer eligible even when another category remains relevant.

Removing a repository from the validated allowlist deactivates its discovery
scope and removes its items from the actionable inbox, even if the next network
acquisition fails. Removing every repository has the same local effect before
reporting the missing configuration. Older outstanding polls cannot reactivate
excluded work: allowlist reconciliation and all scope revisions are captured
transactionally before authentication or other network acquisition begins.
Local follow commands cannot override that exclusion.
Historical review runs, previews, and publication facts are retained.

Own publication records are removed from event comparisons while external
replies and thread-state changes remain visible. Discovery-only HTTP conditional
reads reuse a bounded in-memory representation only after fresh authenticated
304 validation. Errors never fall back to cached success. Cache state is not
persistent; a new process begins with complete reads.

`sync` does not run a reviewer or send a notification. New automatic-policy
events are stored as queued and processed by `watch --headless`; see
[foreground watching](WATCHING.md). `start <item-id>` explicitly releases one
held event for a private review; `skip <item-id>` retains its event-scoped choice.
Standalone private URL review and exact-payload publication remain available.

SQLite schema 2 adds discovery observations, status, inbox items, and follow
preferences without replacing existing private previews or publication facts.
An older executable may refuse the newer schema; upgrade it rather than deleting
the profile. Schema-1 prepared-review payloads remain supported independently of
the database migration.

JSON retains the existing `schema_version` and `runs` fields and adds `items`
and `discovery`. Integer identities are preserved exactly. Plain and JSON output
both distinguish cached work, failed discovery, prepared runs, and actual
publication records.

Current evidence uses owned HTTP/SQLite fixtures, not live team membership or
GitHub discovery acceptance. The local scheduler, notifications and personal
holds have owned journey coverage. The [optional terminal inbox](TERMINAL.md)
uses the same application behavior. Owner-aware recovery is also wired;
[native acceptance](ACCEPTANCE.md) records the exercised environments and limits.
See [watching](WATCHING.md) for durable
server cooldown and retry behavior.
