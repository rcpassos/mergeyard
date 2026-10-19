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
worktree (`10m`), the first transfer into a managed base (`1h`), routine
fetches and remote reads (`10m`), and pushes including the repository's
pre-push hook (`30m`). A first clone is staged beside the managed base and
renamed into place only after Git succeeds, so a killed transfer never leaves
something that looks like a completed clone; the next attempt retries the
clone under the clone budget instead of continuing it as a shorter routine
fetch. `gh` uses one budget per request, shared by its internal retries. The
gate that serializes the tick, its lifecycle controls and dashboard actions
observes the caller's context, as does the Git manager's own serialization, so
a caller that gives up stops waiting instead of queueing behind an operation it
no longer needs.

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
- **Let operators configure every budget.** Deferred: the regression came from
  one budget covering clone, routine fetch and push, not from the defaults
  being immutable. Separate class budgets make a slow first transfer and a long
  pre-push hook possible without adding deployment knobs that multiply the ways
  a deployment can wedge itself. Configuration can be reconsidered when a
  concrete deployment needs a different class budget, rather than a different
  timeout for every individual call.
- **Resume a killed clone by fetching into the partial checkout.** Rejected:
  Git does not index a killed transfer's temporary pack, so the fetch would
  usually restart the whole transfer anyway while claiming to be a routine
  refresh. Staging the clone and renaming it on success makes the next attempt
  an honest clone under the larger clone budget.

## Consequences

- An expired deadline is `git.timeout` or `github.timeout`, naming the stalled
  operation and how long it ran; a phase deadline also names its class budget,
  and a caller or outer deadline that fired first is named as such. The dashboard
  and the saved attention message therefore explain a budget-exhausted failure,
  and the existing retry paths handle it. A later tick proceeds. It stays
  distinct from `git.canceled` and `github.canceled`, which mean the context
  was cancelled rather than expired; a caller's own deadline is still a
  timeout.
- A command that reaches its deadline ends its owned process group, and output
  draining is bounded, so a hook or transport child that outlives the killed
  process cannot keep the invocation waiting. One process policy covers managed
  Git, `gh`, and phase/system-step commands: every command that does not drain
  cleanly stops its group, and a command that exited successfully still reports
  its own exit status when such a child lingers.
- A commit and its push keep a budget per phase: the operation's outer bound
  covers every phase, the commit and its hooks use the worktree budget, remote
  reads use the fetch budget, and the push and its pre-push hook use the push
  budget, so a slow hook cannot consume the push's deadline, a hook longer than
  a routine fetch can still publish, and the other phases cannot consume the
  push's deadline.
- A first clone runs under the larger clone budget. It is staged beside the
  base and renamed into place only on success, so an interrupted attempt leaves
  no base for a routine fetch to reuse and the next attempt runs a real clone
  again. A transfer that never finishes still fails and follows the ordinary
  retry path instead of holding the tick forever.
- Unattended SSH is non-interactive and keepalive-aware by default (batch mode,
  connect timeout), so a missing credential or dead connection fails instead of
  prompting. A `GIT_SSH`, `GIT_SSH_COMMAND`, or a `core.sshCommand` the machine
  resolves for the command's repository — including repository-scoped
  conditional includes — is an explicit transport choice and is left alone;
  `mergeyard doctor` applies the same policy to its remote probes, which run in
  disposable repositories where repository-scoped includes cannot apply.
- Timeouts are per operation, not per process: a caller with a shorter deadline
  keeps it, and one stalled call does not consume the budget of the calls that
  follow.
