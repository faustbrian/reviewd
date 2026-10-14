# Command reference

`reviewd --help` and `reviewd <command> --help` describe the installed build.
Commands use the selected local profile; initialization does not start a
watcher, review or service. IDs come from `inbox`, `show` and `inputs` output.

## Shared options

| Option | Meaning |
| --- | --- |
| `--config <path>` | Select an explicit global TOML file. |
| `--profile <name>` | Select the named configuration profile; default is `default`. |
| `--json` | Use versioned machine-readable output without a terminal attachment. |
| `--model <id>` | Select a model for `review`/`start`, setup diagnostics or configuration explanation. |
| `--reasoning-effort <level>` | Select effort for the same operations; unsupported settings fail rather than silently falling back. |
| `--version` | Print the binary version. |

An explicit `state.directory` selects the durable state location even across
profile names: use distinct directories for genuinely separate inboxes. Model
options on unrelated controls/publication commands are rejected. `init` persists
explicit model/effort choices; `doctor` checks them without changing
configuration. A running review retains its captured settings; changing defaults
affects new work.

## Setup and configuration

| Command | Outcome |
| --- | --- |
| `init` | Create safe personal configuration and report prerequisites; preserve an existing file. |
| `doctor [--json]` | Check authentication metadata, prerequisites and selected review settings without reviewing a PR. |
| `config explain <owner/repo> [--offline]` | Explain effective values/provenance; offline omits trusted repository preferences and reports that limit. |
| `completion <bash\|zsh\|fish>` | Print completion without installing or modifying shell configuration. |

See [configuration](CONFIGURATION.md) for paths, examples, trust and permissions.

## Discovery, monitoring and inspection

| Command | Outcome |
| --- | --- |
| `sync` | Synchronize allowlisted GitHub work without starting a reviewer. |
| `watch` | Run the foreground watcher, attaching the keyboard inbox when both streams are terminals. |
| `watch --headless` | Run the same watcher without an attachment. |
| `watch --once --json` | Synchronize and drain currently eligible work, then exit; initial backlog remains held. |
| `inbox [--json]` | Read the current durable snapshot; no synchronization or watcher starts. |
| `inbox --interactive` | Attach a keyboard inbox without owning a watcher. |
| `show <run-id> [--json]` | Inspect retained state, current execution identities and exact prepared preview when available. |
| `open <item-or-run-id> [--print]` | Open the identified PR, or print its URL without launching a browser. |
| `open <run-id> --conversation [--print]` | Open/print a recorded review conversation on that PR; the link is not newly verified. |

Detaching `watch` stops its owned foreground watcher. Detaching a separately
attached `inbox` leaves an existing headless watcher running. See
[watching](WATCHING.md), [discovery](DISCOVERY.md) and [keyboard actions](TERMINAL.md).

## Decisions and private reviews

| Command | Outcome |
| --- | --- |
| `start <item-id> [--event <event-key>] [--now]` | Queue a private review of that inbox event; a watcher must process it. |
| `skip <item-id> [--event <event-key>]` | Skip that event, not future requests/revisions. |
| `review <pr-url> --private [--now]` | Prepare a private review directly without authorizing automatic publication. |
| `refresh <run-id>` | Refresh checks/disposition of a prepared source result without another source/model review. |
| `cancel <run-id>` | Request cancellation of eligible queued/waiting/active private work; acknowledgement is not proof that the worker has already exited. |
| `inputs [run-id] [--json]` | Inspect exact pending/retained clarification targets. |
| `answer <input-id> --from-file <path>` | Explicitly select a complete answer JSON file for that exact input. Use `-` for stdin. |

Use `--event` from the displayed inbox JSON when a script must reject a changed
event. `--now` proceeds before mandatory checks settle while retaining readiness
gaps. `--model`/`--reasoning-effort` apply to `review` or the individual `start`
decision, not the watcher as a whole. No answer grants execution, write or
publication permissions. Answer JSON and private-file guidance are in
[sessions](SESSIONS.md); review/cancellation details are in
[reviews](REVIEWS.md) and [cancellation](CANCELLATION.md).

## Publication

| Command | Outcome |
| --- | --- |
| `publish <run-id>` | Refresh/inspect publication state; omission of `--payload` grants no new publication decision. |
| `publish <run-id> --payload <exact-preview-id>` | Explicitly authorize that exact preview, subject to current policy/evidence validation and remote readback. |
| `publish <run-id> --payload <exact-preview-id> --repair` | Reconcile and explicitly repair one missing inline finding, not blindly repost a review. |

Inspect the body, event, findings and payload identity before selecting it.
Changed evidence requires a new decision. Repeating publication after an unknown
submission reconciles before any write; it does not grant a blind retry. A
self-review uses `COMMENT`, not formal GitHub approval. See
[publication and recovery](PUBLICATION.md).

## Personal controls and local maintenance

| Command | Outcome |
| --- | --- |
| `pause <owner/repo>` / `resume <owner/repo>` | Hold/release scheduled work, automatic publication and notifications under existing policies/decisions. |
| `snooze <item-id> --for 2h` | Hold one inbox category until the selected duration expires. |
| `snooze <item-id> --until-head` | Hold until new commits; same-head re-requests do not wake it. |
| `unsnooze <item-id>` | Release the hold without granting new execution/publication consent. |
| `follow <item-id>` / `unfollow <item-id>` | Continue/stop followed-PR updates; new explicit requests can still appear. |
| `recover [--dry-run]` | Classify private owners and reclaim only safely stopped interrupted resources; never resume an old thread or signal a saved PID. |
| `cleanup [--dry-run]` | Expire eligible terminal private text under retention policy, keeping protected work, summaries and publication facts. |

Pausing/snoozing does not cancel an already running review or retract a claimed
publication. Recovery and history expiry are different operations. See
[controls](CONTROLS.md), [owner recovery](RECOVERY.md) and [retention](RETENTION.md).

## Plain and JSON use

```sh
reviewd --config ~/.config/reviewd/config.toml inbox --json
reviewd show <run-id>
reviewd inputs <run-id> --json
reviewd start <item-id> --event <event-key> --model gpt-6.1-sol --reasoning-effort high
```

The explicit path is an example, not the macOS default; see platform paths in
[configuration](CONFIGURATION.md). Private output can contain source/review text;
protect files and avoid sharing it as telemetry. JSON carries `schema_version`;
diagnostics go to stderr. An operation can return nonzero while retaining useful
state/JSON, such as waiting checks, no publication decision or unknown resources.
Inspect the reported state rather than treating an exit code as publication.
Interruption returns 130. Use command-specific help and linked workflows for
blocking prerequisites and corrective actions.
