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
| 13 | Proposed repository config cannot expand authority | Not implemented; sealed broker is a prerequisite |
| 14 | Field-level override/provenance resolution | Not implemented |
| 15 | Invalid reload preserves the last valid config | Not implemented |
| 16 | Missing prerequisites produce actionable errors | Partial: runtime startup and model/effort errors |
| 17 | Mandatory check states classified accurately | Not implemented |
| 18 | Interrupted/malformed workers never become clean reviews | Partial: protocol EOF/malformed-response tests |
| 19 | Own-PR feedback grants no fix authority | Not implemented |
| 20 | Self-review uses the permitted GitHub event | Not implemented |
| 21 | Closing a PR cancels queued work | Not implemented |
| 22 | Debounce coalesces code updates | Not implemented |
| 23 | Quiet hours/snooze/grouping preserve inbox state | Not implemented |
| 24 | Concurrent clients preserve scheduler invariants | Not implemented |
| 25 | Model and effort reach actual Codex invocation | Partial: subprocess wire tests and opt-in live fixture |
| 26 | Worker cannot bypass publication policy | Partial: sealed broker, restricted runtime and excluded credentials; publisher absent |
| 27 | Owned snapshots/process trees clean up safely | Partial: runtime setup/failure/cancellation checks |
| 28 | TUI resize/keyboard/native selection/restoration | Not implemented |
| 29 | Headless and JSON expose the same workflow states | Not implemented |
| 30 | Platform builds and native runtime match claims | Partial: target compilation; hosted/native acceptance pending |

Named commands, discovery completeness, full review evidence, selected review
skill quality, publication preflight, configuration validation, retry budgets,
retention, installation and distribution documentation remain in scope beyond
this table. They must be audited before declaring the complete goal achieved.
