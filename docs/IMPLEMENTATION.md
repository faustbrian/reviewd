# Implementation boundaries

The specification remains authoritative. This document records implementation
choices without changing its scope.

Supported operating systems are macOS and Linux only, on amd64 and arm64.
Windows support was explicitly removed by the operator on 2026-10-13.

## Current delivery

The private Codex protocol adapter, sealed evidence broker, setup, diagnostics
and trusted configuration explanation are delivered. Manual private review
preparation, complete immutable source/context capture, SQLite run persistence,
exact previews and checks-only refresh without another source/model review are
delivered at `b8a4fd46aa7165e66e5042f11bd2aebe1c5737db`. It is not the finished application:
continuous polling, scheduling/recovery, notifications and the TUI remain
outstanding. See [configuration and setup](CONFIGURATION.md) and
[private reviews](REVIEWS.md). Explicit publication, duplicate references,
readback and partial repair are implemented with owned Git/HTTP journey checks;
see [publication and recovery](PUBLICATION.md). Live publication is unverified.

`internal/codex` starts a separate App Server with a private control directory,
fresh workspace and a copied provider authentication file. It does not copy
the user's Codex configuration, writable connectors or GitHub credentials.
The selected installed skill is explicitly injected. Built-in command execution,
apps, browser/computer use, image reading/generation, ambient skill discovery,
plugins, hooks, memory and native agent spawning are disabled; the review turn uses a
read-only, network-disabled sandbox. Permission expansion and write requests
are declined. Only application-granted immutable source and evidence are
available through the broker.

The broker exposes paged source manifests/text at named comparison-base,
head and trusted-target revisions. Its inputs are frozen copies. It has no
filesystem, shell, network or publisher capability. App-owned bare Git
acquisition reconciles complete changed-file/commit counts and accounts for
binary/large-file, submodule and LFS limitations. An optional app-owned
specialist capability returns at most one independent sealed leaf report;
the leaf cannot delegate again or obtain publisher capabilities.

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

Setup/configuration at `ab689855eb62a57e68741ab92b84279138f81c25` passed all four
native hosted jobs in [run 38413412605](https://github.com/faustbrian/reviewd/actions/runs/38413412605).
The manual-review batch at `b8a4fd46aa7165e66e5042f11bd2aebe1c5737db` passed
native macOS arm64 and both Linux jobs in
[run 38444663689](https://github.com/faustbrian/reviewd/actions/runs/38444663689).
Its macOS Intel job exposed a cleanup failure. A controlled Darwin reproduction
confirmed that a group containing only an exited child can return `EPERM`
between the metadata scan and signal. The supervisor now rechecks liveness after
that error; live or uninspectable groups still fail and retain private state.
The fixture exercises safe removal and both retention controls. This local
reproduction does not establish the unrecorded process state of the failed CI
job; native hosted follow-up remains a separate verification boundary.

A separate native CLI acceptance adapter used owned HTTP/Git fixtures and the
actual authenticated Codex/installed `requesting-code-review` package. It found
a seeded percentage-calculation defect, retained the exact canonical inline
preview, kept proposed head publication preferences from granting authority,
and exercised durable `show`/`inbox` after preparation. This proves the exercised
private composition, not live GitHub acquisition/publication or every review
quality partition. No GitHub review was posted.

That acceptance exposed proactive token refresh in the metadata probe. A
non-refresh read reported available account metadata while a forced refresh
returned a null account; a separate actual SDK turn accepted cached credentials.
Account diagnostics now avoid proactive refresh. Provider acceptance still
belongs to the actual turn, not the metadata result.

A later native adapter acceptance used the actual installed skill/model against
a comment-only owned fixture. Captured CI failed, then passed before preparation;
the model's identified check observation was superseded without losing its source
conclusion. Two subsequent `refresh` commands changed failed/passed disposition
while preserving the same primary thread/turn. Plain `show` and JSON `inbox`
remained available. This is private native composition with owned Git/HTTP—not
live GitHub acquisition/publication or a specialist-subprocess acceptance.

One-shot allowlisted discovery and a durable inbox are now wired through `sync`,
`inbox`, `follow`, and `unfollow`; see [discovery](DISCOVERY.md). This does not
claim completion of continuous scheduling, notifications, or terminal attachment.
