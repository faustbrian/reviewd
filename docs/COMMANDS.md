# Commands and workflows

- `init`: create a private operator config without overwriting an existing file.
- `doctor`: inspect configuration, GitHub account/repository access, storage and
  the Codex executable. It does not start a model, probe MCP tools or certify
  skill delivery.
- `config explain <owner/repo> [--offline]`: settings and their provenance.
- `sync`: discover work without launching agents.
- `inbox [--interactive]`: cached work and history; JSON/redirected output stays
  plain regardless of the interactive flag.
- `watch [--headless] [--once]`: foreground discovery, notifications and dispatch.
  Continuous monitoring backs off through acquisition failures without stopping
  accepted skills. `--once` drains its accepted work before returning an error.
  Reviewd settings freeze at job pickup, including preparation; reloads affect
  later pickups, and busy-PR discovered jobs return queued for a fresh selection.
- `review <pr-url> [--private]`: a fresh selected review skill. `--private` adds a
  read-only/do-not-post prompt request; it is not a tool sandbox.
- `feedback <pr-url>`: a fresh selected feedback-handling skill.
- `start <item-id> [--event <event-key>]`: queue that event's configured skill.
  `--model` and `--reasoning-effort` choose one-run settings for its lane.
- `skip <item-id> [--event <event-key>]`: skip this event, not future feedback.
- `show <run-id>`: stored output, session identity and factual process state.
- `cancel <run-id>`: request stop; this cannot undo an agent's external effects.
- `pause <project>`, `resume <project>`: hold/release scheduled work and attention.
- `snooze <item-id> --for 2h` or `--until-head`; `unsnooze <item-id>`: personal
  category holds without losing newer observations.
- `follow <item-id>`, `unfollow <item-id>`: control follow-up review monitoring.
- `open <item-or-run-id> [--print]`: open or print the canonical PR URL.
  `--conversation` supports a recorded historical review URL when available.
- `cleanup [--dry-run]`: expire completed native output under the configured
  retention period, retaining run/session identity and all uncertain or legacy work.
- `recover [--dry-run]`: inspect interrupted local process ownership; never
  resume an agent or replay its remote actions.
- `completion bash|zsh|fish`: print shell completion, without installing it.

Global `--json`, `--config` and `--profile` work without a desktop. Review and
feedback commands accept one-run model/effort overrides. Normal Codex settings
own any subsequent tools, MCP access, permissions, hooks and skill workflows.
If a skill needs human continuation, inspect `show` and use the recorded session
with Codex's own continuation interface; reviewd has no private answer broker.

## Keyboard inbox

`0` all, `1` reviews owed, `2` feedback, `3` running, `4` history. Arrows select,
Enter opens details, `s` starts the event's skill (`h` aliases it), `x` skips,
`c` cancels, `a` pauses/resumes a project, `z` snoozes, `Z` holds until new commits,
`u` unsnoozes, `f`/`F` follow/unfollow, and `o` opens the PR. `m`/`e` edit model
and effort in a start choice; `y` confirms and Escape/`n` dismisses. `?` shows
help; `q` closes the attachment and its owned watcher. An independent headless
watcher continues. Ordinary mouse selection remains terminal-owned.

There are no reviewd publish, repair, refresh-checks, feedback-resume, private
input-answer, metadata, label or issue-tracker workflow commands.
