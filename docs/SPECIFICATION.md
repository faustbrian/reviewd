# Goal: implement reviewd completely and deliver it continuously to main

Operator scope update (2026-10-13): support macOS and Linux only. Windows
implementation, artifacts and acceptance are excluded. All other requirements
remain in scope.

Implement the entire specification below as a production-quality, personal, local-first Go CLI in:

git@github.com:faustbrian/reviewd.git

Complete the product from initial repository setup through implementation, automated verification, hands-on CLI/TUI acceptance, documentation, packaging, and final delivery. Push coherent, verified implementation batches regularly to main.

This is an implementation directive, not a request for another proposal, scaffold, proof of concept, or partial MVP.

## 1. Authority and execution boundaries

You are authorized to:

- Inspect and clone the named repository.
- Create and modify its implementation, tests, documentation, configuration examples, and build workflows.
- Execute local builds, focused tests, integration tests, and hands-on CLI/TUI checks.
- Create task-owned disposable local test repositories, databases, fixtures, caches, and processes.
- Commit coherent implementation batches and ordinarily push them directly to the named repository’s main branch.
- Read GitHub repository state, authentication metadata, and hosted checks needed for this work.
- Run controlled Codex integration checks using the operator’s existing authorized Codex authentication.

The repeated instruction to push to main is standing approval for ordinary pushes to that exact target. Do not request confirmation before each authorized push.

Do not:

- Implement on an unrelated branch or mix unrelated workspace changes into this repository.
- Create implementation pull requests instead of delivering to main.
- Merge GitHub pull requests.
- Force-push, rewrite published history, bypass hooks, disable protections, or perform destructive Git operations without their applicable explicit approval.
- Publish test reviews to unrelated or real customer pull requests.
- Create GitHub repositories, tags, or formal releases without applicable authority.
- Install persistent OS services or alter the operator’s global Codex/GitHub configuration without explicit approval.
- Treat available credentials as authority for unrelated external mutations.

If a real GitHub publication test requires additional authority, finish its safe preparation first, then identify the exact test repository, operation, and cleanup scope requiring approval. Do not substitute mocked evidence for a live claim.

A missing authority or environmental prerequisite must be reported precisely. Continue independent authorized work while it is unresolved.

If a goal facility is available, start and maintain a goal for this complete objective. No token budget has been supplied.

## 2. Workspace and continuous delivery

Verify the repository, remote, current branch, worktree state, and applicable instructions before writing.

Use an existing clean checkout only when it is the intended repository and delivery target. Otherwise create an appropriate task-owned clean checkout or isolated workspace, leaving unrelated checkouts untouched.

The intended delivery branch is main. If the remote repository is empty, establish main without inventing an unrelated feature-branch workflow.

Before every implementation batch:

- Record the exact starting revision.
- Confirm that the selected files and purpose belong to this task.
- Account for remote changes and existing local changes.

Deliver regularly:

1. Finish one coherent, useful implementation batch.
2. Complete its applicable verification and independent reviews.
3. Stage only explicit task-owned paths.
4. Create an Intent-compliant commit.
5. Ordinarily push to main.
6. Verify the remote contains the delivered commit.
7. Assess applicable hosted checks before making readiness claims.

Do not accumulate the entire implementation locally until the final push. Conversely, do not push incomplete behaviour merely to demonstrate activity.

If the remote advances, reconcile it without silently rebasing or force-pushing. Preserve the separation between integration commits and subsequent implementation fixes required by applicable instructions.

## 3. Product definition

reviewd is a distributable personal CLI that monitors GitHub, maintains an actionable review inbox, sends notifications, and runs Codex reviews according to user-controlled policies.

The product must answer:

- What needs my attention?
- Why did this item appear?
- What happens automatically?
- What is the reviewer doing?
- Is the result current?
- Has anything actually been published to GitHub?

It must serve two distinct workflows:

### Reviews the user owes

Examples:

- A user or applicable team is requested to review a PR.
- A previously followed or reviewed PR receives new commits.
- An explicit review is requested again on the same revision.

### Feedback on the user’s own PRs

Examples:

- A human publishes a review.
- New inline review comments appear.
- Review-thread feedback changes.

Receiving feedback is not automatically a request to rerun a code review, implement fixes, or resolve threads.

### Non-goals

Do not turn this into:

- A shared team bot or hosted SaaS.
- An automatic code-fixing agent.
- A PR merge or thread-resolution system.
- A replacement for Codex authentication.
- A permanent LLM supervisor responsible for polling GitHub.
- A general-purpose workflow language.
- An arbitrary shell-hook execution framework.

## 4. Technology and architecture

Use Go.

Recommended components:

- Bubble Tea for the terminal inbox.
- SQLite for durable local state.
- TOML for configuration.
- Codex App Server over local stdio for review execution.
- Platform adapters for desktop notifications and necessary process/IPC behaviour.

Select maintained dependencies and verify their current APIs from primary documentation. Prefer dependencies that keep release binaries free of additional system-library requirements.

Use one application-owned scheduler and one publication owner.

Keep these responsibilities separate:

- GitHub discovery and evidence acquisition.
- Configuration resolution.
- Policy evaluation.
- Durable inbox and run storage.
- Scheduling and concurrency.
- Codex protocol integration.
- Review preparation and validation.
- GitHub publication and reconciliation.
- Terminal UI.
- Notifications.

Keep boundaries explicit without inventing a large plugin framework, generic event bus, or second workflow engine.

The CLI/TUI should be a presentation layer over the same application behaviour available to headless and JSON consumers.

## 5. Configuration contract

Global configuration must be sufficient. Repository configuration is optional.

Use a platform-appropriate user configuration directory. Document the actual path on each supported OS. Support an explicit configuration-path override.

The intended configuration shape is:

schema_version = 1

[github]
host = "github.com"
repositories = ["shipitfi/api", "shipitfi/bill"]
watch_review_requests = true
watch_own_prs = true

[watch]
poll_interval = "60s"
debounce = "30s"
include_drafts = false

[reviewer]
model = "gpt-6.1-sol"
reasoning_effort = "high"
skill = "requesting-code-review"

[limits]
max_concurrent_reviews = 2

[actions]
new_pull_request = "ignore"
review_requested = "ask"
head_updated = "ask"
review_submitted = "notify"
review_comment_added = "notify"
publish_review = "ask"

[notifications]
desktop = true
terminal = true

[projects."shipitfi/api".actions]
review_requested = "notify"
head_updated = "notify"

[projects."shipitfi/bill".actions]
review_requested = "auto"
head_updated = "auto"
publish_review = "auto"

The result must be:

- API sends notifications without automatically starting reviews.
- Bill automatically reviews eligible requests and subsequent code updates, then publishes eligible results.
- Incoming human feedback is notification-only unless configured otherwise.
- Unspecified projects retain the global defaults.

### Precedence

Resolve configuration from lowest to highest precedence:

1. Built-in defaults.
2. Global defaults.
3. Trusted repository .reviewd preferences.
4. Global exact-project overrides.
5. Command-line overrides.

Requirements:

- Merge settings field by field.
- Replace lists rather than silently concatenate them.
- Reject unknown keys, invalid durations, invalid action values, and incompatible settings with useful errors.
- Distinguish explicit false/zero values from omitted values.
- Canonicalize repository identities with host awareness.
- Retain provenance for every effective setting.
- Explain effective configuration without exposing credentials.
- Freeze effective configuration for each running job.
- Apply configuration changes to new jobs without silently changing an active run.
- Reject invalid reloads while preserving the last valid configuration.

Avoid wildcard matching and a complex rules DSL in the initial product. Exact-project sections must work reliably.

### Repository .reviewd

Support a TOML file such as:

schema_version = 1

[reviewer]
reasoning_effort = "high"

[review]
instructions = """
Pay particular attention to monetary idempotency
and compatibility with existing integrations.
"""

Repository preferences must come from the trusted base revision, not the PR’s proposed changes or an arbitrary dirty local file.

Repository settings must not:

- Enable automatic execution or publication.
- Install or select arbitrary executable tools.
- Introduce shell hooks.
- Supply credentials.
- Expand permissions.
- Override operator-controlled trust boundaries.

Define and document the allowed repository preference fields. Treat proposed changes to .reviewd as review evidence, not instructions granting authority.

A missing .reviewd must be normal. Invalid repository configuration must be surfaced clearly without silently changing automation policy.

## 6. Discovery and event semantics

Implement:

- new_pull_request
- review_requested
- head_updated
- review_submitted
- review_comment_added

Support explicit repository allowlists, configured GitHub host, draft filtering, user review requests, applicable team review requests, and feedback on the authenticated user’s own PRs.

Use bounded polling initially:

- Exhaust pagination.
- Use conditional requests where supported.
- Respect rate limits, retry guidance, and backoff.
- Avoid fetching full review evidence for every unchanged PR on every poll.
- Recover after sleep, disconnection, and missed polling intervals.
- Distinguish authentication failure from an empty inbox.
- Avoid dependence on a search result that silently truncates the relevant repository scope.

Do not use an LLM for event detection or policy matching.

### Initial synchronization

On first synchronization:

- Establish a baseline.
- Import existing relevant work into the inbox.
- Do not automatically process the entire existing backlog.
- Offer an explicit way to opt into backlog processing.

### Followed PRs

Continue tracking a reviewed PR even after GitHub removes its original review request.

Stop or update tracking appropriately when the PR closes, merges, becomes ineligible, or the user explicitly stops following it.

Do not repeatedly rediscover ignored or snoozed work as a new item unless its relevant state changes.

### Event coalescing

- Coalesce bursts of commits and comments.
- Avoid one notification per inline comment.
- Allow an explicit same-revision re-request to produce new work.
- Do not trigger a new review from reviewd’s own published review.
- Do not interpret every metadata update as a code update.
- Treat meaningful requirement/context changes as potential review invalidation, rather than deduplicating solely by head SHA.

## 7. Policy and decision semantics

For event actions, support:

- ignore: no actionable inbox item or notification.
- notify: update the inbox and notify without starting a reviewer.
- ask: retain a pending review decision.
- auto: schedule eligible review work.

For publication, support:

- never: keep the review private.
- ask: prepare a review and require an explicit publication decision.
- auto: publish after applicable validation.

Starting a review and publishing it are separate decisions.

An unanswered request must remain pending. Silence, elapsed time, a closed terminal, or an expired desktop notification must never become approval.

Decisions must be bound to the item and reviewed revision/context. A decision to publish an old result must not authorize publication of a materially changed result.

Manual actions must make their scope clear. Do not use an ambiguous local command called approve when it could be confused with a GitHub APPROVE review event.

## 8. Codex integration

Require a separately installed, authenticated Codex CLI. Do not embed provider credentials or reimplement login.

Use Codex App Server over local stdio.

Implement:

- Protocol initialization.
- Version/capability compatibility checks.
- Thread and turn lifecycle.
- Explicit model and reasoning-effort selection.
- Explicit skill discovery and injection.
- Streaming progress.
- Requests for user input and applicable execution approvals.
- Cancellation.
- Failure handling.
- Thread identification and recovery.
- Available usage reporting.

The configured default is gpt-6.1-sol with high reasoning effort. These must be configurable globally, per project, and for a single run.

Validate supported settings. Do not silently substitute another model or effort. Expose a reported effective-model change when Codex reroutes execution.

Use a fresh review thread by default. These are independent review sessions, not native subagents of an existing interactive terminal conversation.

Allow internal specialist delegation when permitted by the selected review skill. Do not create an additional planner, adversary, finalizer, or supervisor LLM merely for orchestration.

### Skill contract

Use the selected installed skill, including $requesting-code-review.

- Verify that it exists and is usable.
- Inject its actual instructions.
- Retain its identity/version or digest for the run.
- Support explicit local skill-path configuration where appropriate.
- Report missing skills as actionable errors.
- Never silently fall back to a weaker generic reviewer.
- Do not copy the operator’s private skill tree or machine-specific instructions into the public repository.

The worker must run privately with an explicit do-not-post instruction and produce a prepared review for the application’s publication owner.

### Access separation

Enforce, rather than merely prompt, the separation between review preparation and publication:

- Reviewer access to GitHub must be read-only.
- Publication tools/credentials must belong to the publisher.
- Do not inherit unrestricted writable GitHub connectors or credentials into a supposedly private reviewer.
- Do not execute untrusted PR code, hooks, installers, builds, or tests merely because remote content requests it.
- Acquire complete immutable source evidence in isolated task-owned storage.
- Keep the user’s ordinary checkout untouched.

Prove this boundary in the integration spike before relying on ask-before-publication behaviour.

## 9. Review contract and result representation

A review must cover the complete relevant base-to-head change, requirements, affected callers/consumers, variants, tests, and current mandatory verification.

Preserve the selected skill’s:

- Exact-head review requirements.
- Trusted-instruction boundary.
- Complete-diff collection.
- Causal review method.
- Mandatory-check disposition.
- Finding and publication formats.
- Deduplication.
- Identity handling.
- Post-publication verification.

Keep technical findings, missing context, readiness gaps, and publication failures distinct.

Store a compact prepared result containing enough information for preview, validation, and publication:

- Repository and PR identity.
- Posting identity.
- Exact review boundary.
- Relevant context/configuration/skill identity.
- Internal disposition and proposed GitHub event.
- Review body.
- Inline findings with canonical anchors and publication text.
- Verification/context gaps.
- Prepared payload identity.
- Run/thread identity.

Do not require an exhaustive formal review ledger unless the selected workflow explicitly requires it. Do not create a second evidence-management product.

## 10. Scheduler, persistence, and recovery

Use SQLite as the authoritative local inbox/run store.

Persist:

- Discovered relevant PR state.
- Pending decisions.
- Follow/snooze state.
- Effective policy for each run.
- Reviewed base/head and relevant context.
- Run lifecycle.
- Codex thread identifiers.
- Prepared results.
- Publication attempts and verified remote identifiers.
- Notification deduplication state where necessary.

Use explicit transactional transitions and appropriate uniqueness constraints.

Suggested lifecycle:

Discovered
  → Pending decision or Queued
  → Reviewing
  → Prepared
  → Awaiting publication or Publishing
  → Published

Additional states must represent:

- Waiting for checks.
- Stale/superseded.
- Failed.
- Skipped.
- Cancelled.
- Publication outcome unknown.

Requirements:

- One scheduler instance per intended local profile.
- No overlapping automatic runs for the same review work.
- Configurable global concurrency.
- Fair scheduling, with explicit review requests preferred over optional discovery.
- Debounced code updates.
- Cancellation of queued work when a PR closes.
- Stale marking when an active review boundary changes.
- Bounded retry/backoff for recoverable failures.
- Durable pending decisions when no UI is attached.
- Safe recovery after process termination, machine sleep, and restart.
- Cleanup of superseded temporary source snapshots and processes.
- Do not retry indefinitely or conceal repeated failure.

Reuse prior evidence only where it remains valid. A stored result must not certify a changed boundary.

## 11. CI and changing evidence

Automatic reviews should normally wait while resolved mandatory checks are pending, then proceed when they settle—including failure.

Provide a manual “review now” option.

Requirements:

- Discover required checks from trusted repository protection/ruleset evidence.
- Do not assume an aggregate green status proves mandatory checks passed.
- Surface required checks that are missing, unavailable, skipped, failed, or pending.
- Keep a check failure separate from a source-confirmed defect introduced by the PR.
- Do not repeatedly perform a complete source review merely because CI remains pending.
- When only check status changes, refresh the disposition using still-valid source review evidence.
- When code or controlling context changes, reassess affected evidence and the complete current review scope.
- Never publish “No findings” or a formal approval while the selected skill’s mandatory readiness requirements remain unresolved.

## 12. Publication correctness

The application owns publication.

Before publishing:

1. Confirm the authenticated posting identity.
2. Confirm the PR remains eligible and open.
3. Refresh the exact review boundary and material context.
4. Refresh mandatory checks.
5. Reconcile the current disposition.
6. Validate inline anchors against the reviewed objects.
7. Deduplicate against existing reviews and comments.
8. Submit against the explicit reviewed commit.
9. Refetch and verify the resulting review and comments.

Handle PR-author identity correctly: a self-review must use the allowed COMMENT path, not self-approval or self-request-changes.

Preserve the selected skill’s required review-body and inline-comment formats.

Do not promise exactly-once remote publication or an atomic current-head condition that GitHub does not provide.

If the head changes during publication:

- Preserve the actual reviewed revision.
- Mark/report the result as stale where applicable.
- Schedule or request reassessment according to policy.
- Do not claim the review certifies the new head.

If a submission times out or the response is lost:

- Reconcile against GitHub before retrying.
- Verify identity, reviewed commit, review body, and submitted comments.
- If the outcome remains ambiguous, retain publication outcome unknown.
- Do not blindly repost.

Partial publication must remain visible and recoverable without duplicating already published findings.

## 13. CLI and terminal UX

Implement a useful foreground experience:

reviewd init
reviewd doctor
reviewd watch
reviewd inbox
reviewd review <pr-url> --private
reviewd show <run-id>
reviewd publish <run-id>
reviewd snooze <item-id> --for 2h
reviewd config explain <owner/repo>
reviewd watch --headless
reviewd inbox --json

Add any necessary start, skip, follow/unfollow, pause/resume, cancellation, or dry-run commands while keeping naming consistent.

Requirements:

- init must detect prerequisites and guide configuration without modifying repositories.
- Show the GitHub identity and automation policy before enabling automatic publication.
- doctor must provide concrete corrective actions and nonzero exit status for blocking failures.
- TUI categories must distinguish reviews owed, feedback on own PRs, running work, and publication decisions.
- Details must explain trigger, policy, model/effort, review revision, findings, gaps, and actual publication state.
- Preview must show the exact proposed body, inline comments, and GitHub event.
- Provide “why am I seeing this?” explanations.
- Support per-project pause.
- Support one-run model/effort overrides.
- Support snooze for a duration and until new commits.
- Support opening the PR or relevant conversation.
- Preserve normal unmodified terminal text selection.
- Avoid unnecessary mouse capture.
- Handle resizing, narrow terminals, Unicode, redirected output, cancellation, and terminal restoration.
- Do not use “completed” ambiguously for prepared versus published work.
- Provide stable JSON output and useful exit codes.
- Keep implementation details out of ordinary user flows unless needed to make a decision.

The TUI is optional. Core workflows must work through ordinary commands and headless operation.

## 14. Notifications

Implement desktop and terminal notifications behind platform adapters.

Notify about material states:

- New work requiring attention.
- Incoming feedback.
- Prepared reviews awaiting publication.
- Verified publication.
- Failures requiring user action.
- Relevant stale/superseded results.

Requirements:

- Group bursts by PR.
- Deduplicate repeated observations.
- Support quiet hours and a subsequent digest.
- Honour project-specific notification settings.
- Avoid notification storms after reconnect or initial sync.
- Do not fail the watcher merely because desktop notifications are unavailable.
- Show useful repository/PR identity and the required action.
- Treat notification click actions as an enhancement, not the only decision surface.
- Every decision must remain accessible through the terminal.

## 15. Compatibility, distribution, and documentation

Implement and validate supported macOS and Linux behaviour.

Provide:

- Native binary build/release automation.
- Reproducible, documented build commands.
- Shell completion.
- Example global and repository configuration.
- Configuration schema/version documentation.
- A supported Codex version/capability policy.
- Clear installation and prerequisite instructions.
- Authentication and permission explanations.
- CLI command reference.
- Troubleshooting and recovery documentation.
- Plain-output and JSON usage examples.
- Documentation of temporary data, durable state, retention, and cleanup.
- Explicit limitations around publication races and unavailable platform notification actions.

Do not automatically install a persistent OS service.

Design service integration as optional. An explicit future service-install command may wrap the OS mechanism, but ordinary foreground use must not depend on it.

Do not claim a formal release was published unless it actually was. Build artifacts and release automation are distinct from tag/release publication.

## 16. Testing and acceptance

Follow applicable repository/global instructions and the required independent engineering lifecycle.

For non-trivial behavioural work:

- Obtain fresh independent contract mapping.
- Obtain one bounded independent QA planning artifact before production writes.
- Use test-first implementation and observe the focused regression fail for the expected reason.
- Verify the implemented behaviour.
- Obtain an independent regression-coverage audit.
- Obtain a complete final-diff correctness review.
- Obtain relevant delivery-state auditing before conclusive delivery claims.
- Preserve native role reports, identities, reviewed boundaries, findings, and dispositions.

Reuse the QA plan unless requirements, base, or strategy materially change. Do not create recursive approval or receipt machinery.

Use deterministic test seams:

- Fake GitHub API responses.
- Fake Codex protocol peers.
- A controllable clock.
- Temporary SQLite databases.
- Task-owned synthetic repositories.
- Notification adapters.

Exercise runtime behaviour rather than merely asserting configuration constants or command registration.

### Required acceptance scenarios

At minimum, prove:

1. API notify-only policy does not start a reviewer.
2. Bill automatic policy produces one eligible prepared/published review.
3. Duplicate observations do not duplicate jobs.
4. New commits invalidate an old publication decision.
5. An explicit same-head re-request can create a new run.
6. A same-head context change cannot silently reuse invalid review evidence.
7. Restart after successful submission does not blindly repost.
8. Lost publication responses produce reconciliation or an explicit unknown outcome.
9. Partial publication does not duplicate already published comments.
10. “Ask” remains pending without an attached terminal.
11. Notification expiration does not authorize action.
12. Initial sync does not unexpectedly review the backlog.
13. PR changes to .reviewd cannot enable publication or expand access.
14. Project overrides merge by field and retain explainable provenance.
15. Invalid configuration reloads preserve the last valid configuration.
16. Missing skills/models/authentication produce actionable failures.
17. Pending, failed, missing, or unavailable mandatory checks are classified correctly.
18. Interrupted or failed workers cannot become clean reviews.
19. Own-PR feedback appears without granting fix authority.
20. Self-review uses the allowed GitHub event.
21. Closed PRs cancel queued work.
22. Debounce coalesces repeated code updates.
23. Quiet hours, grouped notifications, snooze, and resume preserve inbox work.
24. Concurrent CLI/UI access does not corrupt scheduler state.
25. Model/effort overrides reach the actual Codex invocation.
26. Worker access cannot bypass application publication policy.
27. Source snapshots and process trees are cleaned up on success, failure, and cancellation.
28. TUI resizing, keyboard actions, ordinary text selection, and terminal restoration work.
29. Headless and JSON workflows expose the same meaningful states.
30. Supported-platform builds and platform-specific behaviour meet their documented claims.

Perform hands-on CLI/TUI acceptance, not only automated tests or source review.

Real Codex and GitHub integration evidence must be recorded separately from fake-protocol and mocked-API evidence. Do not conflate successful local preparation, verified publication, and hosted CI.

For every Go test/build/analyzer run, use a task-owned disposable GOCACHE and remove it after the bounded run, including failure paths. Clean up all other task-owned disposable resources when no longer needed.

## 17. Implementation sequence

Use this sequence, adjusting bounded batch sizes as evidence requires:

### Phase A: foundation and integration spike

- Verify repository and delivery target.
- Establish Go project structure and core CI.
- Record the authoritative specification and acceptance mapping.
- Prove App Server initialization, model/effort selection, skill injection, private review execution, input/cancellation handling, and access separation.
- Prove a compact prepared-review format can support preview and validation.

Do not build the whole product around an unproved publication boundary.

### Phase B: complete vertical slice

Deliver:

One configured repository
→ discovery
→ inbox item
→ private Codex review
→ preview
→ explicit publication
→ verified GitHub result

Use a controlled authorized test environment for external mutations.

### Phase C: policies and durable automation

Implement:

- Global/project/repository configuration.
- Event semantics.
- Automatic scheduling.
- Concurrency and debounce.
- Revision/context invalidation.
- CI handling.
- Crash recovery.
- Publication reconciliation.

### Phase D: complete personal workflow

Implement:

- Own-PR feedback.
- Terminal inbox and details.
- Notifications.
- Snooze and quiet hours.
- Project pause.
- Headless/JSON operation.
- Diagnostics and configuration explanation.

### Phase E: distribution and final acceptance

Complete:

- Platform adapters and supported-platform checks.
- Binary build/release automation.
- Documentation and examples.
- Hands-on acceptance.
- Final full-diff review.
- Exact-head delivery audit.
- Final push and hosted-check assessment.

Every phase must result in integrated, usable behaviour. Do not leave placeholder implementations, success-shaped fake responses, TODO-only promised features, or disconnected packages that never participate in the real workflow.

## 18. Progress and completion

Maintain a concise acceptance mapping showing which original requirements are implemented, verified, and still outstanding. Keep it proportional; do not create a parallel project-management system.

Report material findings, changes in scope, failures, and decisions promptly. Use the required status cadence and include progress toward the whole product, not merely the current batch.

Do not equate progress with:

- Commit counts.
- Test counts.
- Artifact counts.
- Lines written.
- Number of completed procedural steps.

Progress means proving the specified user workflows.

Completion requires:

- The complete specification is implemented and wired into the actual application.
- Applicable verification passes.
- Hands-on acceptance supports the stated CLI/TUI claims.
- Independent reviews have no unresolved findings.
- Documentation and distribution automation match the actual product.
- The final implementation is committed and pushed to main.
- The exact remote head and applicable hosted checks have been freshly verified.
- Every remaining external, platform, or release-publication boundary is stated accurately.

If a material requirement remains unimplemented or unverified, do not mark the entire goal complete or describe scaffolding as the finished product.

The final report must identify:

- Delivered repository and exact remote main revision.
- Implemented user-facing capabilities.
- Compact verification and independent-review outcomes.
- Build/artifact and hosted-CI state.
- Any remaining limitation, unavailable evidence, or operator action.

Continue autonomously through all authorized work until this goal is genuinely complete.
