# Run lifecycle and events

The runtime exposes `Runtime.Workflow` and `Runtime.Events`. Scheduler, CLI,
and dashboard handlers must use `Workflow.Transition` for all lifecycle writes.
`Workflow.Get` returns the persisted lifecycle view. Transitions leave worktree,
agent, PR, and review-round metadata intact; those components own that metadata.

States, phases, and triggers are typed strings in `internal/workflow`. The
central rules implement PRD §21. An `ACTIVE` run always has an `implement`,
`review`, or `fix` phase. Waiting for a harness preserves that phase. Other
transitions retain the last phase for history unless reconciliation chooses a
new phase. Terminal transitions set `completed_at`; retry clears it when the
destination is non-terminal.

`IssueClaimed` creates a run and requires its ID, repository, and issue number.
Other triggers load the existing run inside the transaction. `HandBack` requires
the next agent phase. `Retry` requires a reconciled destination, with a phase
for `ACTIVE` or `WAITING_FOR_HARNESS`. Reconciliation may choose `CLAIMING`,
`PREPARING`, `ACTIVE`, `WAITING_FOR_CI`, `WAITING_FOR_HARNESS`, `READY_TO_MERGE`,
or `COMPLETED`. The caller must first verify the relevant GitHub, Git, harness,
and CI facts as specified in PRD §22–23. Other triggers reject destination
overrides. Retry accepts both `NEEDS_ATTENTION` (§21) and `FAILED` (§22).

Transitions to `NEEDS_ATTENTION` or `FAILED` require `fault.Error` with nonempty
`Code` and `Message`. Both fields are persisted and included in the event's run
snapshot. The next successful transition clears the previous error. The error
type continues to support `errors.As` and `errors.Is` through its underlying
cause; existing configuration and workspace errors remain compatible.

Each accepted transition records exactly one event in the same SQLite
transaction as the run write. Its payload contains the prior state and phase,
the trigger, and the resulting run snapshot. Event types use the core §25
vocabulary; reconciled retries additionally emit `run.retried`. Rejected
transitions and storage failures leave both the run and event history unchanged.
Logging and in-process delivery happen only after commit.

Use `Events.Publish` for standalone events such as `scheduler.paused`.
`Events.Commit` supports other mutations that need an atomic event: its callback
must use the supplied transaction and must not reenter the bus. The runtime
shares one bus to serialize commits and deliver events in durable ID order.

`Events.Subscribe` returns a bounded channel and an idempotent cancellation
function. A slow consumer's channel closes when its buffer fills, allowing run
progress to continue. For SSE replay, subscribe first, read `Events.History`
from the client's last event ID in pages, and discard duplicate live IDs. If
the stream closes, reconnect and replay from the last delivered ID. Subscriber
payloads are independent copies. Runtime shutdown closes the bus and streams
before closing SQLite.
