# Active private reviews

Session progress is implemented in source after `f73eab6`. Binaries at that
older revision do not include the later watcher, terminal or progress work.

```sh
reviewd show <run-id>
reviewd show <run-id> --json
reviewd inbox --interactive
reviewd cancel <run-id>
```

Manual and scheduled reviews record the primary Codex thread as soon as it is
known, then the active turn before processing streamed events. Another client
can inspect those identities while the review is still running. Details show
the requested and reported effective model, the latest available token usage,
and an activity such as reviewing, reading evidence, thinking or delegating.
Usage is a report from Codex, not a cost estimate or a completeness guarantee.

Activity is deliberately coarse. It does not store reasoning, streamed answer
text, tool arguments or source content. Repeated identical activity, model and
usage observations do not cause additional progress writes. A specialist does
not replace the primary thread identity; its sealed result and identifiers
remain separate in the completed worker metadata.

Progress never grants publication or changes the run lifecycle. If progress
cannot be persisted, the private worker stops instead of preparing a result
with unrecorded execution facts. Failed and cancelled runs retain the identity
already acquired. The ordinary commands, headless watcher and terminal details
read the same durable record.

This is not session reconnection. Review threads
remain private and ephemeral; a recorded identifier alone does not establish
that a process is alive or resumable. Durable textual clarification is described
below; [owner/resource recovery](RECOVERY.md) separately classifies interrupted
private work and preserves ambiguous resources. Execution, write and permission-expansion requests stay
declined, regardless of a model's wording.

## Explicit textual clarification

A private primary reviewer or the sole permitted specialist can request one
to three questions using the application-owned `reviewd_request_input` tool.
The tested Codex Default-mode host does not offer native `request_user_input`;
reviewd does not switch the review to Plan mode. Native input requests are
handled if a compatible host actually emits them, but native availability is
not claimed. Execution, write, network, connector and publication capabilities
remain denied; a textual answer never changes those grants.

```sh
reviewd inputs                 # private questions across this profile
reviewd inputs <run-id> --json
reviewd answer <input-id> --from-file answers.json
# Or supply the same JSON on stdin:
reviewd answer <input-id> --from-file -
```

The answer file contains exactly one answer for every question ID, with no
extra, duplicate or case-aliased schema keys. Schema names are exact (for
example, `answers`, not `Answers`); question IDs remain case-sensitive. For example:

```json
{"answers":{"scope":{"answers":["Billing"]}}}
```

Use a private file or stdin rather than putting answers in shell arguments.
The terminal inbox has a separate running row for each pending request; `i`
opens its captured question set, Enter advances, and a final `y` explicitly
submits. Secret questions mask their editor text. Escape, closing the terminal,
notification expiry and elapsed time never answer a question. Use the ordinary
command for multiline answers or more extensive editing.

Questions and delivery facts are durable. `show`, JSON inbox, the private
`inputs` command and terminal details expose the same opaque application input
ID, run, participant, thread and turn. Even when a host reuses its wire request
ID, an old input ID cannot select an answer for a different question. Concurrent
answers have one transactional winner. No selected answer is replayed by a new
review session.

A local selection, an attempted send, a completed transport write and a server
clear are different facts. `selected_at`, `written_at` and `closed_at` retain
those distinctions. A server clear can mean resolution or cancellation; it is
not proof of atomic remote acceptance. Lost or blocked writes retain an honest
`delivery_unknown` outcome. Dynamic calls are normally closed by turn lifecycle,
not by an invented server acknowledgement. A turn with an unresolved input
cannot become a clean prepared review. Cancellation and EOF stop waiting and
retain the private question history.

Limits are deliberately small: eight requests awaiting a completed reply write and at most 64 per
runtime, one to three unique question IDs, eight options per question, 512-byte
IDs, 128-byte headers, 8192-byte question/answer text, and 64 KiB JSON payloads.
Text is UTF-8 and limits are bytes; each question accepts exactly one nonblank
answer. Rejections never silently truncate or choose a default. The single-line
terminal editor rejects overlong additions; the ordinary command supports all
valid text. Answers are stored privately for the live owner and omitted from
ambient run JSON, progress, notifications and diagnostics. The model necessarily
receives the answer and can incorporate it into its prepared result; inspect
that exact private preview before granting publication.

An ephemeral dead session cannot be resumed merely because its thread ID was
saved. Owner-aware recovery closes inputs for positively identified interrupted
runs and does not replay answers. A pending record alone is not proof of a live
process; unknown legacy owners remain preserved. [Private-history retention](RETENTION.md) expires
eligible closed text without removing delivery facts. Inspect the run and
explicitly request a fresh review after a lost session.

Clarification persistence uses state schema 5; owner/resource recovery advances
the local schema to version 7 (including durable GitHub cooldowns). Older
binaries fail closed on this newer schema; they do not downgrade it. Preserve
or back up the private profile before experimenting with an older snapshot,
rather than replacing the database and losing decisions/publication history.

## Native watcher acceptance boundary

A controlled native macOS check uses the actual authorized Codex account and
`gpt-6.1-sol` with configured high effort through the default private worker.
The headless `watch --once` consumes an explicit start decision, exposes a
pending clarification and primary identity through independent plain/JSON CLI
commands, then consumes an unpredictable answer and prepares privately. A
second watcher invocation preserves the same run/thread/turn/payload, and the
closed private owner leaves no runtime/source/
frozen-skill storage behind.

Its Git repository, GitHub API and clarification skill are owned fixtures.
This is actual model/watcher composition, not live GitHub publication, installed
staff-review-skill quality, native Linux UX or actual-model hard-crash proof.
This acceptance does not establish hosted CI or artifact delivery for a later commit.
