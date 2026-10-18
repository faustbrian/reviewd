set positional-arguments

# List available commands.
default:
    @just --list

# Build the local development binary.
build:
    go build -o reviewd ./cmd/reviewd

# Install into ~/.local/bin, or a supplied directory: just install /path/to/bin.
install directory=(env_var("HOME") / ".local/bin"): build
    #!/usr/bin/env sh
    set -eu
    mkdir -p "$1"
    install -m 0755 ./reviewd "$1/reviewd"

# Run Go tests with race detection; optionally select packages or test flags.
test *args:
    #!/usr/bin/env sh
    set -eu
    if [ "$#" -eq 0 ]; then set -- ./...; fi
    exec go test -race -count=1 "$@"

# Run Go static analysis; optionally select packages.
vet *args:
    #!/usr/bin/env sh
    set -eu
    if [ "$#" -eq 0 ]; then set -- ./...; fi
    exec go vet "$@"

# Test packaging and release scripts without publishing anything.
test-scripts:
    python3 -B -m unittest discover -s scripts -p 'test_*.py' -v

# Run the CI quality gate: race tests, vet and all four target builds.
check:
    python3 scripts/check.py

# Build and run a local command, for example: just run watch --headless.
run *args: build
    #!/usr/bin/env sh
    set -eu
    exec ./reviewd "$@"
