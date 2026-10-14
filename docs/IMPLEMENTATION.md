# Implementation boundaries

The specification remains authoritative. This document records implementation
choices without changing its scope.

Supported operating systems are macOS and Linux only, on amd64 and arm64.
Windows support was explicitly removed by the operator on 2026-10-13.

## Current implementation

The integrated personal workflow is delivered in the `dd4d595` development
snapshots: setup/configuration, immutable private review preparation, explicit
publication/reconciliation, durable discovery and scheduling, CI waiting,
server-guided cooldowns, notifications, project/item holds, optional terminal
controls, live primary progress, explicit clarification, cancellation, owner
recovery and retention. It is not the finished goal: live GitHub publication
is still unverified; native observations cover only their documented tested
environments.

All four native quality and all four native package jobs passed for that
revision. Downloaded artifacts were verified. Separate native macOS observations
cover actual-model owner-exit/EOF recovery, ordinary Terminal header drag and
notification adapter dispatch/AX surfacing. Linux observations also cover the
actual terminal artifact and private xterm/Dunst components. Their limits are in
[acceptance tracking](ACCEPTANCE.md); none proves power-loss recovery, every
terminal/desktop, or live GitHub publication.

See [watching](WATCHING.md), [configuration](CONFIGURATION.md),
[reviews](REVIEWS.md), [publication](PUBLICATION.md), [controls](CONTROLS.md),
[terminal workflows](TERMINAL.md), [sessions](SESSIONS.md),
[recovery](RECOVERY.md), [retention](RETENTION.md) and
[distribution](DISTRIBUTION.md) for the operator contracts.

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

- The TUI is an optional attachment: users may choose ordinary
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

The historical batch results below identify their own revisions; they do not
replace the current delivered boundary above or its acceptance limits.

Run `python3 scripts/check.py` for offline tests, race detection, vet and
CGO-free target compilation. Every bounded run creates and removes its own Go
build/module/temp caches. Target compilation is not native runtime acceptance.

For an explicitly authorized live Codex check, use
`python3 scripts/check.py --live-codex`. This uses the existing Codex login and
a task-owned fixture skill; it does not post a review. That fixture proves
protocol/skill injection and bounded evidence access, not staff review quality
or live GitHub publication through the application's publication owner.

Use `python3 scripts/check.py --codex-capabilities` for the installed-host
capability check. Its synthetic API key only reaches an owned local fixture;
it never copies or sends operator authentication to that fixture.

CI installs the tested public Codex package, 0.162.1, into disposable storage
and exercises that fixture natively on each supported OS/architecture. Use
`python3 scripts/check.py --codex-capabilities --install-codex` to reproduce it.
No provider account is needed for that check. The SDK batch at
`8ce17e56f9d20fd397588a26fb99f757bfa40e3f` passed all four native hosted jobs in
[run 38396971620](https://github.com/faustbrian/reviewd/actions/runs/38396971620).
Those results do not certify later source revisions.

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
`inbox`, `follow`, and `unfollow`; see [discovery](DISCOVERY.md). The later local
watcher, notifications and terminal attachment have separate verification and
delivery boundaries; they are not included in the historical snapshots below.

Immutable macOS/Linux amd64/arm64 distribution snapshots at `f73eab6` passed
all four native quality and packaging jobs in
[run 38503954299](https://github.com/faustbrian/reviewd/actions/runs/38503954299).
All four downloaded archives were checked against that source revision and
the macOS arm64 download passed version/help/completion smoke. These artifacts
exclude the later local watcher/notification changes; see
[distribution](DISTRIBUTION.md).

Local private-session input now has ordinary `inputs`/`answer` commands,
transactional exact-target selection, bounded asynchronous protocol waits,
separate local/write/clear facts, generic notifications and an optional terminal
editor. The actual selected model asked and consumed an independently supplied
fixture answer through the default private CLI. These later changes are absent
from the older `f73eab6` snapshot. Hands-on Linux desktop UX, arbitrary hard-crash
modes and live GitHub publication remain outside that fixture. The later bounded native observations
are recorded in [acceptance tracking](ACCEPTANCE.md).
Native macOS PTY checks now cover answering, dismissal, interruption, stale
prompts and terminal restoration without mouse reporting.

Local private-history expiry now uses the configured retention period through
`cleanup` and the foreground watcher. It preserves unresolved work, logical work
keys, following and compact publication receipts; see [retention](RETENTION.md).
Retention does not itself provide crash-left process/source/runtime recovery;
the separate owner-aware recovery path handles that boundary.

A later native watcher acceptance uses the actual installed review skill and
actual `gpt-6.1-sol` account execution against an owned Git/HTTP fixture. It finds
a seeded percentage-discount regression at the canonical changed line, retains
the private preview and skill/session identities, and confirms owner-storage
removal. This is separate from the controlled clarification skill and its
unpredictable-answer oracle. No GitHub review is posted and no broad staff-review
quality guarantee is inferred. The opt-in `TestLiveCodexWatcherUsesInstalledReviewSkill`
requires both `REVIEWD_LIVE_CODEX=1` and `REVIEWD_LIVE_SKILL_PATH` naming the
operator-authorized installed `SKILL.md`; ordinary offline checks skip it.
