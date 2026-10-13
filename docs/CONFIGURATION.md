# Configuration and setup

The configuration resolver and setup/diagnostic commands are implemented.
Watching, reviewing pull requests and publication are not available yet.
Examples describe the intended policies; configuring `auto` does not constitute
proof that the future scheduler or publisher implements them.

## Locations

On macOS, the default configuration is
`~/Library/Application Support/reviewd/config.toml`. On Linux it is
`$XDG_CONFIG_HOME/reviewd/config.toml`, or `~/.config/reviewd/config.toml`
when that environment variable is unset.

Use `--profile NAME` for `reviewd/profiles/NAME/config.toml` under the same
configuration root. An explicit `--config PATH` takes precedence over
`REVIEWD_CONFIG`, which takes precedence over the profile default. `~/` is
expanded; arbitrary environment variables are not interpolated. Profile names
are exact local identifiers, not repository names or patterns.

The default future state location is
`~/Library/Application Support/reviewd/profiles/ID` on macOS and
`$XDG_STATE_HOME/reviewd/profiles/ID` (or `~/.local/state/reviewd/profiles/ID`)
on Linux. ID is derived from the canonical configuration location, including
the nearest existing ancestor before initialization. Config content changes
and symlink aliases do not move the profile's inbox. `state.directory` selects
an explicit alternative. XDG directories must be absolute. No durable database
is initialized by the commands delivered in this batch.

## Initialize and diagnose

```sh
reviewd init
reviewd doctor
reviewd doctor --json
reviewd init --profile work --model gpt-6.1-sol --reasoning-effort high
```

Initialization creates the selected configuration exclusively with mode 0600,
creating missing parent directories with mode 0700. It never overwrites an
existing file or symlink. It does not modify repositories, Codex/GitHub
configuration or install a service. The initial repository allowlist is empty,
review requests require a decision, and publication requires a separate decision.
Explicit model/effort selections on `init` are persisted.

`init` checks prerequisites and shows the authenticated GitHub identity, policy
and corrective actions. Exit 0 means safe configuration creation succeeded;
`ready: false` can still indicate prerequisites need attention. `doctor` exits
1 for blocking failures and exposes them in its JSON report. Warnings, such as
an empty repository allowlist, are not authentication success or failure.

Both commands inspect account/model metadata without starting a review or paid
model turn. Account metadata is read without proactive token refresh in an
owned isolated Codex control directory. Metadata cannot guarantee that the provider
will accept a later actual review. Selected installed skill files must be
nonempty readable regular files of at most 1 MiB. Diagnostics do not certify
review quality or execute instructions.

Install GitHub CLI and authenticate with `gh auth login --hostname HOST`.
The authenticated `/user` response establishes the posting account, rather than
assuming GitHub CLI exit status implies valid auth. Tokens are captured in memory
and kept out of configuration, output, snapshots and worker environments.

Codex CLI **0.162.1** is the current tested compatibility baseline. Other
versions are rejected by diagnostics until their protocol/capability boundary
is verified. Use separately configured Codex file authentication (`auth.json`)
or supported provider API authentication. Keyring-only authentication is not
supported by the isolated adapter. The operator's Codex config is not inherited.

Install `requesting-code-review` in `$CODEX_HOME/skills` (default
`~/.codex/skills`) or `~/.agents/skills`, or configure `reviewer.skill_path`.
Missing skills or unavailable model/effort pairs produce actionable failures;
there is no generic reviewer or model fallback.

## Precedence and trust

Settings merge by field, lowest to highest:

1. Built-in defaults.
2. Operator global configuration.
3. Trusted base `.reviewd` preferences.
4. Operator exact-project overrides.
5. Command-line overrides.

Lists replace rather than append. Explicit false and valid zero durations are
retained. Unknown keys/tables, wrong types, unsupported schema versions and
invalid project settings reject the whole document. Repository identities are
canonicalized as lowercase `host/owner/repo`; one profile's allowlist must use
its configured GitHub host. Exact-project keys may omit the host. Wildcards
are unsupported; duplicate canonical identities are errors.

`.reviewd` may set only `schema_version`, `reviewer.reasoning_effort` and
`review.instructions`. It cannot select tools, credentials, model, skill, access
or automation policy. Proposed PR changes are not controlling instructions.
An absent file is normal; a present empty or malformed file is an error.

```sh
reviewd config explain shipitfi/api --json
reviewd config explain shipitfi/bill --offline
reviewd config explain shipitfi/api --reasoning-effort medium
```

Online explanation authenticates the host and reads preferences from a pinned
default-branch commit/tree/blob. Unavailable, incomplete or mismatched evidence
cannot be treated as an absent file. Default-branch explanation is not a PR
job's target-base snapshot. Actual review jobs must resolve against their exact
target-base SHA. Offline explanation explicitly reports `not_checked` and an
incomplete layer; it makes no GitHub or Codex calls.

Each effective field includes provenance. Immutable snapshots retain values,
provenance and a semantic fingerprint and can be serialized without losing
policy. Invalid manager reloads retain the previous fully valid document;
previously resolved snapshots do not change. Watcher reload wiring is still
outstanding.

Explanation masks credential-shaped strings in effective values/provenance.
When anything is masked, JSON includes `configuration.redacted: true`, retaining
the original configuration fingerprint for identification. Such presentation
output is explicitly incomplete and cannot be restored as a private job
snapshot. Actual internal snapshots remain faithful to the selected instructions.
The surrounding report metadata, including repository preference branch names,
shares the same credential-shaped-string boundary and marks `redacted: true`
at the report root when masked. It is not limited to effective field values.

## Schema 1 settings

See [the global example](../examples/config.toml) and
[the repository example](../examples/repository.reviewd). Only repository fields
listed above are permitted; all other preferences are operator controlled.

| Settings | Default / constraints |
| --- | --- |
| `github.host` | `github.com`; hostname, no scheme/port/credentials |
| `github.repositories` | Empty explicit allowlist; exact identities |
| `github.watch_review_requests`, `github.watch_own_prs` | `true` |
| `watch.poll_interval` | `60s`; 1s–24h |
| `watch.debounce` | `30s`; 0–1h |
| `watch.include_drafts` | `false` |
| `reviewer.model`, `reviewer.reasoning_effort` | `gpt-6.1-sol`, `high`; account-advertised pair required |
| `reviewer.skill`, `reviewer.skill_path` | `requesting-code-review`; optional explicit SKILL.md |
| `reviewer.executable`, `reviewer.auth_home` | `codex`; optional explicit auth donor directory |
| `limits.max_concurrent_reviews` | 2; 1–16 |
| `limits.review_timeout` | `30m`; 1m–24h |
| `actions.new_pull_request` | `ignore` |
| `actions.review_requested`, `actions.head_updated` | `ask` |
| `actions.review_submitted`, `actions.review_comment_added` | `notify` |
| Event action values | `ignore`, `notify`, `ask`, `auto` |
| `actions.publish_review` | `ask`; `never`, `ask`, `auto` |
| `notifications.desktop`, `notifications.terminal` | `true` |
| `notifications.quiet_start`, `notifications.quiet_end` | Empty; both HH:MM, different |
| `notifications.timezone` | `Local`; valid IANA timezone |
| `notifications.group_interval` | `5s`; 0–5m |
| `review.instructions` | Empty additional review guidance |
| `state.directory`, `state.retention_days` | Platform default; 30 days, 1–3650 |

Exact-project settings may override watch debounce/drafts, reviewer
model/effort/skill/path, review timeout, actions, notifications and instructions.
GitHub identity/scope, polling interval, executable/auth donor, global
concurrency and state settings belong to the global profile.

## Output and completion

JSON reports use `schema_version: 1` and are written to stdout without styling;
command/configuration failures normally use stderr. `doctor --json` keeps
blocking diagnostic failures inside its structured report. Exit codes: 0 for
successful commands, 1 for operational/diagnostic failure, 2 for invalid usage
or configuration, 130 for interruption. No elapsed time or notification expiry
can authorize an action.

Diagnostic presentation also masks credential-shaped strings in paths,
messages and scope metadata, including invalid-configuration failures; JSON
reports mark this with `redacted: true`. Interrupted diagnostics may retain
their structured report but return interruption status rather than an ordinary
prerequisite failure.

```sh
reviewd completion bash > reviewd.bash
reviewd completion zsh > _reviewd
reviewd completion fish > reviewd.fish
```

Completion is printed, not installed. Only macOS/Linux shells are supported.
