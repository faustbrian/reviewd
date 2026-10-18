# Upgrade and scope correction

Reviewd now launches ordinary skills. It no longer implements the review or
feedback workflows around them. This corrects already-pushed scope expansion;
it is not merely a change to uncommitted plans.

## Configuration and local history

The public operator format is `schema_version = 1`, as is the limited repository
preference format. Backend selection does not introduce a public v2 adoption
step. Archived run snapshots keep their recorded formats and
fingerprints independently of the live configuration and database counters.
Configure `auto` only when the selected ordinary skill should run with your
normal native agent configuration and tools. No config file is automatically rewritten.

Remove these settings: `reviewer.skill_path`, `reviewer.auth_home`,
`feedback.skill_path`, `actions.publish_review`, `feedback.container_endpoint`,
`feedback.container_image`, `feedback.metadata_validators`,
`feedback.metadata_profile`, and `feedback.label_catalog`. Configure skills,
MCP servers, tools and workflow rules in the native agent or the selected prompts instead.
The default review skill is now `review`, not `requesting-code-review`.

Repository `.reviewd` files retain their separate limited schema version 1.
The local database upgrades to schema 10. Historical run rows/outcomes remain
readable; old unstarted inbox decisions are held as `legacy_pending`, with their
former state retained, until an explicit new choice or genuinely new event.
Historical prepared reviews retain implicit follow monitoring; explicit unfollow
still takes precedence. This does not authorize reusing their old previews.
Interrupted or uncertain agents are not resumed/replayed. Keep old versions
stopped before upgrading; do not downgrade a schema-10 database.

The publish/repair, check-refresh, private answer-broker and feedback-resume
commands are retired. `show` retains historical output; use normal Codex
continuation for a native session. Older retained resource descriptions remain
recognizable so upgrade does not fabricate proof of shutdown or discard work.
There is no container prerequisite for new review or feedback agents.

Configuration reloads affect later pickups. A picked-up job keeps its selected
reviewd settings through GitHub preparation, before any durable run exists;
busy-PR discovered work returns queued for a fresh pickup. GitHub changes and
configuration reloads no longer cancel a running skill. Use `cancel` to stop a particular run; its
selected timeout and watcher shutdown still stop it. Historical/ineligible
inbox entries are not authority to interrupt a skill already working.

## Already-pushed behavior removed or reworked

| Earlier commits | Scope correction |
| --- | --- |
| `8ce17e5`, `b8a4fd4`, `d0fcfaa`, `3af74d5` | Remove sealed/private reviewer runtime, immutable preparation and app-owned publication/reconciliation |
| `dd4d595` | Retain scheduling/inbox/notifications; replace the embedded review/publication/specialist lifecycle with ordinary skill dispatch and separate accepted-run lifetime from inbox/discovery health |
| `e300f38`, `847f962`, `237b1d8`, `0919ee7` | Remove feedback broker, signing/workspace/fixing machinery; retain/rework per-PR process serialization only |
| `2cbc122`, `9f68afc` | Retain author filters and independent feedback discovery; replace bespoke workflow-consent UI with a normal skill start choice |
| `01e4957`, `08cde65`, `d733578`, `f4c8acd`, `758ce5d`, `beafa1e`, `8510438` | Remove sandbox/container command execution and helper packaging |
| `e6ae5f4`, `3177f05`, `e59a912`, `09ed067`, `a54e98e`, `726c8a8` | Remove app-owned feedback delivery, metadata/no-code/clarification/label/deferral enforcement |
| `f73eab6` | Replace derivative workflow tests/docs/packaging with coverage of the trigger boundary |

No standalone Linear API client had been pushed. An unfinished, uncommitted
tracker-configuration experiment was discarded. The removed behavior was
reviewd's attempt to own/enforce issue, label, metadata and delivery workflows;
those responsibilities belong to the user's skills, prompts and normal MCPs.

## Adding Claude Code

Existing configuration and stored Codex runs remain Codex; backend omission
never reinterprets their executable or session. Select Claude per lane/project
with the new backend settings described in [configuration](CONFIGURATION.md#native-agent-selection).
No public configuration version change or history rewrite is required. Reviewd
never resumes a Codex conversation in Claude or vice versa.
