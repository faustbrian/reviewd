# reviewd

A personal, local-first GitHub review inbox backed by Codex.

Implementation is in progress. The complete user-authorized scope is recorded
in [the specification](docs/SPECIFICATION.md). The initial private Codex adapter
is implemented; the user-facing watcher and inbox are not yet available.

See [implementation boundaries](docs/IMPLEMENTATION.md) for the current scope
and [acceptance tracking](docs/ACCEPTANCE.md) for outstanding product work.

## Development

Requires Go 1.26 or newer and Python 3 for the disposable-cache check runner.

```sh
python3 scripts/check.py
```

Ordinary checks are offline. The opt-in live Codex fixture requires a separately
installed, authenticated Codex CLI and does not write to GitHub.
