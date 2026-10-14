# reviewd

A personal, local-first GitHub review inbox backed by Codex.

Implementation is in progress. The complete user-authorized scope is recorded
in [the specification](docs/SPECIFICATION.md). The initial private Codex adapter
is implemented. Setup, diagnostics, configuration explanation and private
review preparation with a durable local run inbox and explicit publication
with reconciliation are available. The initial headless watcher and durable
start/skip decisions, grouped notifications, personal holds, optional terminal
attachment and owner/resource recovery are implemented in source. Native platform
UX and live GitHub publication acceptance remain incomplete. CI snapshots are
commit-specific; the older `f73eab6` artifacts exclude these later features.

See [implementation boundaries](docs/IMPLEMENTATION.md) for the current scope
and [acceptance tracking](docs/ACCEPTANCE.md) for outstanding product work.

## Current CLI

Download a [checksummed macOS/Linux CI snapshot](docs/DISTRIBUTION.md), or build
`cmd/reviewd` using a disposable Go cache, then see
[configuration and setup](docs/CONFIGURATION.md) for prerequisites, locations,
exact-project policy and trusted repository preferences.

```sh
reviewd init
reviewd doctor --json
reviewd config explain shipitfi/api --offline
reviewd completion zsh
reviewd watch --headless
reviewd inbox --interactive
reviewd start <item-id>
reviewd pause owner/repo
reviewd snooze <item-id> --for 2h
reviewd cancel <run-id>
reviewd cleanup --dry-run
reviewd review https://github.com/owner/repo/pull/17 --private
reviewd inbox --json
reviewd show <run-id>
reviewd open <item-or-run-id> --print
reviewd publish <run-id>
```

Initialization never chooses repositories, enables automatic publication or
installs an OS service. Offline explanation requires no account access.
Private preparation does not grant publication, including on projects whose
normal publication policy is automatic. See [private reviews](docs/REVIEWS.md)
and [explicit publication](docs/PUBLICATION.md). See [foreground watching](docs/WATCHING.md)
for automatic policy, held backlog, retry and recovery boundaries, and
[notifications](docs/NOTIFICATIONS.md) for grouping and quiet-hour settings.
[Personal holds](docs/CONTROLS.md) documents project pause/resume and item snooze.
[Scoped cancellation](docs/CANCELLATION.md) distinguishes a retained request
from actual worker exit; it never retracts a submitted review.
The [optional terminal inbox](docs/TERMINAL.md) uses the same commands and
publication owner, with keyboard choices and exact-preview confirmation.
[Private-history retention](docs/RETENTION.md) documents terminal-text expiry,
retained summaries and publication receipts, and protected unresolved work.
[Private-owner recovery](docs/RECOVERY.md) explains live/orphan classification,
safe resource reclamation and preserved ambiguous work.

## Development

Requires Go 1.26 or newer and Python 3 for the disposable-cache check runner.

```sh
python3 scripts/check.py
```

Ordinary checks are offline. The opt-in live Codex fixture requires a separately
installed, authenticated Codex CLI and does not write to GitHub.

Discovery and durable inbox commands are described in [discovery](docs/DISCOVERY.md).
One-shot synchronization does not run reviews; `watch` owns continuous automation.
