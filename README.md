# reviewd

A personal, local-first GitHub review inbox backed by Codex.

Implementation is in progress. The complete user-authorized scope is recorded
in [the specification](docs/SPECIFICATION.md). The initial private Codex adapter
is implemented. Setup, diagnostics, configuration explanation and private
review preparation with a durable local run inbox and explicit publication
with reconciliation are available. Monitoring, notifications and the TUI are
still being implemented.

See [implementation boundaries](docs/IMPLEMENTATION.md) for the current scope
and [acceptance tracking](docs/ACCEPTANCE.md) for outstanding product work.

## Current CLI

Build `cmd/reviewd` using a disposable Go cache, then see
[configuration and setup](docs/CONFIGURATION.md) for prerequisites, locations,
exact-project policy and trusted repository preferences.

```sh
reviewd init
reviewd doctor --json
reviewd config explain shipitfi/api --offline
reviewd completion zsh
reviewd review https://github.com/owner/repo/pull/17 --private
reviewd inbox --json
reviewd show <run-id>
reviewd publish <run-id>
```

Initialization never chooses repositories, enables automatic publication or
installs an OS service. Offline explanation requires no account access.
Private preparation does not grant publication, including on projects whose
normal publication policy is automatic. See [private reviews](docs/REVIEWS.md)
and [explicit publication](docs/PUBLICATION.md).

## Development

Requires Go 1.26 or newer and Python 3 for the disposable-cache check runner.

```sh
python3 scripts/check.py
```

Ordinary checks are offline. The opt-in live Codex fixture requires a separately
installed, authenticated Codex CLI and does not write to GitHub.

Discovery and durable inbox commands are described in [discovery](docs/DISCOVERY.md).
One-shot synchronization does not run reviews; continuous automation remains in progress.
