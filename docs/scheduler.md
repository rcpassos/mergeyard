# Scheduler

`internal/scheduler.New` takes the effective config and `scheduler.Resources`
from a locked runtime: its database, shared workflow, event bus, workspace, and
scheduler control. These explicit resources keep the scheduler independent of
application startup. Pass `Runtime.Scheduler` as the control to share the
dashboard's pause/resume gate. `Run(ctx)` polls immediately and then every
`poll_interval` (default 30 seconds). `Reconcile(ctx)` advances existing runs and
returns orphan findings without dispatching new work. Every `Tick(ctx)` reconciles
before dispatch, including the first tick after startup. Ticks serialize, and run operations coordinate with lifecycle controls so takeover
or stop waits for an in-flight operation before changing state. Running tmux agents are observed without waiting
for them to finish. Tick errors are logged by `Run` and tried again at the next
interval. Cancelling the scheduler leaves agent sessions running.

`Pause(ctx, true)` prevents new claims; existing implementations continue.
`Pause(ctx, false)` resumes dispatch. These operations publish scheduler events.
`Runs(ctx)` returns durable workflow snapshots. Startup calls `Reconcile(ctx)`
before enabling new claims and reports its findings. The CLI starts this
scheduler and shares its controls with the dashboard.

`Stop(ctx, runID)` serializes with phase advancement and first persists stop
intent. It interrupts running attempts through the runner, preserves the
worktree/branch/PR, removes ready and running labels, adds attention if work was created,
and transitions to `STOPPED`. Failed interruptions or label writes leave intent
pending; subsequent ticks and startup reconciliation finish the stop before
advancing work. Successful stops are idempotent. `Watch(ctx, runID)` returns the
live attempt of the current automated phase, rejecting missing or finished
sessions.

Repositories are visited in configuration order. Issues are ordered by creation
time and then issue number. Only open ready issues without running/attention
labels, unresolved native GitHub blockers, or an existing nonterminal run are
claimed. Dependency API errors fail closed. Body text is never interpreted as a
dependency. Global and repository limits count all nonterminal runs except
`READY_TO_MERGE`, including attention, manual and waiting states. M1 does not
apply usage-limit scheduling.

Claims first persist `CLAIMING`, then add running, remove ready, persist
`PREPARING`, and prepare the managed base/worktree. Label failure aborts the
flow. Git ownership and issue context are retained in `scheduler_runs`; the
branch/worktree and implementer session UUID are also retained on the run. Each
attempt gets an input file, a durable tmux identity, logs and exit metadata.
Completed native structured output is validated and saved as `result.json` by
the control plane. This file is an archival report, not an agent-written
fallback result contract. Agent failures and invalid results can retry within
`implementer.max_attempts`; blocked results require attention immediately.

Claude and Codex implementers are supported; review currently requires Claude.
Codex emits its identity through `thread.started`. Local observation every 250 ms
persists that UUID and publishes `harness.session_discovered` without waiting for
the GitHub polling interval. Reconciliation also recovers identity from captured
output after a restart. Attempt and tmux identities are persisted before launch.

Codex uses native `exec --json --output-schema … -o …`, with cwd and sandbox flags
before an exact-ID `resume`. Every invocation supplies model, effort, skills,
sandbox and network settings. Approval policy is `never`; the selected sandbox
remains enforced. The network toggle applies to `workspace-write`;
`danger-full-access` permits networking regardless of that toggle, and snapshots
show its effective network permission as `true`. Full issue context and schema live outside the worktree. Only
exit 0, `turn.completed`, and a valid native `last-message.json` from the unique
attempt can succeed. A verified missing rollout requires attention and keeps its
UUID; fresh-session recovery belongs to a later slice. Changing the implementer
harness on restart requires attention before another attempt can launch, so a
retained UUID cannot be sent to the other harness.

Implementer snapshots retain the harness, UUID, model, effort, skills, permissions,
attempt and process identity in dashboard and status output, including during review. Supply `Dependencies.Env` explicitly with
the environment the harness needs, for example `PATH`, `HOME`, `LANG`, and
`TMPDIR`. It is the complete child environment, not an implicit copy of the
control-plane environment. Dependencies default to the real GitHub, managed Git,
and local tmux adapters.

After implementation, Mergeyard commits changes, pushes without force, discovers
an existing PR or creates a draft, and persists its number and URL. The persisted
`ACTIVE/review`, round 1 endpoint continues on the next tick into an independent
Claude review. Approval waits for CI; changes required enters `ACTIVE/fix` and continues the
bounded fix/re-review loop for Claude implementers. Codex fix execution requires
attention until its separate implementation slice. CI monitoring, merge detection and user retry belong
to later slices. These states continue consuming a concurrency slot. Repeated ticks and
scheduler restarts observe persisted attempts and do not launch duplicates.

Blocked/invalid results, exhausted attempts, empty implementations, branch
conflicts and other system-step failures enter `NEEDS_ATTENTION`. The scheduler
removes running, adds attention, and preserves code and diagnostics. Failed
attention-label writes are retried on later ticks; ready is never re-added.
A missing/ambiguous process is left for human inspection rather than relaunched.

## Restart reconciliation

Reconciliation reads current issue state and labels, verifies persisted Git
ownership and the worktree's branch without fetching or recreating it, reads the
PR (including closed PRs), and observes persisted running tmux attempts. An agent
still running stays under observation. An agent that exited while the control
plane was offline is recovered through `exit.json` and native result output; the
usual implement flow archives `result.json`, pushes, and reuses an existing PR.
Reconciliation failures prevent new dispatch. Closed issues/PRs, missing worktrees,
and missing sessions require attention. Manual and attention runs are never
automatically resumed. First review progresses through the same reconciliation boundary. CI/merge
progression remains outside this slice.

`mergeyard reconcile [--config <path>]` loads the selected configuration and takes
exclusive ownership of its workspace. It uses the same reconciliation boundary,
without claiming new work. Stop the control plane first if it holds the lock.
The command prints `reconcile.orphaned_claim`, `reconcile.orphaned_worktree`, and
`reconcile.orphaned_session` findings, also published to the durable event stream.
It scans every configured repository even when dispatch is paused, disabled, or
at capacity. Running labels are never silently reset on orphaned issues. Worktree
directories, registered worktree metadata, and managed tmux sessions without
persisted ownership are reported and preserved. Terminal runs' retained artifacts
remain owned. Startup uses the same boundary before serving the dashboard and
running the scheduler.

## Tests

The normal scheduler tests use real SQLite, Git and tmux with fake Claude and Codex
executable, plus fake GitHub responses. They cover claim order and failure,
blocking, ordering, concurrency, retries, pause, attention, and duplicate
prevention after restart. They require `git` and `tmux` but no agent credentials.
Reconciliation tests kill a real control-plane subprocess during implement,
recover agents still running or finished offline, and verify one attempt, commit,
and PR. They also cover orphan reporting and preservation, unrecoverable claims,
and missing owned worktrees. CLI tests use fake GitHub/tmux commands to verify
configuration selection, reporting, and workspace lock release.

The live test is opt-in and uses only the issue explicitly provided. Prepare a
dedicated `owner/mergeyard-integration-test` repository with an initial commit,
draft PR support and the three standard labels. Create an open, unblocked test
issue labelled `ready-for-agent`. Its `mergeyard/issue-<number>` branch and PR
must not already exist. Configure local Git identity and GitHub authentication.

```sh
MERGEYARD_SCHEDULER_INTEGRATION=1 \
MERGEYARD_GITHUB_TEST_REPO=owner/mergeyard-integration-test \
MERGEYARD_GITHUB_TEST_ISSUE=1 \
go test ./internal/scheduler -run TestGitHubSchedulerIntegration -v
```

The test exercises GitHub labels and draft PR creation with real Git/tmux and a
fake Claude executable that edits a file. It verifies one run, worktree, commit,
and PR through repeated ticks. Cleanup closes the test PR, deletes its test
branch and restores the issue's ready label. Interrupted cleanup can require
manual removal of these test artifacts.

## Dashboard discovery and Stop

`Queue(ctx)` reads ready/blocked/attention issues from enabled repositories in
configuration and issue creation order. It does not mutate labels or claim runs,
and works independently of pause and concurrency. Dependency lookup failures
return an error rather than reporting an issue as unblocked.

`Stop(ctx, runID)` shares the workflow operation lock with dispatch. It preserves
Git artifacts, terminates running attempts, removes ready and running labels,
and adds attention when work exists before marking the run STOPPED. Stop intent
is committed with `run.stop_requested`; failures remain available for browser
retry and are retried before normal advancement. An already-stopped run is a
no-op, including remote labels, so replaying Stop cannot affect a newer run.
Completed and failed runs reject Stop. Browser and CLI callers share this
operation.

Each implement attempt is persisted with its model, effort, and skills snapshot
and a `phase.attempt_started` event in one transaction before launch. The event
includes phase, round, and attempt, and notifies pages on every retry without
requiring a workflow state transition.

## Independent review

Each attempt stores a distinct reviewer session, configured model/effort/skills,
permissions, PR head SHA, pinned base-to-head diff and exact pre-review Git
snapshot before tmux launch. Claude edit tools are disabled; Bash remains available
for tests. Native reports require approved/changes_required/blocked/failed and
unique finding IDs, severities and nullable locations. Only blocking findings
prevent approval; contradictory reports are invalid.

After the reviewer exits, contamination is journaled before restoring tracked
and non-ignored files, the staged index and branch HEAD. Ignored test/build output
is retained. Changes or commits invalidate the report even after restoration;
review retries stay in the same round and obey reviewer.max_attempts. Changed
branches, unsafe filesystem shapes, missing sessions, closed PRs/issues or a
changed external PR head require attention and preserve ambiguous work.

SQLite atomically accepts the restored report, finishes its attempt, persists
approved_sha and advances workflow with review.completed. The status API and CLI
include the latest reviewer settings, report and diagnostics. Stop interrupts
review and restores its owned changes before stopping; Watch observes its live
tmux process. Restart replays pending restoration before any verdict acceptance,
including when Git was restored but the journal commit did not finish.

A missing Claude transcript during a review retry replaces the reviewer UUID and
uses a fresh session within reviewer.max_attempts. The attempt-start event retains
the previous session ID and a harness.session_resume_failed warning code.

## Fix and re-review

A changes-required verdict resumes the original implementer's conversation with
fresh issue/PR context and the accepted blocking findings. Invocation settings are
reapplied. The strict fix report includes success/blocked/failed, a summary, and
per-finding responses (`finding_id`, fixed/disputed `resolution`, and a `note`).
Successful reports account for every supplied blocker exactly once. Unknown,
duplicate or missing responses and claims of fixes without changes are invalid.
Blocked output requires attention; failures retry within implementer.max_attempts
(default one). Attempt retries stay in the same review round.

After success, Mergeyard commits tracked and non-ignored new files, retaining any
legitimate implementer commits. A dispute-only fix can proceed without a commit.
The commit SHA is journaled before a non-force push. Remote divergence, changed
PR identity, or edits after that commit is pinned require attention and preserve
work. Restart can replay commit or push acknowledgement without duplicate commits
or agent attempts, including a push that succeeded before the process died.

Publication advances the review round exactly once and clears stale approval.
The reviewer resumes its separate session with complete issue/PR/diff context,
prior findings and the fix report. It independently adjudicates disputes and
returns the complete remaining blocking list. Approval enters WAITING_FOR_CI;
it does not make the draft ready. No fix starts without a subsequent review
round. Exhausting max_rounds (default five) persists review.max_rounds_exceeded
and requires attention, retaining code, reports and diagnostics. Explicit extra
round grants belong to the later retry slice.

Run detail, status and events retain fix settings, findings, responses and
publication progress across rounds. Stop uses the shared operation gate during
fix/commit/push, preserves implementer edits, and prevents subsequent review.
The ordinary suite covers the loop with fake harnesses and GitHub responses,
real managed Git/tmux, and subprocess kills at the fix/publication boundaries.

## CI and readiness

An accepted review starts `WAITING_FOR_CI` for its approved commit. Every reported
check (including optional checks), legacy status, and required check must pass.
Requirements are read from classic branch protection and active inherited rulesets;
permission, transport, malformed response, and unsupported required-workflow rules
remain unknown and block readiness. Native skipped/neutral conclusions remain visible
and are accepted under GitHub's documented status-check policy. Missing required
checks, stale evidence, and pending/unknown outcomes block the gate.

Only successful queries establishing no checks and no requirements allow readiness
after two minutes from the saved wait start. `ci_timeout` defaults to 60 minutes.
Expiry requires attention with diagnostics and never launches a code fix.
Failed/timed-out outcomes are saved for the dependent CI repair ticket; until that
slice lands, `ci.repair_unavailable` requires attention without starting a fix.
Canceled/action-required outcomes also require attention.

The review target, approved commit and fresh PR head must agree before readiness.
An external push before readiness invalidates approval and preserves the run for
explicit retry. Mark-ready intent is saved before converting a draft; an uncertain
response is reconciled through fresh PR reads, never blindly replayed. An already
normal PR needs no write. `READY_TO_MERGE` displays “Waiting for your merge” and
releases capacity. A later push produces a warning; closure without merge requires
attention. Mergeyard never merges a PR. CI/readiness reconciliation continues while
new claims are paused. Manual merge completion and explicit retry remain dependent
M2 slices.
