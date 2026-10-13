# reviewd

A personal, local-first GitHub review inbox backed by Codex.

Implementation is in progress. The complete user-authorized scope is recorded
in [the specification](docs/SPECIFICATION.md). The initial private Codex adapter
is implemented. Setup, diagnostics and configuration explanation are available;
the watcher, review/publication commands and inbox are not yet available.

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
```

Initialization never chooses repositories, enables automatic publication or
installs an OS service. Offline explanation requires no account access.

## Development

Requires Go 1.26 or newer and Python 3 for the disposable-cache check runner.

```sh
python3 scripts/check.py
```

Ordinary checks are offline. The opt-in live Codex fixture requires a separately
installed, authenticated Codex CLI and does not write to GitHub.
