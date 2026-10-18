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

Install GitHub CLI and your selected Codex CLI or Claude Code separately and configure them normally. No
launchd/systemd job or persistent machine configuration is installed by reviewd.
A user may run `watch --headless` under their own supervisor if desired; the
foreground command remains the same on both supported operating systems.

## Install a published archive

Choose a published tag from [Releases](https://github.com/faustbrian/reviewd/releases).
`v1.0.0` below is an example, not a claim that a release already exists. This
installs only the executable; GitHub CLI and the selected agent remain separate prerequisites.

```sh
set -eu
version=v1.0.0 # Replace with the published version you want.
case "$(uname -s)" in Darwin) os=darwin;; Linux) os=linux;; *) exit 1;; esac
case "$(uname -m)" in x86_64) arch=amd64;; arm64|aarch64) arch=arm64;; *) exit 1;; esac
archive="reviewd_${version}_${os}_${arch}.tar.gz"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
gh release download "$version" --repo faustbrian/reviewd \
  --pattern "$archive" --pattern SHA256SUMS --pattern manifest.json --dir "$work"
awk -v selected="$archive" '$2 == selected || $2 == "manifest.json"' \
  "$work/SHA256SUMS" > "$work/selected-checksums"
test "$(wc -l < "$work/selected-checksums")" -eq 2
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$work" && sha256sum -c selected-checksums)
else
  (cd "$work" && shasum -a 256 -c selected-checksums)
fi
tar -xzf "$work/$archive" -C "$work"
prefix="$HOME/.local/bin"
mkdir -p "$prefix"
install -m 0755 "$work/reviewd" "$prefix/reviewd"
"$prefix/reviewd" --version
```

Add `~/.local/bin` to your shell's `PATH` if needed. Inspect `BUILD.json` and
`manifest.json` for source commit/tree, version, target and hashes. Checksums
detect damaged/mixed downloads; obtaining them from the same release is not an
independent signature-verification mechanism. To upgrade, stop the foreground
watcher, install the chosen archive, then restart it with the same configuration.
Read [upgrade notes](UPGRADING.md) before replacing a historical build.

## Versioned release workflow

Pushing an approved `vMAJOR.MINOR.PATCH` tag (optionally a prerelease such as
`v1.0.0-rc.1`) runs the four native quality and packaging/smoke jobs. Each archive
embeds that exact tag and immutable source. The final job assembles all four
targets, verifies archive/binary hashes and embedded source/version metadata,
then publishes through GitHub CLI for the **existing** tag. Prerelease suffixes
produce prereleases. Main-branch pushes still produce development CI artifacts,
not formal releases.

The publication job alone has release write permission. GitHub CLI stages a
draft, uploads the complete assets and publishes only after successful uploads.
A failure is not success; inspect any draft before manually retrying. The script
does not overwrite an existing output directory or automatically resume a
failed remote publication. The workflow never creates an implicit tag.

For a local rehearsal without creating a tag or release:

```sh
python3 scripts/package.py --version v0.0.0-rehearsal --output rehearsal-packages
python3 scripts/release.py --packages rehearsal-packages \
  --version v0.0.0-rehearsal --revision "$(git rev-parse HEAD)" \
  --tree "$(git rev-parse 'HEAD^{tree}')" --output rehearsal-release
```

Only an explicit `--publish` invokes GitHub release publication. Local tests use
an inert GitHub CLI adapter; a rehearsal does not prove a hosted tag publication
has happened. No release is published merely by implementing this workflow.
