# Every external call carries its own deadline, and a timeout is a failure

One hung `gh` or `git` process must not freeze the control plane. A tick holds
the scheduler gate across reconciliation, dispatch, CI observation and identity
observation, and push, fetch and the repository's own commit and push hooks all
run inside that window. A stalled network call or hook therefore blocks every
runnable operation behind it, and the dashboard's actions wait on a lock that
ignores cancellation. SSH remotes make it worse: with no batch mode, a missing
credential or a dead socket waits on a prompt that an unattended process can
never answer.

Each managed invocation therefore derives its own deadline from its operation
and never outlives the caller's context. Managed Git distinguishes quick local
reads (`2m`), operations that may run repository hooks or touch the whole
worktree (`10m`), and operations that contact the remote (`10m`). `gh` uses one
budget per request, shared by its internal retries. The gate that serializes
the tick, its lifecycle controls and dashboard actions observes the caller's
context, as does the Git manager's own serialization, so a caller that gives up
stops waiting instead of queueing behind an operation it no longer needs.

## Considered Options

- **Bound the whole tick with one deadline.** Rejected: a tick's work is
  unbounded in count, so one budget would have to cover reconciliation, every
  run's advancement and every dispatch decision. A long push would consume the
  same budget as a fast reconcile, and a single deadline cannot distinguish
  "this operation was too slow" from "the tick has run long enough".
- **Skip repository hooks with `--no-verify`.** Rejected: it removes the hang
  but also removes the repository's own guardrails, and the requirement is that
  a hung hook is reported, not silenced.
- **Treat a timeout as cancellation.** Rejected: callers already read
  `ctx.Err()` as "the tick was canceled", so a bounded call would silently
  abandon a run with no attention record and no way to retry it.
- **Let operators configure every budget.** Deferred: no current need, and
  configuration would multiply the ways a deployment can wedge itself. The
  bounds are process-wide defaults with a clear per-operation rationale.

## Consequences

- An expired deadline is `git.timeout` or `github.timeout`, naming the stalled
  operation or the serialized wait, so the existing retry and attention paths
  handle it and a later tick proceeds. It stays distinct from `git.canceled`
  and `github.canceled`, which mean the caller's context ended.
- A command that reaches its deadline ends its owned process group, and output
  draining is bounded, so a hook or transport child that outlives the killed
  process cannot keep the invocation waiting. A command that exited
  successfully still reports its own exit status when such a child lingers.
- Unattended SSH is non-interactive and keepalive-aware by default (batch mode,
  connect timeout), so a missing credential or dead connection fails instead of
  prompting. A `GIT_SSH` or `GIT_SSH_COMMAND` already in the environment is an
  explicit transport choice and is left alone; the same default applies to
  `mergeyard doctor`'s remote probes.
- Timeouts are per operation, not per process: a caller with a shorter deadline
  keeps it, and one stalled call does not consume the budget of the calls that
  follow.
