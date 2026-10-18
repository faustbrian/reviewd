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

## Start

Install `gh` and your chosen Codex CLI or Claude Code, authenticate them normally,
and install your chosen
review and feedback skills. Build with Go 1.26 or use a native package produced
by this repository's CI. Supported targets are macOS/Linux, amd64/arm64. See
[archive installation](docs/DISTRIBUTION.md) and
[Codex capabilities](docs/CODEX.md) and [Claude Code support](docs/CLAUDE.md).

```sh
go install github.com/faustbrian/reviewd/cmd/reviewd@main
reviewd init
# Edit the reported config path: select repositories and action policies.
reviewd doctor
reviewd sync
reviewd inbox
reviewd watch
```

`watch` offers a keyboard inbox on an interactive terminal. Use
`watch --headless` for foreground monitoring without the attachment, or
`watch --headless --once --json` for one poll and its eligible work. A service
manager is optional; there is no required launchd or systemd installation.

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
