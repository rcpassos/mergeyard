# M1 scheduler

`internal/scheduler.New` uses the effective config and a locked `app.Runtime`,
including its SQLite database, shared workflow, and event bus. `Run(ctx)` polls immediately and then every
`poll_interval` (default 30 seconds). `Tick(ctx)` is also available for explicit
reconciliation. Ticks serialize, and run operations coordinate with lifecycle controls so takeover
or stop waits for an in-flight operation before changing state. Running tmux agents are observed without waiting
for them to finish. Tick errors are logged by `Run` and tried again at the next
interval. Cancelling the scheduler leaves agent sessions running.

`Pause(ctx, true)` prevents new claims; existing implementations continue.
`Pause(ctx, false)` resumes dispatch. These operations publish scheduler events.
`Runs(ctx)` returns durable workflow snapshots. Startup and CLI/dashboard wiring
remain the responsibility of issue #14.

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
branch/worktree and Claude session UUID are also retained on the run. Each
attempt gets an input file, a durable tmux identity, logs and exit metadata.
Completed native structured output is validated and saved as `result.json` by
the control plane. This file is an archival report, not an agent-written
fallback result contract. Agent failures and invalid results can retry within
`implementer.max_attempts`; blocked results require attention immediately.

M1 supports the Claude implementer. The constructor rejects enabled repositories
configured with another implementer. Supply `Dependencies.Env` explicitly with
the environment the harness needs, for example `PATH`, `HOME`, `LANG`, and
`TMPDIR`. It is the complete child environment, not an implicit copy of the
control-plane environment. Dependencies default to the real GitHub, managed Git,
and local tmux adapters.

After implementation, Mergeyard commits changes, pushes without force, discovers
an existing PR or creates a draft, and persists its number and URL. **M1 ends at
`ACTIVE/review`, review round 1, without launching a reviewer.** This endpoint
continues to consume a concurrency slot. Review/CI, merge detection, user retry,
and full startup reconciliation belong to later milestones. Repeated ticks and
scheduler restarts observe persisted attempts and do not launch duplicates.

Blocked/invalid results, exhausted attempts, empty implementations, branch
conflicts and other system-step failures enter `NEEDS_ATTENTION`. The scheduler
removes running, adds attention, and preserves code and diagnostics. Failed
attention-label writes are retried on later ticks; ready is never re-added.
A missing/ambiguous process is left for human inspection rather than relaunched.

## Tests

The normal scheduler tests use real SQLite, Git and tmux with a fake Claude
executable, plus fake GitHub responses. They cover claim order and failure,
blocking, ordering, concurrency, retries, pause, attention, and duplicate
prevention after restart. They require `git` and `tmux` but no Claude credentials.

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
