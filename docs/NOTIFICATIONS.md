# Notifications

Foreground `reviewd watch --headless` presents desktop and terminal notices
without granting review, publication, code-fix or thread-resolution authority.
Every decision remains available through `reviewd inbox`, `reviewd start`,
`reviewd skip`, `reviewd show` and `reviewd publish`. Closing or dismissing a
notification does nothing to those decisions.

Notifications accompany the foreground watcher and are included in `dd4d595`
development snapshots. Older `f73eab6` snapshots do not contain this workflow.

## Settings

Use global defaults or exact-project overrides; a repository `.reviewd` cannot
control notification policy:

```toml
[notifications]
desktop = true
terminal = true
group_interval = "5s"
quiet_start = "22:00"
quiet_end = "07:00"
timezone = "Europe/Helsinki"

[projects."shipitfi/api".notifications]
desktop = false
```

Both quiet-hour endpoints must be present, use `HH:MM` (or a single-digit hour
as `H:MM`), and denote different clock values. Existing unpadded settings and
frozen policies retain their identity; comparison uses normalized clock values.
Leave both empty to disable quiet hours. The interval includes its start and excludes its
end, including when it crosses midnight. The timezone is an IANA name or
`Local`. Quiet membership uses the actual instant's local wall clock: both
occurrences of a repeated daylight-saving hour are quiet, and a skipped end
time releases the digest at the first actual non-quiet instant. The watcher
must be running for delivery; sleeping or stopped processes do not send notices.

The grouping interval accepts `0s` through `5m`. A window begins with the first
pending observation; later changes replace its contents without postponing it
indefinitely. `watch --once` flushes that invocation's grouping window before
exiting, but never bypasses quiet hours. In continuous mode the foreground loop
checks local notification state approximately once per second; ongoing GitHub
requests can delay that check.

## What appears

- Pending review decisions and notification-only work identify the repository,
  PR and terminal action. Own-PR feedback never starts a fixer.
- Prepared results are labelled prepared, not published. Verified publication,
  stale reviews, failed work and ambiguous or partial publication have distinct
  messages pointing to the durable run.
- Discovery failure is attention-worthy, not an empty inbox.
- Multiple categories and inline-comment changes on one PR share a notice.
- Existing backlog is summarized rather than replayed one PR at a time.
  Quiet-hour work is retained for a subsequent digest. More than five pending
  groups are likewise summarized to prevent reconnect storms.
- Digests combine projects only when their enabled delivery channels match;
  projects still in quiet hours do not join a released digest. The body shows
  at most five entries and points to the complete inbox.

[Project pauses and item snoozes](CONTROLS.md) defer pending notifications until
release; they do not consume the inbox decision or pending notice.

Current operator/project settings govern delivery, independently of a review's
frozen execution settings. Muting a channel does not hide inbox work. Work
observed while all channels are disabled is not replayed merely because a
channel is later enabled; a new material event can notify again.

Deduplication and pending grouping state live in the same private SQLite
profile as the inbox. Repeated observations and ordinary restarts do not
replay notices. New explicit same-revision requests are distinct events.
Preparing or delivering a notice never creates a publication decision.

Delivery is best-effort. The application transactionally records an attempt
before invoking output adapters, so a crash at that boundary can lose a popup
rather than replay a notification storm. The inbox and its decisions survive.
An unavailable desktop or terminal output does not end the watcher or trigger
unbounded popup retries. A session warning directs the operator to the inbox.

## Desktop adapters

macOS uses `/usr/bin/osascript` with a fixed AppleScript notification program;
the title and body are literal arguments, not script code. Banners and alerts
depend on the user's Notification Center permissions and Focus settings.
Nothing installs a service or changes those settings. See Apple's
[notification scripting documentation](https://developer.apple.com/library/archive/documentation/LanguagesUtilities/Conceptual/MacAutomationScriptingGuide/DisplayNotifications.html).

Linux uses the optional `notify-send` program from libnotify and the current
desktop notification session. A headless machine, missing executable or absent
notification daemon can make desktop delivery unavailable. Install the
distribution's libnotify tools if desired; terminal/inbox workflows do not
require them. Text is passed as arguments with option parsing terminated, and
body markup is escaped. The supported command options are defined by
[libnotify's notify-send implementation](https://github.com/GNOME/libnotify/blob/master/tools/notify-send.c).

Neither adapter waits for a click, opens a browser automatically, or treats
notification expiry as consent. Click-to-decide actions are not implemented;
use the terminal commands. A successful adapter exit proves dispatch, not that
the operating system visibly displayed a banner or that the user read it.

Automated owned-adapter/SQLite checks cover grouping, quiet boundaries/DST,
deduplication, failure isolation and decision preservation. A separate native
macOS arm64 check invoked the actual desktop sink once; dispatch passed and
Notification Center exposed the uniquely titled benign message with nonzero
accessibility geometry. This is native AX surfacing, not screenshot/pixel
visibility, human readability, full scheduler-to-popup composition or Linux
desktop acceptance. No settings/history were cleared. Owner-scoped dismissal
was unavailable, so the benign test entry may remain in OS history.
