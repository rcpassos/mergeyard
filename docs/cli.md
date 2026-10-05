# CLI operations

`mergeyard` and `mergeyard start` load configuration using the standard search
order, acquire the configured workspace lock, open/migrate SQLite, run dependency
checks, reconcile persisted M1 attempts, and start the scheduler and HTTP/SSE
server. The dashboard uses `127.0.0.1:<port>` (default 7331). `open_browser: true`
launches it independently of HTTP serving with the platform browser launcher;
a delayed launcher does not block the dashboard or CLI controls. Launch failures
are reported without stopping the runtime. Shutdown cancels and waits for the
launcher. `--config <path>` works with every command.

SIGINT and SIGTERM cancel scheduler polling and HTTP/SSE requests, wait for
scheduler operations to return, and close SQLite before releasing the workspace
lock. Running tmux phases stay alive. The next startup observes those persisted
attempts and picks up completed results. Full reconciliation of GitHub labels,
PRs, and orphaned resources remains in issue #13.

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

M1 supports the Claude implementer and ends at a draft PR without launching
review. The existing `takeover`, `handback`, `retry`, `reconcile`, and `open`
command entries remain reserved for their respective later work.
