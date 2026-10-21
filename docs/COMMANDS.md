# Commands and workflows

- `init`: create a private operator config without overwriting an existing file.
- `doctor`: inspect configuration, GitHub account/repository access, storage and
  the selected native executables. It does not start a model, probe MCP tools or certify
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
  `--backend`, `--executable`, `--model` and `--reasoning-effort` choose
  one-run settings for its lane.
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
feedback commands accept one-run backend/executable/model/effort overrides. Normal native agent settings
own any subsequent tools, MCP access, permissions, hooks and skill workflows.
If a skill needs human continuation, inspect `show` and use the recorded session
with the selected CLI's own continuation interface; reviewd has no private answer broker.

## Keyboard inbox

The aligned table separates review/feedback lanes from execution state and shows
PR authors from captured GitHub data. At normal widths it includes repository,
PR and title; Ctrl-V selects full columns with activity and its age. Ctrl-O
switches Compact/Comfortable row density independently: Comfortable gives long
titles and activity two lines. The status area shows both choices. Compact columns keep
identity visible in narrow terminals. Selected-item status and scrollable details
retain the remaining information.

- Search is focused on startup, as in cdx. Type across repository, PR number,
  author, title, lane, state and recorded activity; search keys never dispatch
  work. Arrows/page keys browse without leaving search. Enter explicitly enables
  table action shortcuts; `/` returns to search. Ctrl-U clears, Escape clears
  search and enables table actions.
- Tab/Shift-Tab cycles table, search, filter and sort controls. Left/right or
  Space changes the focused filter/sort. `0` all, `1` reviews, `2` feedback,
  `3` running **and queued**, `4` history; filters compose with search.
- `t` toggles queue order/recent activity; Ctrl-V toggles full columns where
  width permits. Queue/Recent is also visible in the top Sort control; Tab then
  left/right changes it without entering table action focus. Selection follows
  item identity across updates and sorting.
- Arrows, Home/End and PgUp/PgDn navigate; the wheel browses three entries per
  notch inside the table, or scrolls the open details/help/diagnostics/confirmation
  without changing its captured target. Enter opens details. `?` opens help,
  `l` cached discovery diagnostics and the monitoring log; Escape returns.
  Details/help/logs are scrollable.
- `s` starts the event's skill (`h` aliases it), `x` skips, `c` cancels,
  `a` pauses/resumes a project, `z` snoozes, `Z` holds until new commits,
  `u` unsnoozes, `f`/`F` follows/unfollows and `o` opens the PR.
  `m`/`e` edits model/effort in a start choice; `y` confirms the captured event,
  Escape/`n` dismisses. Refreshing never grants consent.

Slots are a point-in-time observation of existing execution leases. Queue
explanations report known backlog, pause, snooze, retry/cooldown, debounce,
discovery, capacity or PR-ownership facts; otherwise the reason remains unknown.
Activity age measures changes to the recorded preview, not discovery refreshes
or usage updates. A completed tool event is not a completed review. Older runs
without retained identity, settings or activity clocks explicitly show unknown.
Details separate frozen accepted execution settings from prospective operator
settings; target-base repository preferences are resolved at pickup.

`q` in table focus closes this attachment and joins its owned watcher, if any. A separate
headless watcher continues. Ctrl-C interrupts. Mouse capture starts enabled so
the wheel operates the inbox rather than terminal scrollback. F2 toggles capture
without changing work; turn it off for ordinary terminal text selection. The
Mouse status is shown at wide widths. Terminal mouse mode is restored on exit.
Redirected and `--json` output remain noninteractive.

There are no reviewd publish, repair, refresh-checks, feedback-resume, private
input-answer, metadata, label or issue-tracker workflow commands.
