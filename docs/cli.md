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
are supported in either role. `takeover`, `handback`, and `open` remain reserved.

`mergeyard reconcile` runs the same reconciliation without claiming new work.
It acquires the configured workspace lock, so stop the control plane first if
it is running. The command prints findings and preserves orphaned artifacts.

Status output escapes terminal controls and Unicode formatting controls in review
text, locations, settings and attention diagnostics. Stored reports keep their
original text; control sequences appear as visible escapes in the terminal.
