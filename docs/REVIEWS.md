# Private reviews and exact previews

The development version currently supports manual private preparation, a
durable run inbox and exact preview. Explicit publication and reconciliation
are described in [publication and recovery](PUBLICATION.md). The automatic
watcher is described in [foreground watching](WATCHING.md); notifications and
terminal UI are not available yet. A prepared review is not
a posted review or formal GitHub approval.

## Commands

```sh
reviewd review https://github.com/owner/repo/pull/17 --private
reviewd review https://github.com/owner/repo/pull/17 --private --now
reviewd review https://github.com/owner/repo/pull/17 --private \
  --model gpt-6.1-sol --reasoning-effort high
reviewd inbox
reviewd inbox --json
reviewd show <run-id>
reviewd show <run-id> --json
reviewd refresh <run-id>
reviewd refresh <run-id> --json
```

The explicit URL selects one PR on the configured host; it does not add the
repository to the monitoring allowlist. Known `/files` or `/commits` views and
fragments are canonicalized. Credentials, query parameters, ports, HTTP URLs
and another host are rejected before authenticated acquisition.

The development command requires `--private`. Preparation never posts to
GitHub, even if the project is configured for automatic publication. That flag
suppresses automatic publication; the `publish` command requires
a fresh explicit decision for the exact prepared payload, subject to
the operator's publication policy.

Pending mandatory checks leave durable `waiting_checks` work without starting
a model turn. Item-linked watcher runs resume when checks settle. Standalone
manual URL reviews are not automatically adopted by the watcher;
invoke the explicit review again after checks settle, or use `--now` to review
immediately while retaining the mandatory readiness gap. Failed, skipped,
missing and unavailable mandatory checks are never treated as passing.

## Evidence and trust

Preparation requires Git, authenticated GitHub CLI access and separately
installed authenticated Codex CLI 0.162.1. It captures repository/PR and posting
identity, complete comparison counts, target-base/head objects, conversation,
request timeline, resolved review-thread state and linked closing issues.
API pages and GraphQL cursors are traversed with explicit completeness checks;
unavailable context is retained as a gap, not an empty successful read.

Git acquisition uses disposable bare storage and complete history. It never
checks out or executes PR code, repository hooks, installers, builds or tests.
Ambient Git configuration, helpers, proxies and SSH authority are excluded.
The ordinary checkout is untouched. Repository source is not fetched from a
PR-provided clone URL.

Three revisions have distinct meanings:

- **Target base:** trusted `.reviewd` preferences and repository instructions.
- **Comparison base:** the unique merge base used for the complete diff.
- **Head:** the exact reviewed commit, later required for submission.

The target-base `.reviewd` must be a regular file and use the narrow preference
whitelist. A proposed head change cannot select a skill/tool or grant execution
or publication authority. Vendor/generated files, renames, deletions and
symlink content remain accounted for. Symlink targets are not followed.

Text blobs have an 8 MiB per-object limit and the distinct text collection a
256 MiB limit. Manifest/diff limits are explicit acquisition failures rather
than silent truncation. Changed binary/non-UTF-8 content, unsupported reader
paths, submodules and external LFS content remain explicit source limitations;
they cannot become a clean approval. Source manifests and large individual
lines are paged until the broker's explicit end marker.

`refresh` revalidates a prepared review's identity, source boundary, relevant
conversation/requirements and complete mandatory definitions, then updates its
checks without another model turn. Changed source/context/definitions require a
fresh review. A changed preview revokes earlier publication decisions, including
when checks later return to an earlier payload. A failed refresh does not certify
the stored preview as current; it never posts to GitHub.

Model check observations are explicitly identified and superseded by current
application-owned check evidence. Source defects and unrelated readiness/context
gaps and inconclusive source/context opinions are retained, including uncertainty
expressed in the summary. CI-only readiness uses identified check observations,
not an inconclusive source opinion. Summaries contain source/context facts rather
than frozen CI status prose.

Required checks are resolved from every page of active branch rules and classic protection,
then matched to the exact head and required integration. Legacy statuses are
read from the immutable-head endpoint. When a required name has both a check
run and a legacy status, both remain visible and must pass; a failed component
cannot hide a pending sibling. An aggregate status is not a substitute. Inaccessible protection and unresolved required reusable
workflow identities are explicit readiness gaps. The current adapter does not
verify reusable-workflow source identity; it reports these requirements as
unavailable and cannot turn them into a clean approval.

## Selected skill and worker

The actual selected installed skill is frozen privately with its references,
helper text and explicitly linked sibling instruction dependencies. Its digest
is retained in the run. This never copies the operator's private skill tree
into the public repository. Missing, unreadable, oversized or symlink reference
resources fail usefully instead of selecting a generic reviewer.

The worker uses the sealed App Server adapter and only application-granted
immutable evidence. GitHub credentials and writable connectors are absent;
shell, image/browser access and native agent spawning remain disabled.
When permitted by the selected skill, the primary can obtain at most one
application-owned independent specialist report. The leaf has the same sealed
evidence and configured model, explicit high/xhigh effort, no publisher and no
recursive delegation. Primary model/effort is never silently substituted.

Source findings, mandatory readiness gaps and missing context remain distinct.
The application validates structured results and canonical diff anchors, owns
publication formatting and separates internal opinion from proposed GitHub
event. A self-review proposes `COMMENT`, never self-approval or self-request-
changes. Credential-shaped or unsafe publication text is rejected, not posted.

## Durable state and privacy

The selected profile stores `reviewd.sqlite` in its documented state directory.
Directories must be private (0700), and the database a private regular file
(0600); symlink database paths are rejected. SQLite transactions and uniqueness
constraints retain queued/waiting, reviewing, prepared and failed/cancelled
states across independent clients. Exact-payload publication claims and unknown
outcomes are distinct stored states. Explicit publication persists attempts
before writes and verifies remote identities; real GitHub acceptance remains
outstanding.

A run retains frozen configuration/provenance, relevant context/skill identity,
review revisions, exact preview, available model/usage and thread/turn metadata.
Temporary Git objects, frozen instruction bundles and worker processes are
removed when shutdown is proven; uncertain children retain their owned storage.
Local [retention](RETENTION.md) expires eligible private text, while
[owner/resource recovery](RECOVERY.md) classifies interrupted work without
resuming an old thread or replaying an answer. Stored thread IDs alone never
prove liveness or resumability. These additions follow the older `f73eab6` snapshot.

Plain and JSON commands describe the same durable runs. JSON schema version 1
is a presentation API, not an import format for granting execution authority.
Known credential-shaped strings are redacted with an explicit `redacted` flag;
the private stored configuration and payload identities are not rewritten.
`show` distinguishes the proposed body/comments/event from actual publication
state. Errors and interrupted workers do not become clean reviews.
