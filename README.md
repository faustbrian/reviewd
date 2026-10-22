# reviewd

A personal GitHub inbox and trigger for Codex CLI and Claude Code on macOS and Linux.

Reviewd watches selected repositories and launches ordinary fresh native agent sessions:

```text
$review https://github.com/owner/repo/pull/17
$handle-pr-feedback https://github.com/owner/repo/pull/17
```

Your installed skills and prompts own the work. Agents inherit your normal native agent
configuration, authentication, MCP servers, plugins and tool permissions. Reviewd
has no Linear integration, review publisher, feedback-fixing engine, command
sandbox, specialist workflow or external delivery policy.

## Quickstart

Install Git, Go 1.26+, [just](https://github.com/casey/just), GitHub CLI (`gh`),
and your chosen Codex CLI or Claude Code. Authenticate GitHub with `gh auth login`,
authenticate the agent normally, and install your review and feedback skills.
Supported platforms are macOS and Linux on amd64/arm64.

```sh
git clone https://github.com/faustbrian/reviewd.git
cd reviewd
just install
export PATH="$HOME/.local/bin:$PATH"
reviewd init
```

`just install` installs into `~/.local/bin`; keep that directory on your shell's
`PATH`. Use `just install /path/to/bin` for another destination. For Claude
reviews, use `reviewd init --backend claude` instead. You can also install a
[published archive](docs/DISTRIBUTION.md) without Go or just.

Open the configuration file printed by `init`. Set the following, editing
existing sections instead of duplicating them. Replace `acme/api` with your
repository:

```toml
[github]
repositories = ["acme/api"]

[actions]
new_pull_request = "ask"
review_requested = "ask"
head_updated = "ask"

# Also offer to handle incoming feedback on your own PRs.
[feedback]
authors = ["@me"]
action = "ask"
```

For a Claude-only setup, add `backend = "claude"` to `[feedback]` as well;
review and feedback backends are independent.

Start monitoring:

```sh
reviewd doctor
reviewd watch
```

`watch` continuously polls GitHub and opens a searchable table inbox. It shows
PR authors, review/feedback lanes, execution states and recorded activity.
The table is focused initially: `s` queues the selected pending event with its
configured skill and `x` skips it immediately. `S`/`X` starts/skips the matching
pending group with one confirmation, including offscreen matches. Search and
filters constrain the group; new events cannot join an already opened choice.
Use `h` for advanced start options, `0`–`4` to filter, `t` to sort, Enter for
details and `?` for help. `/` enters search; Enter returns to table actions.
Ctrl-V toggles core/full columns and
Ctrl-O switches compact/comfortable rows. The wheel browses the table or scrolls open details;
F2 releases mouse capture for terminal text selection. Change the relevant `"ask"` policies to `"auto"` for automatic handling of new events,
or `"notify"` for notifications only. The first observed backlog stays held for an explicit
choice even with automatic policies. Skipping discards only that event, not
future reviews or feedback for the PR.

For monitoring without the keyboard inbox, keep this running in a terminal:

```sh
reviewd watch --headless
```

Both modes run in the foreground; Ctrl-C stops monitoring. `q` in table focus also closes the
interactive watcher. A service manager is optional, not installed by reviewd.
Use `reviewd sync` and `reviewd inbox` to inspect work without launching agents,
or `reviewd watch --headless --once --json` for one poll and its eligible work.

### Manual actions

```sh
reviewd review https://github.com/owner/repo/pull/17
reviewd feedback https://github.com/owner/repo/pull/17
reviewd review <pr-url> --backend claude --model sonnet --reasoning-effort high
reviewd start <item-id>
reviewd show <run-id>
```

Both lanes default to Codex (`gpt-6.1-sol`, `high`). Select Claude independently
with `reviewer.backend` or `feedback.backend`; Claude defaults to Opus 5.5
(`claude-opus-5-5`) with native effort unless explicitly configured. Review
discovery excludes `@me` by default. Feedback handling is independently opt-in and can select only
your own PRs, other named authors, or both. Manual URL commands are explicit
one-off actions and do not subscribe a repository or apply discovery filters.

Use exact-project sections to notify for `acme/api` and automate
`acme/bill`; see [the example](examples/config.toml).

## Development

Run `just` to list commands:

| Command | Purpose |
| --- | --- |
| `just install` | Build and install into `~/.local/bin` |
| `just build` | Build `./reviewd` |
| `just run --help` | Build and run the local binary with any CLI arguments |
| `just test` | Run all Go tests with race detection |
| `just test ./internal/config -run TestBackend` | Run focused Go tests |
| `just vet` | Run Go static analysis |
| `just test-scripts` | Test packaging/release scripts without publication |
| `just check` | Run the CI quality gate, including four-target builds |

Python 3 is required for `just test-scripts` and `just check`.

## Boundaries

A completed run means the selected native agent returned a successful result, not that reviewd verified a
GitHub review, commit, push, issue, or fix. Inspect the skill's output and recorded
native session. Reviewd never automatically resumes or replays an interrupted
agent. Skills can use their normal tools to perform their workflows.
Reviewd settings freeze when a job is picked up, including GitHub preparation;
reloads affect later pickups. Busy-PR discovered jobs return queued for a fresh
selection. Running skills keep working through GitHub changes and reloads; use
explicit cancellation, their timeout or watcher shutdown to stop them.

- [Configuration](docs/CONFIGURATION.md)
- [Commands and workflows](docs/COMMANDS.md)
- [Current specification](docs/SPECIFICATION.md)
- [Architecture and checks](docs/IMPLEMENTATION.md)
- [Packaging](docs/DISTRIBUTION.md)
- [Codex compatibility](docs/CODEX.md)
- [Claude Code compatibility](docs/CLAUDE.md)
- [Upgrade and already-pushed scope correction](docs/UPGRADING.md)
