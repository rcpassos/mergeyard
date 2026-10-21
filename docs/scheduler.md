# Scheduler

`internal/scheduler.New` takes the effective config and `scheduler.Resources`
from a locked runtime: its database, shared workflow, event bus, workspace, and
scheduler control. These explicit resources keep the scheduler independent of
application startup. Pass `Runtime.Scheduler` as the control to share the
dashboard's pause/resume gate. `Run(ctx)` polls immediately and then every
`poll_interval` (default 30 seconds). `Reconcile(ctx)` advances existing runs and
returns orphan findings without dispatching new work. Every `Tick(ctx)` reconciles
before dispatch, including the first tick after startup. Ticks serialize through
a gate that honors cancellation: callers wait only while their context lives, so
shutdown and dashboard actions are never blocked indefinitely by a stalled tick.
Run operations coordinate with lifecycle controls so takeover
or stop waits for an in-flight operation before changing state. Running tmux agents are observed without waiting
for them to finish. Tick errors are logged by `Run` and tried again at the next
interval. Cancelling the scheduler leaves agent sessions running.

Every `gh` and `git` call on the scheduler path has a deadline. A first clone
gets its own longer budget (`1h`), routine fetches and remote reads get `10m`,
worktree operations get `10m`, and a publication push including its pre-push
hook gets `30m`; local reads get the shortest budget. A commit keeps its own
budget while the remote read and the push keep theirs. `gh` shares one
two-minute budget across a request's retries. A deadline ends the owned process
group and surfaces as `git.timeout` or `github.timeout`, naming the operation
and, for a phase deadline, the budget so the dashboard explains it; the
existing retry and attention paths handle the failure. Git never prompts: SSH uses batch mode with connect timeout
and keepalive options unless the machine selects its own transport. See
[git.md](git.md) for the operation budgets.

`Pause(ctx, true)` prevents new claims; existing implementations continue.
`Pause(ctx, false)` resumes dispatch. These operations publish scheduler events.
`Runs(ctx)` returns durable workflow snapshots. Startup calls `Reconcile(ctx)`
before enabling new claims and reports its findings. The CLI starts this
scheduler and shares its controls with the dashboard.

Startup treats a reconcile failure as retryable whenever it belongs to the
GitHub boundary of one repository or run: unreachable, rate limited, cancelled,
an unclean response, or a conclusive missing repository, permission, login, or
input answer. Listing issues, reading or writing labels, comments, or pull
requests can fail that way without stopping the dashboard; the failure is printed
and published as a finding, and the running loop retries it on later ticks. A
coded wrapper classifies by its cause, so pending handback publication wrapped
around a GitHub failure is deferred too. Only local-state failures (database,
workspace, Git, runner, or harness) abort startup. No issue is claimed before a
reconcile pass finishes; a scoped failure skips only its repository or run.
Disabled repositories are still scanned for reconciliation findings, but their
GitHub failures never block dispatch or startup.

`Stop(ctx, runID)` serializes with phase advancement and first persists stop
intent. It interrupts running attempts through the runner, preserves the
worktree/branch/PR, removes ready and running labels, adds attention if work was created,
and transitions to `STOPPED`. Failed interruptions or label writes leave intent
pending; subsequent ticks and startup reconciliation finish the stop before
advancing work. Successful stops are idempotent. `Watch(ctx, runID)` returns the
live attempt of the current automated phase, rejecting missing or finished
sessions.

A run whose Git ownership was never saved can still own a branch and a worktree
from an interrupted creation, whatever state it stopped in (a failed Prepare
leaves NEEDS_ATTENTION and a Retry leaves CLAIMING, both with the preparation
intent in place), and the next run for the same issue
has a different ID, so it cannot recover them. Stop considers that preparation
before it stops: the leftovers are released when they are proven to hold no
work, so the next run prepares instead of refusing them. Leftovers with commits,
changes, or ignored files are kept, never deleted, and so is a checkout Git never finished
initializing while its directory holds anything beyond the recorded start: the
run records them, the issue is flagged for attention, and a comment names the
branch and the worktree the user has to remove. Leftovers Stop cannot check (an
empty or corrupt preparation intent, a missing or replaced managed base, a Git
check that fails or exceeds its deadline) are kept and flagged the same way,
and the comment says they could not be verified; Stop still finishes, so a
stuck check never leaves the stop request pending. The comment may land on a
public issue, so it names the worktree relative to the Mergeyard workspace and
never posts an absolute local path; run detail and the `run.stop_leftovers`
event keep the full path. The preparation of a run that already saved its
ownership is never released, because that work belongs to the run. The
leftovers are recorded in the `run.stop_leftovers` event, whose `unverified`
field marks leftovers that could not be checked.

Repositories are visited in configuration order. Issues are ordered by creation
time and then issue number. Only open ready issues without running/attention
labels, unresolved native GitHub blockers, or an existing nonterminal run are
claimed. Dependency API errors fail closed. Body text is never interpreted as a
dependency. Global and repository limits count all nonterminal runs except
`READY_TO_MERGE`, including attention, manual and waiting states. Account-wide
harness gates withhold new claims and phase launches using the limited harness;
eligible other-harness work and CI observation continue within capacity.

Each poll reads one listing per repository, filtered server-side to that
repository's ready, running and needs-attention labels. GitHub ANDs the labels of
one request, so the adapter reads one query per label and merges the pages by
issue number; the listing never pages through every open issue and PR.
Reconcile and dispatch share the poll's snapshot and the dashboard queue reuses
it, so a repository is listed once per poll instead of once per reader; an
explicit `Plan` read starts its own poll. Concurrent readers share one fetch per
repository instead of racing duplicates, and a fetch that began before a new
poll cannot replace that poll's listing. Waiting for a shared fetch honors the
caller's context: a reader that gives up is released instead of fetching again
with an expired context. A snapshot stays reusable for two poll intervals: one
covers the scheduler's own refresh and the second absorbs timer skew between the
scheduler and the dashboard. A reader outside a scheduler poll (the dashboard
without a running scheduler) refreshes its own listing once the window expires.
A dashboard pass that spends its refresh budget waiting for a listing returns
the last usable queue marked stale, so it does not replace discovery with an
error. Readers never mutate the shared snapshot.

`Plan(ctx, repository)` uses the same read-only candidate inspection and pure
capacity/label eligibility policy as `Tick`. It reserves prospective slots in
memory and returns every open ready issue's decision without reconciliation or
claim effects. `PlanOffline` uses an existing read-only store (or no store for a
fresh workspace). The CLI and local `GET /api/plan` expose the versioned snapshot;
see [CLI operations](cli.md) for the JSON contract and reason precedence.

Each issue resolves its implementer and reviewer through
`config.ResolveRoleSettings`: fixed, case-insensitive
`mergeyard:implementer=<agent>` and `mergeyard:reviewer=<agent>` labels override
the repository harness. The implementer account gate runs per issue; restricted
issues are skipped without preventing later issues in the repository from being
considered. A restricted reviewer waits at its phase gate.

Candidate inspection reads the implementer gate before the expensive per-issue
trust, blocker and fresh-issue calls, because a gated candidate is never claimed
and would otherwise respend them on every poll for the whole limit. The early
gate resolves the implementer from the labels the listing already carries, and
decides only whether to wait: it never supplies the run's settings, which stay
resolved from the claim snapshot as [ADR-0001](adr/0001-role-settings-resolved-at-claim.md)
requires. When the listing reports the implementer available, the gate is
rechecked on the fresh pre-claim issue, whose labels can name a different
harness. A candidate the listing reports as limited waits for the next poll
instead of reading an issue it cannot claim.

That ordering also defers work a gated candidate would otherwise do. An untrusted
or unverifiable issue keeps its ready label until the harness is available
instead of being refused during the limit. An issue that was already refused and
has its ready label back is the exception: it still spends the cached trust
verification, so a trusted relabel clears the refusal and an untrusted one is
refused again, instead of the dashboard showing a stale refusal for the whole
limit. An override or role configuration that does not resolve is still refused
from the fresh issue. None of these paths can claim anything, and each resumes on
the first poll once the harness is available.

Claims first persist `CLAIMING` with both resolved roles and their harness
sources (`label` or `config`), then add running, remove ready, persist
`PREPARING`, and prepare the managed base/worktree. Label failure aborts the
flow. Git ownership and issue context are retained in `scheduler_runs`; the
branch/worktree and implementer session UUID are also retained on the run. The
preparation intent outlives the worktree creation and is released once the
run's Git ownership is saved, so an interrupted ownership save still recovers
the run's own branch and worktree on the next attempt instead of refusing
them. A resumed run repeats that release before it continues, so a release
that failed once does not leave a stale claim behind. Each attempt gets an
input file, a durable tmux identity, logs
and exit metadata.
Completed native structured output is validated and saved as `result.json` by
the control plane. This file is an archival report, not an agent-written
fallback result contract. Agent failures and invalid results can retry within
`implementer.max_attempts`; blocked results require attention immediately.

Claude and Codex are supported independently for implementation, review and fixes.
Codex emits its identity through `thread.started`. Local observation every 250 ms
persists each role's UUID during implementation, review or fixes and publishes
`harness.session_discovered` without waiting for the GitHub polling interval. Reconciliation also recovers identity from captured
output after a restart. Attempt and tmux identities are persisted before launch.
Continuation and review read the structured report saved on the attempt, so a
missing or cleaned phase directory does not change the decision; `result.json`
remains only an archive.

Codex uses native `exec --json --output-schema … -o …`, with cwd and sandbox flags
before an exact-ID `resume`. Every invocation supplies model, effort, skills,
sandbox and network settings. Approval policy is `never`; the selected sandbox
remains enforced. The network toggle applies to `workspace-write`;
`danger-full-access` permits networking regardless of that toggle, and snapshots
show its effective network permission as `true`. Full issue context and schema live outside the worktree. Only
exit 0, `turn.completed`, and a valid native `last-message.json` from the unique
attempt can succeed. A verified missing rollout allows one fresh conversation
per role, phase, and round without spending a normal attempt. Its prior UUID and
diagnostic remain in the recovery journal. Stored role settings are fixed for
the run, including after restart, Retry, usage-limit resume, recovery probes,
takeover and handback. Configuration or override-label changes apply to later
claims. Legacy runs without stored settings still read configuration and require
attention if a configured harness no longer owns the retained session.

Implementer snapshots retain the harness, UUID, model, effort, skills, permissions,
attempt and process identity in dashboard and status output, including during review. Supply `Dependencies.Env` explicitly with
the environment the harness needs, for example `PATH`, `HOME`, `LANG`, and
`TMPDIR`. It is the complete child environment, not an implicit copy of the
control-plane environment. Agent launches additionally receive `MERGEYARD_RUN_ID`,
`MERGEYARD_PHASE`, `MERGEYARD_PORT_BASE`, and `MERGEYARD_PORT_COUNT` from the run's
persisted port block; these reserved values override entries in `Dependencies.Env`.
See [agent isolation configuration](configuration.md#agent-ports-and-session-isolation).
The `start` and `reconcile` CLI commands explicitly populate `Dependencies.Env`
with their full inherited environment; there is no CLI environment allowlist.
System steps receive `Dependencies.Env` as supplied. All tmux launches, including
harness checks, deliver quoted environment assignments through a transient
owner-only (`0600`) `environment.sh` file in the phase directory. The wrapper
loads and unlinks it before invoking `env -i`; values never enter tmux or
long-lived wrapper argv, and bypass tmux's command-size limit. Start waits for
consumption; failure, Stop, and reconciliation of exited/missing sessions clean
up leftovers. Generated wrappers contain no environment values; harness output
remains unredacted. Restart/reconciliation still uses the same
session references and process/exit journals, including for sessions launched
with older wrappers. Older wrappers are not scrubbed by this change.

Dependencies default to the real GitHub, managed Git,
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
Local-state reconciliation failures prevent new dispatch. A GitHub failure is
scoped to the repository whose listing failed or the run whose issue/PR/label
read failed: reconciliation reports `reconcile.repository_failed` or
`reconcile.run_failed`, dashboard discovery reports `reconcile.issue_failed`
when one refused issue has no snapshot to show after a failed read, and each
appears in the needs-attention section. A scoped failure skips only that
repository or issue while other work continues, and stays visible until a later
pass observes the subject successfully. Findings persist only the fixed GitHub code and a safe human
message; raw `gh` diagnostics stay on the error for application logs. Closed
issues/PRs, missing worktrees, and missing sessions require attention. Manual and attention runs are never automatically
resumed. First review progresses through the same reconciliation boundary. CI/merge
progression remains outside this slice.

`mergeyard reconcile [--config <path>]` loads the selected configuration and takes
exclusive ownership of its workspace. It uses the same reconciliation boundary,
without claiming new work. Stop the control plane first if it holds the lock.
The command prints `reconcile.orphaned_claim`, `reconcile.orphaned_worktree`, and
`reconcile.orphaned_session` findings, plus scoped GitHub failures, also published
to the durable event stream. It scans every configured repository even when
dispatch is paused, disabled, or at capacity. Running labels are never silently
reset on orphaned issues. Worktree directories, registered worktree metadata,
and managed tmux sessions without persisted ownership are reported and preserved.
Terminal runs' retained artifacts remain owned. Startup uses the same boundary
before serving the dashboard and running the scheduler; scoped GitHub failures
are reported and left for later ticks, while local-state failures still refuse
to start.

## Tests

The normal scheduler tests use real SQLite, Git and tmux with fake Claude and Codex
executable, plus fake GitHub responses. They cover claim order and failure,
blocking, ordering, concurrency, retries, pause, attention, and duplicate
prevention after restart. They require `git` and `tmux` but no agent credentials.
Reconciliation tests kill a real control-plane subprocess during implement,
recover agents still running or finished offline, and verify one attempt, commit,
and PR. They also cover orphan reporting and preservation, unrecoverable claims,
and missing owned worktrees, and that a deferred GitHub failure still gates
claims until a reconcile succeeds. CLI tests use fake GitHub/tmux commands to
verify configuration selection, reporting, and workspace lock release, and that
startup defers a retryable GitHub reconcile failure to later ticks while a
local-state failure refuses to start.

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
configuration and issue creation order, from the scheduler's shared poll listing.
An issue refused earlier no longer carries a Mergeyard label, so it is read back
individually and stays visible with its reason until a trusted maintainer
relabels it. The readable snapshot is reused across refreshes: only a changed
refusal or the 15-minute window spends another read, while a relabelled issue
returns to the listing. Refusals whose issue is closed or deleted are forgotten
when that snapshot refreshes. A failed refresh keeps the last snapshot, or
records the issue's scoped failure until a snapshot exists, and backs off for
the same window instead of retrying on every pass. Each refused read gets a
bounded child context, so a slow request cannot consume the budget reserved for
the rest of discovery. A read that the refresh budget ended before the full
read window is merely slow and is read again on the next refresh. A timeout
that GitHub reports before the budget ends the read is GitHub's own failure and
backs off for the full window, however small the repository's share of the
budget is, so the number of enabled repositories does not change it.

The refused reads share the caller's refresh budget with the rest of the pass:
they may spend at most half of it, and each repository may spend a share of
what is left, split among the repositories this pass has not discovered yet.
Once that share is spent the pass stops reading and keeps the last readable
snapshot of the refusals it did not reach, so a window full of refusals cannot
spend the whole refresh and leave the other repositories without a queue. The
refusals of a repository are read in turn, so a refusal that always spends the
repository's share cannot keep the refusals behind it out of the queue for
good. The local lookups that assemble the queue do not share the budget
reserved for GitHub, so a spent budget costs the refresh its fresh refusals
rather than its whole queue, and a repository the refresh could not list is
reported on its own instead of failing the queue.

Open blockers are reused while the issue's snapshot and open-blocker count are
unchanged, and a listing summary that reports no open blockers needs no
dependency read at all; a missing summary falls back to reading, so an unknown
status is never shown as ready. `Queue(ctx)` does not mutate labels or claim
runs, and works independently of pause and concurrency. Dependency lookup
failures return an error rather than reporting an issue as unblocked.

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

A verified missing Claude transcript or Codex rollout during resumed review
allows one fresh reviewer conversation per round, separate from
`reviewer.max_attempts`. Codex discovers and persists the new identity from its
stream. Authentication, model and configuration errors do not trigger recovery.
The `harness.session_resume_failed` event retains the previous session ID and
diagnostic, and run detail/status show the warning and active identity.

## Fix and re-review

A changes-required verdict resumes the original implementer's conversation with
the authorized issue snapshot, fresh PR context and the accepted blocking findings. Invocation settings are
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
receives this context alongside the authorized issue snapshot and current PR. A CI-only success
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
CI, readiness or completion. Optional network publication shares a five-second
budget across each batch of writes. A transient failure waits instead of
retrying on the next tick: the backoff grows from one minute up to one hour.
Reconciliation reaches the pending reports of completed, failed and stopped
runs and needs no agent or worktree to render or retry them, but a finished run
stops after a returned failure, so a never-attempted publication or an
interrupted attempt still gets its chance and an ambiguous write can be
reconciled. Disabling `pr_comments` suspends all external comment calls while
retaining pending bodies and warnings; enabling it on a later restart
publishes the backlog. Existing saved reports without queue entries are also
recovered, including reports saved while the control plane was offline.
Publication emits `publication.pending` when a report is queued,
`publication.warning` once per source and code, `publication.published` on
delivery, and `publication.stopped` when a stop is recorded after an earlier
warning or when a finished run stops a publication that already failed. The
pull request body keeps its own names, `verification.warning` and
`verification.published`, and records a stop the same way. The escalation
section records `escalation.updated`.

A permanent answer stops the publication for good and keeps its reason. The
permanent answers are a missing or forbidden target, invalid input such as a
size or field-validation rejection, an unsupported write and an ambiguous
match; a refusal GitHub did not explain with a permanent cause stays transient
and backs off like any other failure, including the secondary rate limit
GitHub applies to a comment created too quickly. The same policy governs pull
request bodies, attention comments and the escalation section, but a requeue
does not return a stopped escalation section to the queue. Stopped report
comments, attention comments and pull request bodies stay visible in run
detail with that reason until a human asks for another attempt:
`mergeyard requeue <run-id>`, the dashboard's requeue action, or a Retry
request. The requeue clears the stop and the reason, restarts the backoff, and
lets the next reconciliation deliver the frozen content again. The identity
marker still finds a comment that already reached the pull request, so a
requeue never duplicates it. A Retry request and the requeue it asks for
commit in one transaction, so an interruption leaves neither a recorded
request nor a stopped publication behind. A requeue that returns stopped
publications emits a `publication.requeued` event.

## Missing conversation recovery

A verified missing Claude conversation or Codex rollout allows one fresh session
per role, phase, and review round, including with `max_attempts: 1`. The failed
resume startup remains in attempt history but does not spend a normal attempt or
advance the review round. Authentication/configuration errors, unknown failures,
and missing session-start events do not authorize recovery.

Mergeyard commits the recovery allowance, replacement attempt, and deterministic
process identity before launching. Claude's new UUID is saved in that transaction;
Codex's replacement UUID is discovered from its native stream. Restarts observe
that process and its captured output. A missing process or ambiguous launch needs
attention; it never creates another fresh session. Normal failures consume the
configured attempt budget, and a second missing conversation in the same phase
and round needs attention even when normal attempts remain.

Every replacement receives the complete current phase input: issue context and,
for review/fix, the pinned PR target, diff or findings, and previous fix report
where applicable. Code, approval gates, and prior round history are preserved.
The `harness.session_resume_failed` event and a durable warning show the missing
identity and the role's active replacement identity in run detail and status.

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
unrelated eligible issues to start. Remote branches are retained. Optional
GitHub maintenance for each completed run keeps a five-second budget; local Git
cleanup uses the Git manager's own operation deadlines, so a large worktree is
not killed at the GitHub budget.

Run detail, the queue, and status show “Merged — cleanup pending”, early-merge
information, the concrete remaining actions, and the latest maintenance error.
Transient cleanup failures retry on later ticks and after restart. Dirty work
is preserved for human attention and an intact worktree is never force-removed. A clean worktree and local branch are also
preserved when their history contains unpublished commits. Merge observation
records the published PR head; cleanup checks publication before both deletions
and keeps unknown publication pending. Review
restoration must succeed before deletion, preserving preexisting human edits.

Worktree removal intent is recorded after ownership inspection and before removal.
A missing worktree with recorded intent can be reconciled as an interrupted
cleanup; unexplained disappearance requires inspection and retains the branch.
Git also records a cleanup boundary after the cleanliness checks pass and before
deletion. A refusal that deletes nothing clears it again, so locked or failed
removals keep their worktree safeguards. An interruption leaves the boundary
behind, and a later tick finishes the tracked deletions or missing link. On a
retry with an intact link, a lock or any change other than those deletions
preserves the tree for human attention. A registration that survives pruning,
such as a deliberately locked one, is reported and retried instead of recording
a false removal.
Already absent labels, closed issues, removed trees, and deleted local branches
are tolerated at their recorded boundaries. Independent report publication still
reaches a completed run's pending reports after Git cleanup finishes, where the
finished-run rule above decides whether one is attempted again.

Failed PR observations do not bypass a saved stop request or the CI wait deadline.
A successful merge observation remains authoritative; otherwise a pending stop
continues locally and unavailable CI evidence still expires at its saved deadline.

## Explicit Retry

Dashboard and CLI call the same `Scheduler.Retry` operation for attention and
failed runs. Retry shares the workflow operation gate with Stop, reconciliation,
and phase processing. It persists intent before observing external state; the
selected lifecycle, attempt window, round grant, CI deadline, and timeline event
commit together. Startup reconciles pending intent, including failed runs, before
any new execution. Selection alone does not launch a harness. Later ticks observe
existing completed work or launch the selected phase using the usual attempt
journal.

Each explicit attempt retry opens the configured attempt window with new physical
attempt numbers and retained role conversations/history. Missing-session recovery
remains limited to once per role, phase, and round. Exhausting the review ceiling
requires explicit Retry to grant exactly the next independent review round;
ordinary attempt retries and missing-session recovery never grant rounds.

CI Retry observes the PR and checks, preserves matching approval, and renews the
configured wait deadline. Its normal CI gate observes again before readiness or
repair. A changed published head clears stale approval even when local edits
prevent Retry from continuing. Reviewing and CI waiting require a clean worktree
matching the published head. Implementation and fixes may retain unpublished
edits or descendant commits; divergent or ambiguous ownership requires inspection.
Retry never resets, cleans, or overwrites files to make the worktree match a PR.
Before PR creation, it also verifies any published run branch is an ancestor of
local work; an absent branch permits first publication. A reconciled changed head
remains the review target in later rounds, with saved fix history retained as context.
Durable Stop supersedes pending Retry and continues interruption even when PR
lookup fails. An observed merge still follows completion and maintenance.
Observed merges go straight to durable completion and safe maintenance, including
failed runs, while closed-unmerged PRs and live owned processes prevent agent work.

## Temporary usage limits

Completed executions without adapter-owned native-success evidence pass through
the harness adapter's `ClassifyFailure(artifacts, observedAt)` seam. Its normalized outcomes are
`ordinary`, `temporary_limit` (optional reliable reset and signal source), and
`credits_exhausted`. Successful native output, including `allowed_warning`, and
valid task-level blocked/failed results do not enter the classifier.

Claude advertises `TemporaryLimitDetection` for the observed headless weekly
rejection: matching session identity, `rate_limit_info.status: rejected` for
`seven_day` with `isUsingOverage: false`, assistant `error: rate_limit`, terminal
`is_error: true` / `terminal_reason: api_error` / HTTP 429, and exit 1. Its result
subtype is nevertheless `success`. The adapter uses the top-level
`rate_limit_info.resetsAt` as Unix seconds; the earlier five-hour reset and
date-less diagnostic text are never reset sources. An absent, malformed, stale,
or conflicting weekly reset leaves cooldown selection to the core. Unavailable
overage credits accompanying this subscription limit do not produce a credit
block. See the [captured rejection and post-reset resume](research/claude-m3/report.md).

Codex advertises `TemporaryLimitDetection` for #62's observed Plus-plan rejection:
exit 1 with matching native `error.message` and `turn.failed.error.message` in
the captured same-day `You’ve hit your usage limit` Plus-plan template, allowing
changes to the upgrade and usage URLs. The same signal is recognized on fresh
and resumed executions; a native resume-limit capture remains unobserved, and
offline tests replay the captured signal on the resume path. The adapter converts
the text's 12-hour clock using the CLI host's local time zone and observation
date (checked against the persisted execution start date), then adds one minute because the text truncates reset seconds. Invalid,
stale, or DST-ambiguous reset times, and executions reconciled across a local
date boundary, leave cooldown selection to the core; a past
clock is never assumed to mean tomorrow. No rollout reset was available in the
captured first-request failure, so account metadata and unrelated sessions are
not reset sources. See [the Codex evidence](research/codex-m3/limit-report.md).

Codex persists `thread.started` identity during execution. A limited execution
without identity retains its wait and requires attention at resumption with
`harness.session_missing`; it never invents a conversation ID. A discovered ID
allows a resume attempt, but does not prove the native session is resumable.

Unknown variants for either adapter remain ordinary failures that consume the
configured attempt budget. Both adapters' `CreditExhaustionDetection` remains
disabled. Core fixtures supply classified outcomes directly; adapter binding
tests replay sanitized native fixtures offline. Normal CI makes no paid harness
calls.

A temporary limit saves the interrupted execution as `usage_limited` and records
`WAITING_FOR_HARNESS`, the same phase/round, and reset provenance atomically. A
reliable future reported reset wins over an estimated account cooldown in either
observation order; within the same source class, the later reset wins. An absent
or stale reset uses `usage_limits.cooldown` (default `30m`). The account gate spans all repositories
using that harness. Waiting and attention runs retain global and repository
slots; eligible work on the other harness and CI observation continue within
capacity. Existing running processes are observed rather than relaunched.

When the reset arrives, the next execution resumes the same role conversation
with full phase input and effective settings. Interruption context describes only
an immediately preceding usage-limited execution resumed in the same conversation;
ordinary retries and fresh missing-session recovery do not inherit that note.
Implementer edits are retained; reviewer edits are restored before waiting. Interrupted
executions spend neither ordinary attempts nor review rounds. Verified missing
conversations use the existing one-time missing-session recovery; ambiguous or
unavailable identity requires attention.

Review and fix recovery applies to all Claude/Codex role pairings, including
account restrictions discovered before a phase launches. Review restoration is
journaled before resumption or verdict acceptance. Automatic review resumption
keeps the interrupted target SHA; a changed head requires attention and explicit
Retry reconciliation before reviewing the new target. A crash with a reserved
execution but no process evidence requires attention rather than replaying an
ambiguous launch.

At `usage_limits.max_waits` consecutive interrupted executions (default `3`),
the run requires attention with `harness.usage_limit_waits_exhausted`. Explicit
Retry grants exactly one additional wait, keeps all history, and honors the known
reset. Repeated Retry while that selection is pending or waiting is idempotent.
Each phase and review round has its own allowance; a review wait grant cannot
extend the fix allowance or a later review round.
Ordinary completion ends the consecutive streak. Status, run detail, and settings
show the affected phase, round, role, harness gate, reset/cooldown source,
interrupted executions, and explicit grants. Account gates before a phase's first
execution display that no execution started rather than a zero wait budget.

## Exhausted credits

A classified `credits_exhausted` interruption records an indefinite account block
with no reset time. The originating run requires attention with
`harness.credits_exhausted`; other work needing that harness waits. Interrupted
executions retain their work, role conversation and ordinary attempt budget.

Retry reconciles the issue, PR, worktree and processes before choosing the next
work. It is also allowed from a run waiting specifically for credit recovery.
When the chosen work uses the blocked harness, Retry atomically reserves one
recovery probe for that harness. All other affected work remains paused. A temporary limit observed during
credit recovery retains its reported reset or cooldown; a reserved probe waits
until that time before launching, while the credit block remains indefinite. Work
selected on another harness or CI proceeds without reserving a probe or clearing
the blocked account. Credit recovery controls require the harness's
`CreditExhaustionDetection` capability; both production adapters keep it disabled
until the native credit-signal binding in #83 is delivered.

The reservation binds to one physical execution before launch. Native model
completion establishes availability independently of task success, including
valid blocked/failed task reports. Login, startup, session discovery, missing
completion evidence and continued credit failure leave the block intact.
Completion from an older probe cannot clear a newer restriction. Stop releases
probe ownership once the owned processes have stopped; label failures cannot
hold that reservation. A restart observes the original execution rather than
launching it again. Reserved launches without process evidence require attention.

Status, dashboard, CLI and durable events expose the credit wait, Retry eligibility,
reserved/running probe and recovery history. Check availability is the separate
#68 surface. Takeover also applies to timed/credit waiting and selected-run
probes, under the existing worktree/session prerequisites. It proves process
exit and finishes reviewer restoration before exposing manual control; ending
the attempt releases probe ownership without clearing the block. Handback
publishes normally and waits on the selected next phase's harness if blocked,
preserving its phase, round, attempt window and any additional review grant.
