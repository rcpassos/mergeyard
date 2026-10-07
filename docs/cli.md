# CLI operations

`mergeyard` and `mergeyard start` load configuration using the standard search
order, acquire the configured workspace lock, open/migrate SQLite, run dependency
checks, reconcile persisted implement and review attempts, and start the scheduler and HTTP/SSE
server. The dashboard uses `127.0.0.1:<port>` (default 7331). `open_browser: true`
launches it independently of HTTP serving with the platform browser launcher;
a delayed launcher does not block the dashboard or CLI controls. Launch failures
are reported without stopping the runtime. Shutdown cancels and waits for the
launcher. `--config <path>` works with every command.

SIGINT and SIGTERM cancel scheduler polling and HTTP/SSE requests, wait for
scheduler operations to return, and close SQLite before releasing the workspace
lock. Running tmux phases stay alive. The next startup observes those persisted
attempts and picks up completed results. Reconciliation verifies GitHub labels,
PRs, Git ownership, and sessions; orphaned claims and artifacts are reported
without deletion or automatic redispatch. Pending stops finish before normal
reconciliation can restore labels or advance work.

The operational commands use the running process's local HTTP API. Start
Mergeyard with the same configuration first. They do not open a second writable
runtime database or acquire the owner's lock.

- `mergeyard status`: print the scheduler state and nonterminal runs with their
  run ID, repository/issue, state, and current phase.
- `mergeyard pause` / `mergeyard resume`: disable/enable new claims. Existing
  runs continue to be monitored.
- `mergeyard watch <run-id>`: attach to the current live phase with
  `tmux -L mergeyard attach-session -r`. The client cannot send input to the
  phase. Detach with the normal tmux detach binding. Runs without a live phase
  return an error.
- `mergeyard stop <run-id>`: send SIGINT, then SIGTERM after a grace period,
  then force termination if needed. Preserve the worktree, branch, and PR;
  remove the ready and running labels; add attention if work was created; mark the run
  `STOPPED`. Interrupted stops and failed label updates are retried from durable
  intent before further work advances. Stop removes ready even if it arrives before claim processing has cleared that
  label, preventing redispatch after the run becomes terminal.

- `mergeyard retry <run-id>`: reconcile a `NEEDS_ATTENTION` or `FAILED` run and
  display its actual next state and phase. Reuse its ID, branch, worktree, role
  sessions, and history. Agent execution starts on a later scheduler tick only
  when reconciliation selects agent work. A repeated request while the run is
  progressing returns its current state without another grant or attempt.

Retry grants one additional independent review round after
`review.max_rounds_exceeded`. A CI timeout on the unchanged approved commit
receives a fresh configured wait window; passing checks can advance immediately,
while failures and action requests follow the normal repair/attention rules.
Neither waiting again nor observing an already merged PR launches an agent.
Changed published heads require another review. Local edits, divergent heads,
closed PRs, and still-running processes receive an actionable diagnosis; Retry
never resets or cleans work. Retry does not replenish a role's missing-session
recovery allowance for its phase and round.

Status includes failed runs, retry instructions and history, granted rounds,
renewed CI deadlines, role settings/session identities, verdicts, findings,
fix responses, CI evidence, and pending merge maintenance. Both Claude and Codex
are supported in either role. `open` remains reserved.

`mergeyard reconcile` runs the same reconciliation without claiming new work.
It acquires the configured workspace lock, so stop the control plane first if
it is running. The command prints findings and preserves orphaned artifacts.

Status output escapes terminal controls and Unicode formatting controls in review
text, locations, settings and attention diagnostics. Stored reports keep their
original text; control sequences appear as visible escapes in the terminal.

`mergeyard takeover <run-id>` prepares manual control through the running
control plane, then launches the exact implementer conversation in its worktree.
It requires a nonterminal run, prepared worktree, and recorded implementer UUID;
a pending Stop or Retry must finish first. Missing prerequisites give an
actionable error and never create a fresh conversation. Automated processes are
interrupted and their exit verified; interrupted review changes are restored
before the run enters `MANUAL`. Partial implementation and fix edits are retained.

Claude resumes with `--resume SESSION_ID --permission-mode default`. Codex uses
`resume -C WORKTREE -a on-request -s workspace-write -- SESSION_ID`, matching the
[verified interactive continuity contract](research/codex-m3/report.md).
These invocations use interactive permissions rather than unattended settings.
The CLI attaches its normal terminal streams and environment directly, without
launching an automated tmux phase. A launch failure leaves the run `MANUAL`;
retry takeover to resume the same conversation. A per-run process lock rejects
concurrent CLI resumes. The CLI reserves this ownership before requesting takeover
and holds it across the response and interactive launch, then releases it when
the interactive process exits. Exit the interactive harness before handing
control back.

Takeover intent is saved before interruption. A cancelled request or restart
finishes that preparation before advancing automation. Uncertain process, Git,
or restoration state preserves work in `NEEDS_ATTENTION`; inspect the diagnosis
before explicitly requesting takeover again. Repeated preparation has no new
attempt or lifecycle event. `MANUAL` survives restart without agent launches;
safe observation of an externally merged PR remains active.

The dashboard's copy command invokes this same CLI launcher using the running
binary and configuration. It revalidates the owning workspace and exact saved
implementer conversation, then holds interactive ownership until exit.

Merge cleanup shares the interactive process lock. While the session is open,
Git restoration, worktree removal, and local branch deletion remain pending.
Issue and label cleanup can finish independently. After interactive exit, a
later reconciliation resumes Git cleanup using the existing ownership and
dirty-work safeguards. The lock also excludes new CLI resumes during cleanup.

## Handback

Exit the interactive harness, then run `mergeyard handback <run-id>` against the
same configuration. Mergeyard checks every recorded phase process and reserves
interactive ownership before reconciling the worktree, branch, issue and PR.
Without a saved PR number, discovery checks all PR states for the branch; closed,
merged or multiple matching PRs preserve work for attention before publication.
Closed or ambiguous state and unexpected divergence preserve your work for
attention; handback never resets or force-pushes it.

Tracked edits and non-ignored new files are committed and pushed through the
existing Git safeguards. Your own commits are retained. Without a PR, handback
selects implementation and resumes the original implementer conversation. With
an open PR, it invalidates approval and selects independent review. An unfinished
review round is reused; an accepted verdict requires the next round. If that
round exceeds the allowance, this explicit request grants exactly one additional
review round. Resuming an interrupted phase opens its configured attempt window
without replenishing missing-session recovery. The CLI discloses the selected
phase and grant, and status retains
publication errors and history.

Publication is journaled before external effects. An interrupted request remains
manual while the scheduler reconciles it after restart; repeated handback resumes
the same intent without duplicate commits or grants. Pending publication withholds
interactive resume until handback finishes or Stop cancels it. Inspect the saved
error and application log if publication cannot finish. Clearing or branching the
interactive conversation does not replace Mergeyard's authoritative original
session identity. The user is responsible for exiting any interactive harness
started outside Mergeyard's ownership-aware launcher before handback.
