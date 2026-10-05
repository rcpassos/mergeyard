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

Claude and Codex are supported independently for implementation, review and fixes.
Codex emits its identity through `thread.started`. Local observation every 250 ms
persists each role's UUID during implementation, review or fixes and publishes
`harness.session_discovered` without waiting for the GitHub polling interval. Reconciliation also recovers identity from captured
output after a restart. Attempt and tmux identities are persisted before launch.

Codex uses native `exec --json --output-schema … -o …`, with cwd and sandbox flags
before an exact-ID `resume`. Every invocation supplies model, effort, skills,
sandbox and network settings. Approval policy is `never`; the selected sandbox
remains enforced. The network toggle applies to `workspace-write`;
`danger-full-access` permits networking regardless of that toggle, and snapshots
show its effective network permission as `true`. Full issue context and schema live outside the worktree. Only
exit 0, `turn.completed`, and a valid native `last-message.json` from the unique
attempt can succeed. A verified missing rollout requires attention and keeps its
UUID during implementation. Review/fix retries can start a fresh conversation
within the configured role attempt limit after a verified missing-session
diagnostic. Changing either role's harness on restart requires attention before
that role can launch another attempt, so a retained UUID cannot be sent to the
other harness.

Implementer snapshots retain the harness, UUID, model, effort, skills, permissions,
attempt and process identity in dashboard and status output, including during review. Supply `Dependencies.Env` explicitly with
the environment the harness needs, for example `PATH`, `HOME`, `LANG`, and
`TMPDIR`. It is the complete child environment, not an implicit copy of the
control-plane environment. Dependencies default to the real GitHub, managed Git,
and local tmux adapters.

After implementation, Mergeyard commits changes, pushes without force, discovers
an existing PR or creates a draft, and persists its number and URL. The persisted
`ACTIVE/review`, round 1 endpoint continues on the next tick into an independent
review by the configured reviewer. Approval waits for CI; changes required enters
`ACTIVE/fix` and continues the bounded fix/re-review loop with the configured
implementer. Explicit user retry remains planned. These states continue consuming
a concurrency slot. Repeated ticks and
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
for tests. Codex retains its configured sandbox and network settings so tests and
builds can produce ignored output. Use writable, ignored in-worktree cache paths
when the sandbox denies access to host caches. Both harnesses use the same
restoration and contaminated-verdict policy. Native reports require approved/changes_required/blocked/failed and
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

A verified missing Claude transcript or Codex rollout during a review retry
starts a fresh reviewer conversation within reviewer.max_attempts. Codex discovers
and persists the new identity from its stream. Authentication, model and
configuration errors do not trigger fresh-session recovery. The attempt-start event retains
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

CI observes both the approved head and GitHub's current test merge commit. The
adapter verifies the merge parents against the current base and head before
accepting its checks/statuses. All reported outcomes participate; when the merge
commit has reported CI, required contexts must be satisfied on that commit. With
no merge reports, required contexts use the head. Approval always remains pinned
to the reviewed head. Missing merge metadata, query failures, stale merge parents,
and base/merge changes during observation block readiness without renewing the wait.

Legacy context identity is case-insensitive: the latest `CI` status replaces an
older `ci` status while preserving the latest display name and diagnostics.
For app-bound legacy statuses, the adapter verifies GitHub's registered app ID
and associated bot user ID against the status creator. Wrong apps cannot satisfy
the requirement, and missing/unverifiable identity remains unknown. A same-named
check run cannot supply source authorization for a legacy status.

Only successful queries establishing no checks and no requirements allow readiness
after two minutes from the saved wait start. `ci_timeout` defaults to 60 minutes.
Expiry requires attention with diagnostics and never launches a code fix.
Failed checks (`ci.check_failed`) and terminal check timeouts
(`ci.check_timed_out`) enter the implementer fix phase only when another review
round remains. Canceled/action-required outcomes take priority and require
attention without a fix. Unreadable evidence keeps the original bounded wait.
Mergeyard never automatically reruns checks.

CI repairs retain the approved target, check names, native outcomes, available
check output/status descriptions, URLs, and wait timestamps. The implementer
receives this context alongside the current issue and PR. A CI-only success
report uses `responses: []`; invented finding responses and success without
code changes are invalid. Fix attempts use `implementer.max_attempts`, and the
next independent review uses `reviewer.max_attempts`. Neither retry spends an
extra review round. Exhausted rounds require `review.max_rounds_exceeded` before
any repair starts.

Entering repair clears approval. The existing pinned commit/push journal handles
restart recovery, then advances exactly one review round. The reviewer receives
the repair evidence and summary with the new complete diff. New CI is considered
only after that independent review approves the new commit and starts a fresh
wait. Previous repair diagnostics remain in fix history after the new CI wait
replaces the failed observation, including in run detail, status, and events.

The review target, approved commit and fresh PR head must agree before readiness.
An external push before readiness invalidates approval and preserves the run for
explicit retry. Mark-ready intent is saved before converting a draft; an uncertain
response is reconciled through fresh PR reads, never blindly replayed. An already
normal PR needs no write. `READY_TO_MERGE` displays “Waiting for your merge” and
releases capacity. A later push produces a warning; closure without merge requires
attention. Mergeyard never merges a PR. CI/readiness reconciliation continues while
new claims are paused. Explicit retry remains a dependent M2 slice.

## Review and fix report comments

With `pr_comments: true` (the default), every locally saved review/fix report is
published as a separate PR conversation comment. Comments show the round and
attempt, review verdict/findings or fix summary/responses, and whether a review
was accepted. Report text is literal JSON in a fenced block, with HTML and
backticks escaped. Every review attempt and fix remains visible in run detail
and status, including when comments are disabled.

A durable publication queue freezes the comment body, run/phase/report identity
and PR destination before calling GitHub. Each attempt records its timestamp and
count; a successful publication records GitHub's comment ID and URL. The adapter
reads all comment pages for the identity marker before a single POST, including
on retries after transport failures or malformed responses. It never edits
matching comments, preserving later human additions. Successfully acknowledged
comments are never replayed, even if subsequently edited or removed by a human.

Posting failures emit a safe `publication.warning` code and persist a separate
warning in run detail/status. They do not set workflow errors or stop phases,
CI, readiness or completion. Optional network publication has a five-second
budget per reconciliation and retries on later scheduler ticks. Reconciliation
includes pending reports for completed/failed/stopped runs and does not need an
agent or worktree to render or retry them. Disabling `pr_comments` suspends all
external comment calls while retaining pending bodies and warnings; enabling it
on a later restart publishes the backlog. Existing saved reports without queue
entries are also recovered, including reports saved while the control plane was
offline. Publication emits `publication.pending` and `publication.published`
events alongside warnings.

## Manual merge completion

A GitHub merge observed in any nonterminal run with a PR is authoritative,
including during review, fixes, CI wait, manual control, or attention. Mergeyard
records early merges when readiness was not reached. Closed PRs without a merge
retain their work and require attention. Terminal stopped runs remain stopped.

Merge observation and stop intent commit before any further phase launch or push.
The shared run-operation gate serializes merge handling with stop and phase work.
Owned phases journal their process group and boot/leader identity before harness
launch. New attempts record this journal requirement before their running intent;
a missing group journal with absent tmux therefore identifies a harness that never
started. Missing identity on older, unjournaled sessions requires inspection.
Stop reconciles that group even if its wrapper or tmux session has exited,
and verifies group exit after signal escalation. Owned phases are stopped and
checked for exit before coding becomes `COMPLETED`
and releases its consumed capacity. Merge completion never becomes `STOPPED`.

A durable maintenance record tracks each label removal, closure of an issue that
is still open, review workspace restoration, worktree removal/pruning, and local
branch deletion. Maintenance continues for completed runs and while claims are
paused. Pending cleanup excludes the source issue from redispatch but allows
unrelated eligible issues to start. Remote branches are retained.

Run detail, the queue, and status show “Merged — cleanup pending”, early-merge
information, the concrete remaining actions, and the latest maintenance error.
Transient failures retry on later ticks and after restart. Dirty work
is preserved for human attention; cleanup never force-removes a worktree. Review
restoration must succeed before deletion, preserving preexisting human edits.

Worktree removal intent is recorded after ownership inspection and before removal.
A missing worktree with recorded intent can be reconciled as an interrupted
cleanup; unexplained disappearance requires inspection and retains the branch.
Already absent labels, closed issues, removed trees, and deleted local branches
are tolerated at their recorded boundaries. Independent report publication keeps
retrying for completed runs after Git cleanup finishes.
