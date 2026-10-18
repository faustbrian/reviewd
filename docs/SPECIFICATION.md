# Product specification

Reviewd is a personal Go CLI for macOS and Linux, amd64 and arm64. Each operator
uses their existing GitHub and Codex accounts. It is a trigger and inbox, not an
agent workflow implementation or shared bot service.

## Responsibilities

1. Poll an explicit repository scope for review requests, optional new PRs,
   followed PR updates and newly received feedback.
2. Exclude selected PR authors from the review lane, defaulting to the operator.
3. Independently select PR authors for opt-in feedback handling.
4. Resolve global and exact-project action/skill/prompt/model/effort settings,
   with limited target-base repository preferences.
5. Route events to ignore, notification, a manual start choice, or automatic
   dispatch. Hold initial backlog; deduplicate durable event decisions.
6. Launch a fresh ordinary Codex session with `$review <url>` or
   `$handle-pr-feedback <url>` (or configured replacement skill names).
7. Bound concurrent child processes, serialize both lanes for a PR across local
   profiles, coalesce later observations and report process outcomes.
8. Provide durable history, cancellation, notifications, personal holds,
   plain/JSON commands and an optional keyboard inbox.

## Ownership boundary

The selected agent/skill owns reviewing, feedback evaluation, fixes, tests,
independent engineering roles, commits/signing/hooks, pushes, GitHub reviews,
replies, re-review requests, thread resolution, metadata, labels and any Linear,
Better Stack or other MCP usage. Reviewd does not implement or enforce these
workflows, broker their tools, or certify their delivery.

The child inherits normal Codex configuration and permissions. Reviewd does not
create a sealed Codex home, strip credentials, disable MCP/plugins/multi-agent,
force a sandbox, or inject a private-only publication policy. Optional user
instructions and `review --private` are prompt text, not capability controls.

## Event and run behavior

Feedback identity follows other actors' substantive content, not the handler's
own replies, its push, automatic review dismissal, line movement or thread
resolution. Review identity likewise excludes the operator's own feedback, so a
posted review does not become a self-trigger loop. New head or feedback events
are coalesced while an agent is running; they do not interrupt its workflow.
Picking up a job freezes reviewd's operator configuration before GitHub
preparation, including its skill, model, effort, instructions and total timeout
budget. Reloads affect later pickups, not a job already preparing. Discovered
work that encounters a busy PR returns queued and selects current settings on
its next pickup. An explicit URL invocation keeps its selected one-off settings
while waiting. This does not freeze Codex configuration, installed skills,
MCP servers, tools or remote GitHub facts.

Current durable event and personal-control checks can still prevent unclaimed
work from starting. Once claimed, the skill's lifetime is independent of later
GitHub observations or configuration reloads: closure, disappearing feedback,
acquisition failures and account/scope/author policy changes do not cancel it.
Explicit cancel, its selected timeout, and watcher shutdown still stop it;
native process or local ownership/storage failures remain factual failures.

New runs have `queued`, `running`, `completed`, `failed`, `cancelled` or
`interrupted` process states. `completed` requires a successful Codex process
and native completed turn with a session identity. It does not mean published,
fixed, merged or deployed. A started job is never automatically replayed merely
because its final outcome was lost. New events remain distinct work; an explicit
manual URL command or start choice can intentionally launch another run.

Existing historical runs, outcomes, notification receipts and retained work
remain readable. Their old private-review/feedback-workflow decisions are not
silently converted into ordinary skill launch permission. See
[upgrading](UPGRADING.md) for the compatibility change.
