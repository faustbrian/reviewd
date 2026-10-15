# Explicit publication and recovery

Preparation and publication are separate operations. The worker cannot post;
the application owns GitHub credentials, payload decisions and readback.

```sh
reviewd publish <run-id>
reviewd show <run-id>
reviewd publish <run-id> --payload <exact-preview-id>
reviewd publish <run-id> --payload <exact-preview-id> --json
```

Omitting `--payload` refreshes the preview and exits without approval or a write.
Inspect the exact body, comments and proposed event before selecting its payload.
Prepared-result format 2 retains validation of format-1 private previews.
Refreshing an older preview reuses its source report and produces a new payload
identity; any old publication decision is invalidated.

Current operator `never` policy rejects new writes. A privately prepared run
cannot publish automatically, but can be published by this explicit decision
under `ask` or `auto`. The source-review configuration remains frozen.

Publication rechecks identity, eligibility, source/context, mandatory evidence,
anchors and remote history. Changed evidence invalidates the selected preview.
An existing inline finding becomes a conversation reference in a new preview,
which requires a new decision. A matching already-submitted review is read back,
not reposted. Self-reviews use GitHub `COMMENT`, separately from internal opinion.

`published` means canonical remote review/comment readback succeeded. `stale`
retains the actual reviewed revision when code or context changed; it does not
certify the new state. No atomic current-head or exactly-once remote guarantee
is claimed.

Lost responses and interrupted attempts remain `publication_unknown` until
GitHub readback establishes the outcome. Repeating `publish` reconciles an
existing attempt without sending another review. Unavailable readback preserves
earlier verified publication records rather than erasing remote identifiers.

For `partial`, inspect the recorded review and missing findings, then explicitly
repair one missing inline at a time:

```sh
reviewd publish <run-id> --payload <exact-preview-id> --repair
```

Each invocation revalidates current authority/evidence, transactionally records
one repair intent, and posts only the missing head-pinned finding. Already
verified findings are not reposted. A lost repair response is reconciled before
any further write; ambiguity stays unknown. An additional missing finding stays
partial and needs another explicit invocation. Changed evidence or unexpected
extra comments require inspection, not a blind repair.

Review-scoped GitHub comment lists may expose only legacy diff positions.
Readback therefore resolves each listed comment by its exact remote ID through
the canonical comment endpoint before validating identity and original anchors.
A missing or mismatched canonical response cannot authorize a repost.

A [controlled live check](ACCEPTANCE.md#controlled-live-github-publication)
posted one COMMENT review and one inline finding, then recovered its persisted
partial state and repeated verified readback without another write. Lost-response,
true-partial repair and changing-evidence paths have separate owned Git/HTTP
coverage; the live check does not claim those failures were induced on GitHub.
