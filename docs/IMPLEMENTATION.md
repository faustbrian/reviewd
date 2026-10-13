# Implementation boundaries

The specification remains authoritative. This document records implementation
choices without changing its scope.

Supported operating systems are macOS and Linux only, on amd64 and arm64.
Windows support was explicitly removed by the operator on 2026-10-13.

## Current delivery

The private Codex protocol adapter and sealed evidence broker are delivered.
The current next batch adds setup, diagnostics, trusted configuration reads,
resolution and explanation. It is not the finished application and does not
implement GitHub discovery, inbox persistence, publication, notifications or
the TUI yet. See [configuration and setup](CONFIGURATION.md).

`internal/codex` starts a separate App Server with a private control directory,
fresh workspace and a copied provider authentication file. It does not copy
the user's Codex configuration, writable connectors or GitHub credentials.
The selected installed skill is explicitly injected. Built-in command execution,
apps, browser/computer use, image reading/generation, ambient skill discovery,
plugins, hooks, memory and internal agent spawning are disabled; the review turn uses a
read-only, network-disabled sandbox. Permission expansion and write requests
are declined. Only application-granted immutable source and evidence are
available through the broker.

The broker exposes source manifests and paginated text at named base/head
revisions. Its inputs are frozen copies. It has no filesystem, shell, network
or publisher capability. Later source acquisition must populate complete
snapshots and explicitly retain binary/large-file evidence limitations.

The protocol uses the experimental App Server dynamic-tool capability. The
installed Codex version exercised during development is 0.162.1; compatibility
with another version must be checked, not inferred. Provider authentication
stored only in a keyring is not yet supported by this adapter.

A credential-free local provider fixture forces an outside-workspace image
read instead of relying on model compliance. The installed host rejects the
call, emits no image-read event and sends no image bytes. This tests actual
host enforcement for the exercised capability; it does not certify every
future host tool or another Codex version.

## Product choices

- The TUI is implemented as an optional attachment: users may choose ordinary
  commands or headless mode. Optional does not mean omitting terminal acceptance.
- Repository preferences come from the exact fetched PR target-base SHA and are
  frozen with the review. A changed controlling base requires reassessment.
- Automatic publication must never interpret unavailable required-check evidence
  as a passing check. A manual review may proceed with an explicit readiness gap.
- Same-head review requests need an actual request-event identity, not just the
  current requested-reviewer list.
- Source/model fixtures, private Codex execution and real GitHub publication are
  separate proving boundaries. No GitHub publication test has been performed.

## Verification

Run `python3 scripts/check.py` for offline tests, race detection, vet and
CGO-free target compilation. Every bounded run creates and removes its own Go
build/module/temp caches. Target compilation is not native runtime acceptance.

For an explicitly authorized live Codex check, use
`python3 scripts/check.py --live-codex`. This uses the existing Codex login and
a task-owned fixture skill; it does not post a review. That fixture proves
protocol/skill injection and bounded evidence access, not staff review quality
or the application's not-yet-implemented publication contract.

Use `python3 scripts/check.py --codex-capabilities` for the installed-host
capability check. Its synthetic API key only reaches an owned local fixture;
it never copies or sends operator authentication to that fixture.

CI installs the tested public Codex package, 0.162.1, into disposable storage
and exercises that fixture natively on each supported OS/architecture. Use
`python3 scripts/check.py --codex-capabilities --install-codex` to reproduce it.
No provider account is needed for that check. The SDK batch at
`8ce17e56f9d20fd397588a26fb99f757bfa40e3f` passed all four native hosted jobs in
[run 38396971620](https://github.com/faustbrian/reviewd/actions/runs/38396971620).
Those results do not certify later uncommitted CLI changes.
