# Configuration

Operator TOML uses `schema_version = 1`. `reviewd init` prints its selected path.
The default is the OS user-config directory plus `reviewd/config.toml`; named
profiles use `reviewd/profiles/<name>/config.toml`. `--config` or `REVIEWD_CONFIG`
selects an explicit file. Reload retains the last valid policy after an error;
a changed state directory requires a restart. Reloaded execution settings apply
to later job pickups. Picking up a job freezes its reviewd operator settings,
even while it prepares GitHub data; its timeout includes that preparation.
Busy-PR discovered work returns queued and selects settings again on its next
pickup. Normal Codex configuration, skills and MCP/tool access remain inherited,
not copied or sealed. Running skills finish unless explicitly cancelled, timed
out or stopped with the watcher.

Resolution is built-in defaults → global settings → target-base `.reviewd`
preferences → exact-project settings → one-run CLI overrides. Lists replace,
never concatenate. Project identities are exact `owner/repo` or
`host/owner/repo`, canonicalized case-insensitively. See
[the complete example](../examples/config.toml).

| Settings | Meaning |
| --- | --- |
| `github.host`, `github.repositories` | Account host and explicit monitoring scope |
| `github.watch_review_requests`, `github.watch_own_prs` | Request discovery and notification-only own-PR monitoring |
| `reviewer.exclude_authors` | PR authors excluded from discovered review work; defaults to `["@me"]` |
| `reviewer.skill`, `reviewer.model`, `reviewer.reasoning_effort` | Review launch selection; defaults `review`, `gpt-6.1-sol`, `high` |
| `reviewer.executable` | Codex executable, globally selected |
| `feedback.authors`, `feedback.action` | Independent feedback subscription and `ignore`, `notify`, `ask`, or `auto` routing |
| `feedback.skill`, `feedback.model`, `feedback.reasoning_effort` | Handler selection; defaults `handle-pr-feedback`, `gpt-6.1-sol`, `high` |
| `review.instructions`, `feedback.instructions` | Additional prompt text; feedback uses its own nonempty instructions or the common review instructions |
| `agent.working_directory` | Optional absolute operator-selected working root; otherwise inherit reviewd's current directory |
| `actions.*` | `new_pull_request`, `review_requested`, `head_updated`, `review_submitted`, `review_comment_added` routing |
| `watch.poll_interval`, `watch.debounce`, `watch.include_drafts` | Polling, update coalescing, and draft eligibility |
| `limits.max_concurrent_reviews` | Shared per-profile capacity for both lanes, 1–16 |
| `limits.review_timeout`, `limits.feedback_timeout` | Lane-specific process time limits |
| `notifications.desktop`, `notifications.terminal` | Best-effort attention channels |
| `notifications.group_interval` | Grouping window; defaults to 5 seconds |
| `notifications.quiet_start`, `quiet_end`, `timezone` | Local quiet hours and IANA timezone; deferred attention becomes a digest |
| `state.directory`, `state.retention_days` | Local profile storage and terminal output retention |

`@me` uses authenticated GitHub account identity, so a login rename does not
change self-exclusion. Other entries are exact logins, including bot logins.
An empty author list replaces the inherited list. Feedback authors select the
**PR author**, not the person leaving comments. Setting `feedback.authors` or
`feedback.action` explicitly enables the independent lane; choosing only a
handler model does not subscribe it.

`ignore` suppresses an event, `notify` leaves attention, `ask` waits for `start`,
and `auto` queues it. The first observed backlog remains held for an explicit
choice. Notifications and elapsed time never choose an action. Legacy incoming
feedback actions do not silently opt in to automatic feedback handling.

## Repository preferences

A repository `.reviewd` keeps its separate `schema_version = 1` and may set only
`reviewer.reasoning_effort` and `review.instructions`. PR jobs read it from the
exact target-base commit, not a working tree or proposed PR head. Project
sections in the operator config can override it. It cannot choose accounts,
automation policy, executables or local paths.

```toml
schema_version = 1
[reviewer]
reasoning_effort = "high"
[review]
instructions = "Pay particular attention to backwards compatibility."
```

`config explain owner/repo` uses the default branch and shows provenance;
`--offline` explains operator-only settings explicitly. A particular PR may have
a different target base. Neither command starts an agent.

## File locations and profiles

Configuration selection is `--config` → `REVIEWD_CONFIG` → the OS default.
An explicit path takes precedence over `--profile`; it still must be a valid
profile name. `~/` is expanded and relative paths resolve from the current
directory. Linux `XDG_CONFIG_HOME`, when set, must be absolute.

| Location | macOS | Linux |
| --- | --- | --- |
| Default config | `~/Library/Application Support/reviewd/config.toml` | `${XDG_CONFIG_HOME:-~/.config}/reviewd/config.toml` |
| Named config | `~/Library/Application Support/reviewd/profiles/<name>/config.toml` | `${XDG_CONFIG_HOME:-~/.config}/reviewd/profiles/<name>/config.toml` |
| State root | `~/Library/Application Support` | `${XDG_STATE_HOME:-~/.local/state}` |

Default profile state is `<state-root>/reviewd/profiles/<id>`, containing
`reviewd.sqlite` and local scheduler locks (`watch.lock`, `review-admission.lock`,
`review-slot-NN.lock`). `<id>` is the first eight bytes of SHA-256 of the
canonical absolute config path, encoded as 16 hexadecimal characters. Symlinks
and the nearest existing ancestor are resolved even before `init` creates a
missing file. Editing config contents does not change the state identity;
moving the file can select a different inbox. `state.directory` overrides this
profile directory and must be a private directory, not a symlink.

macOS ignores `XDG_STATE_HOME`; Linux rejects a relative value. Shared per-PR
execution leases live in `<state-root>/reviewd/feedback-modifiers` with ownership
sidecars. They deliberately remain outside profile/state-directory overrides,
so separate profiles cannot run overlapping review/feedback skills for one PR.
Do not delete locks or uncertain history to retry an interrupted skill; inspect
the recorded session and use the explicit recovery/start commands.

Completed, owner-closed native output expires after `state.retention_days`
(default 30), including the latest activity preview. Identity and deduplication
facts remain. Failed/interrupted and uncertain historical output are retained.
