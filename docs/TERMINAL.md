# Optional terminal inbox and navigation

This interface is included in the `dd4d595` macOS/Linux development snapshots.
The older `f73eab6` snapshots do not contain the watcher/control interface.

```sh
reviewd watch
reviewd inbox --interactive
reviewd watch --headless
reviewd watch --once --json
reviewd inbox --json
reviewd open <item-or-run-id>
reviewd open <run-id> --conversation
reviewd open <item-or-run-id> --print --json
```

`watch` attaches the keyboard inbox when both input and output are terminals.
It runs the same application watcher as `--headless`, not a separate scheduler.
JSON, redirected output and `--once` do not attach a terminal. `inbox` remains
an ordinary snapshot unless `--interactive` is selected; that attachment does
not start a watcher. Start choices remain queued until a watcher processes them.

The views distinguish reviews owed, feedback on your own PRs, running/queued
work and publication decisions/outcomes. Enter opens details: why the item
appeared, policy, model/effort, revisions, findings, gaps and actual publication
state. Operator/project defaults are labelled separately from a run's frozen
configuration; trusted repository preferences are acquired at review start.

## Keyboard

| Key | Action |
| --- | --- |
| `0`–`4` | All, reviews owed, own-PR feedback, running, publication |
| Arrows or `j`/`k` | Select, or scroll details |
| Enter / Escape | Details / back or dismiss a choice |
| Home, End, PgUp, PgDn | Navigate long lists/details |
| `s` | Confirm a private review of the displayed event |
| `m`, `e`, `w` in that confirmation | One-run model, effort, review-now |
| `x` | Confirm skipping the displayed event |
| `p` | Inspect and confirm the exact captured publication preview |
| `P` | Explicitly repair one missing inline finding |
| `v` / `g` | Refresh private checks / reconcile an existing submission |
| `c` | Confirm cancellation of the selected exact run |
| `a` | Confirm project pause or resume |
| `z` / `Z` | Duration snooze / until new commits; `d` edits duration |
| `u` | Release a snooze |
| `f` / `F` | Follow / stop following the PR |
| `o` / `O` | Open PR / recorded review conversation |
| `r` / `l` / `?` | Refresh local state / monitoring log / help |
| `q` / Ctrl-C | Detach / interrupt |

Only `y` in an action confirmation grants that action. Escape, `n`, notification
expiry, detachment and elapsed time grant nothing. Publication confirmation
retains the captured body, inline comments, GitHub event, run and payload even
when the inbox refreshes. The publisher still revalidates current evidence;
a changed result needs a new explicit decision. A start/skip confirmation is
bound to its displayed event, not whichever newer revision now occupies its row.
Scripts can use the same guard with `start`/`skip --event <event_key>` from inbox
JSON. The UI does not grant code-fixing or thread-resolution authority.

Detaching an inbox does not stop a separate watcher. Detaching a `watch` you
started in that terminal stops its owned foreground watcher and joins its work;
unconfirmed decisions remain pending. Cancellation acknowledgement does not
claim that an active worker has already stopped or retract a submitted review.

Normal unmodified text selection stays terminal-owned: the interface does not
enable mouse reporting. Layout uses terminal cell widths rather than byte
counts, and untrusted terminal control characters are displayed literally.

## Browser opening

`open` uses durable repository/PR identity, not an arbitrary URL supplied by a
PR title or body. `--conversation` requires a recorded review link on that same
PR; it does not assert that the remote conversation still exists or was newly
verified. Navigation never grants a review or publication decision.

macOS uses its built-in `open` launcher; Linux uses optional `xdg-open` in a
desktop session. Arguments are passed directly, without a shell. Launcher
success is not proof that a browser became visible. On servers, in restricted
sessions, or when you only want the URL, use `--print`; it does not launch a
browser or connect to GitHub.

## Acceptance boundary

Owned native PTY journeys exercise keyboard choices, Unicode, resize,
interrupt/dismissal, terminal modes and absence of mouse-reporting activation.
Model checks exercise captured-payload scope and bounded layouts. A separate
native macOS Terminal observation used the downloaded `dd4d595` arm64 binary:
ordinary left-button drag with zero modifier flags selected the actual inbox
header, verified through native selected-text state. This covers that Terminal
configuration/header, not every emulator/view or hands-on Linux desktop/browser
behaviour. No live GitHub review is published by these terminal fixtures.

A native macOS PTY check also exercised idle attached `watch` exit by `q` and
Ctrl-C, observed profile-lease release and operational terminal restoration,
and confirmed that detaching a separate inbox preserved its headless watcher.
Its seeded ask item remained pending with no decision/run. A cooldown prevented
remote/model admission; this does not independently prove graceful joining
while a model turn is active.

The downloaded linux/arm64 binary also passed a bounded interactive check in an
isolated arm64 Linux container: help/back, `q`, Ctrl-C and unchanged native Linux
termios before/after exit at 74×18. This used an empty inbox and no desktop
session, so it does not establish graphical drag selection or notification
visibility on a Linux desktop.

A separate private Linux Xvfb/xterm observation selected the actual header by
ordinary unmodified drag and inspected the resized 620×350 empty inbox. A
combined helper's direct-window `q` injection failed; a subsequent same-terminal
probe distinguished missing fixture input delivery from application handling.
With verified focus, the actual inbox accepted `q`, exited 0 and restored
identical terminal settings. The original helper remains failed; these bounded
components do not certify every emulator or a populated review workflow.
