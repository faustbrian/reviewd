# Acceptance tracking

This mapping preserves every original numbered acceptance requirement. Partial
SDK evidence does not imply that the full application workflow is complete.
The exact numbering refers to section 16 of the specification.

| ID | Required outcome | Current evidence |
| --- | --- | --- |
| 01 | API notify-only never starts a reviewer | Not implemented |
| 02 | Bill automatic flow prepares and verifies publication | Not implemented |
| 03 | Duplicate observations do not duplicate jobs | Not implemented |
| 04 | New commits invalidate old publication consent | Not implemented |
| 05 | Explicit same-head re-request creates distinct work | Not implemented |
| 06 | Material context invalidates cached review evidence | Not implemented |
| 07 | Restart after submit does not repost blindly | Not implemented |
| 08 | Lost responses reconcile or remain explicitly unknown | Not implemented |
| 09 | Partial publication avoids duplicate comments | Not implemented |
| 10 | Ask remains pending without an attached terminal | Not implemented |
| 11 | Notification expiry never authorizes action | Not implemented |
| 12 | Initial sync does not process the backlog automatically | Not implemented |
| 13 | Proposed repository config cannot expand authority | Target-base wiring and private flow verified with owned Git/HTTP source and real Codex; automatic publisher remains absent |
| 14 | Field-level override/provenance resolution | Resolver, explanation and frozen manual-run policy verified; automatic scheduling remains outstanding |
| 15 | Invalid reload preserves the last valid config | Manager verified: invalid full reload retains valid document and old snapshots; watcher wiring outstanding |
| 16 | Missing prerequisites produce actionable errors | Setup/doctor verified with fixtures and hands-on real account metadata; actual review/provider failures remain a separate boundary |
| 17 | Mandatory check states classified accurately | Partial: required context/head/integration state fixtures and end-of-preparation refresh; reusable-workflow identity verification outstanding |
| 18 | Interrupted/malformed workers never become clean reviews | Partial: protocol EOF/malformed-response tests |
| 19 | Own-PR feedback grants no fix authority | Not implemented |
| 20 | Self-review uses the permitted GitHub event | Prepared-result variant verified: separate internal opinion and proposed COMMENT; real publication outstanding |
| 21 | Closing a PR cancels queued work | Not implemented |
| 22 | Debounce coalesces code updates | Not implemented |
| 23 | Quiet hours/snooze/grouping preserve inbox state | Not implemented |
| 24 | Concurrent clients preserve scheduler invariants | Partial: two SQLite clients, one review claim and exact-payload publication consent; scheduler/profile locking outstanding |
| 25 | Model and effort reach actual Codex invocation | Partial: subprocess wire tests and opt-in live fixture |
| 26 | Worker cannot bypass publication policy | Partial: sealed broker, restricted runtime and excluded credentials; publisher absent |
| 27 | Owned snapshots/process trees clean up safely | Partial: runtime checks, native bare-source success/failure cleanup and private acceptance; full scheduler/recovery cleanup outstanding |
| 28 | TUI resize/keyboard/native selection/restoration | Not implemented |
| 29 | Headless and JSON expose the same workflow states | Partial: manual prepared/failed/waiting run state, plain/JSON show/inbox; watcher/headless decisions outstanding |
| 30 | Platform builds and native runtime match claims | Partial: SDK target builds plus native capability CI passed on 8ce17e5; current CLI batch/platform UX still pending final checks |

Named commands, discovery completeness, full review evidence, selected review
skill quality, publication preflight, configuration validation, retry budgets,
retention, installation and distribution documentation remain in scope beyond
this table. They must be audited before declaring the complete goal achieved.

The current CLI's hands-on acceptance used task-owned configuration/binary
storage, the existing Codex account, and read-only GitHub access to
`faustbrian/reviewd`. Initialization, doctor, online/offline explanation, safe
repeat initialization and bash/zsh/fish completion were exercised. This did
not start a review or publish anything; it is not full vertical-slice acceptance.
