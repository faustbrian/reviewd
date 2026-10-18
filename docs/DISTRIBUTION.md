# Distribution

Supported packages contain one native `reviewd` binary, `README.md`, `BUILD.json`
and third-party notices. Targets are `darwin/amd64`, `darwin/arm64`,
`linux/amd64` and `linux/arm64`. There is no guest Linux command helper,
container image, bubblewrap or AppArmor installation requirement.

```sh
python3 scripts/package.py --target darwin/arm64 --output /tmp/reviewd-package --smoke
```

Packaging exports an immutable commit, not dirty or untracked workspace files.
It binds the binary/archive hashes, source commit/tree, toolchain, module graph
and license notices into the output metadata and checksums. It refuses an
existing output directory. `--smoke` runs only for the host's native target and
checks version, help and shell completion. Packaging does not publish a release.

Install GitHub CLI and Codex CLI separately and configure them normally. No
launchd/systemd job or persistent machine configuration is installed by reviewd.
A user may run `watch --headless` under their own supervisor if desired; the
foreground command remains the same on both supported operating systems.
