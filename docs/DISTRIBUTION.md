# Builds and downloadable artifacts

reviewd supports macOS and Linux on amd64 and arm64. Binaries do not require
Go, Python, or SQLite system libraries. Running reviews still requires the
separately installed GitHub CLI, Git, authenticated Codex CLI, and selected
review skill described in [setup](CONFIGURATION.md).

The product is still being implemented. A downloadable build is not a claim
that every specification requirement is finished, nor a formal release.

## CI artifacts

After all four native quality jobs pass on a push to `main`, CI builds and
smokes each platform's binary on its matching runner. The smoke exercises
version, help, and bash/zsh/fish completion without authenticating or starting
a review. This is not full native review, notification, or terminal acceptance.

Open a successful [CI run](https://github.com/faustbrian/reviewd/actions/workflows/ci.yml)
and download the artifact named `reviewd-<platform>-<full-commit>`. Choose
`darwin-arm64` for Apple Silicon, `darwin-amd64` for Intel Macs,
`linux-amd64` for x86-64 Linux, or `linux-arm64` for 64-bit ARM Linux.
Artifacts expire after 30 days; they are development snapshots rather than a
permanent release channel. No tag, GitHub release, service, or automatic updater
is created by this workflow.

Each artifact contains a `.tar.gz`, `manifest.json`, and `SHA256SUMS`. The archive
contains the executable, its `BUILD.json`, the selected revision's README, and
Go/dependency license notices. The manifest binds checksums to the full source
commit, Git tree, target, binary version, exact Go toolchain and module sums.
It is unsigned metadata: checksums detect corruption, not a compromised build
account or an independent publisher identity.

In the downloaded artifact directory, verify before extracting:

```sh
# macOS
shasum -a 256 -c SHA256SUMS
# Linux
sha256sum -c SHA256SUMS

tar -xzf reviewd_dev-*_darwin_arm64.tar.gz
mkdir -p "$HOME/.local/bin"
install -m 755 reviewd "$HOME/.local/bin/reviewd"
"$HOME/.local/bin/reviewd" --version
```

Use the archive matching your platform in the extraction command. Add
`$HOME/.local/bin` to your shell's `PATH` if it is not already present.
Installation is an explicit operator action; reviewd never installs a service.
macOS download policy may require the operator to allow an unsigned binary;
these artifacts are not notarized. Linux builds target the Go toolchain's
documented amd64/arm64 baseline; native CI uses Ubuntu 24.04, not every Linux
distribution or kernel. See [Go's platform requirements](https://go.dev/wiki/MinimumRequirements).

Print completion for your shell without modifying its configuration:

```sh
reviewd completion zsh
reviewd completion bash
reviewd completion fish
```

## Reproduce a build

Install the exact Go toolchain recorded in the manifest, Python 3.9 or newer,
and Git. Public module downloads need network access unless already available
through your configured module proxy. The builder does not use shared Go
build/module caches and does not install or update the Go toolchain.

```sh
# All four targets, from the current committed HEAD:
python3 scripts/package.py --output dist

# One native platform, with version/help/completion smoke:
python3 scripts/package.py --target darwin/arm64 --output native-dist --smoke

# Rebuild a particular commit and its recorded binary label:
python3 scripts/package.py --revision <full-commit> --version <recorded-version> --output rebuilt-dist
```

Output directories must not already exist; existing artifacts are not replaced.
The default binary label is `dev-<12-character-commit>`. An explicit `--version`
sets only the binary/archive label; it does not prove a version tag exists.
For a smoke check, select exactly the current host's target.

The builder exports only the selected immutable commit into disposable storage.
Dirty files, untracked files, checkout hooks, and local Git archive attributes
cannot enter a commit-labelled artifact. The ordinary checkout and index stay
untouched. Build failures publish no partial artifact set, and temporary source,
Git objects, Go caches and binaries are removed on normal or error exits.

Go uses `CGO_ENABLED=0`, explicit architecture baselines, `-trimpath`, read-only
module metadata and disabled automatic VCS stamping. Archive ordering,
ownership, permissions and timestamps are normalized. Reproducibility means
identical source, version, target, module inputs and toolchains; use the same
Python/zlib versions when comparing compressed archive bytes. Cross-compilation
alone is not native runtime acceptance. See the [Go build flags](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies).

Packaging checks use an owned compiler fixture for archive/provenance/failure
behavior; actual native builds and smoke results are separate evidence:

```sh
python3 -B -m unittest discover -s scripts -p test_package.py -v
```
