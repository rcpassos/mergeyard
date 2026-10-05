# Run lifecycle and events

The runtime exposes `Runtime.Workflow` and `Runtime.Events`. Scheduler, CLI,
and dashboard handlers must use `Workflow.Transition` for all lifecycle writes.
`Workflow.Get` returns the persisted lifecycle and PR/review metadata snapshot.
Transitions preserve component-owned metadata unless the caller supplies an
atomic `Request.Metadata` patch.

States, phases, and triggers are typed strings in `internal/workflow`. The
central rules implement PRD §21. An `ACTIVE` run always has an `implement`,
`review`, or `fix` phase. Waiting for a harness preserves that phase. Other
transitions retain the last phase for history unless reconciliation chooses a
new phase. Terminal transitions set `completed_at`; retry clears it when the
destination is non-terminal.

`IssueClaimed` creates a run and requires its ID, repository, and issue number.
Other triggers load the existing run inside the transaction. `HandBack` accepts
only `implement` (no PR) or `review` (PR exists), as reconciled by the caller.
`Retry` requires a reconciled destination, with a phase
for `ACTIVE` or `WAITING_FOR_HARNESS`. Reconciliation may choose `CLAIMING`,
`PREPARING`, `ACTIVE`, `WAITING_FOR_CI`, `WAITING_FOR_HARNESS`, `READY_TO_MERGE`,
or `COMPLETED`. The caller must first verify the relevant GitHub, Git, harness,
and CI facts as specified in PRD §22–23. Other triggers reject destination
overrides. Retry accepts both `NEEDS_ATTENTION` (§21) and `FAILED` (§22).
Stop, internal failure, and takeover apply only to non-terminal runs, so a
completed run cannot be revived and a failed run keeps its error and retry path.
They remain available for runs with malformed persisted phases; normal automated
progression rejects those phases.

Transitions to `NEEDS_ATTENTION` or `FAILED` require `fault.Error` with nonempty
`Code` and `Message`. Both fields are persisted and included in the event's run
snapshot. The next successful transition clears the previous error. The error
type continues to support `errors.As` and `errors.Is` through its underlying
cause. `Error()` retains the underlying cause for diagnostics, while `Message`
provides the human message separately. Existing configuration and workspace
errors remain compatible. Duplicate claims and conflicting failed-run retries
return `internal.run_conflict` with "Issue already has an active run"; storage
errors retain `internal.run_store` or `internal.event_store`.

Each accepted transition records exactly one event in the same SQLite
transaction as the run write. Its payload contains the prior state and phase,
the trigger, and the resulting run snapshot. Event types use the core §25
vocabulary. Reconciled retries emit the destination's event (for example,
`run.completed` or `phase.started`), with `retry` retained as the payload trigger.
The other retry destinations emit `run.claimed`, `run.preparing`, `ci.updated`,
`run.waiting_for_harness`, or `pr.ready_for_review`. A run entering
`WAITING_FOR_HARNESS` emits `run.waiting_for_harness`; resuming it emits
`phase.started` because §21 starts a new attempt. `harness.usage_limited` and
`harness.available` are application events without a run ID, published once per
harness limit by the component that writes `harness_limits`. Rejected
transitions and storage failures leave both the run and event history unchanged.
Logging and in-process delivery happen only after commit.

`Request.Metadata` is a typed `MetadataPatch` applied by the workflow in the
same transaction as the lifecycle write and event. Its optional `PRNumber`,
`ReviewRound`, and `ApprovedSHA` fields update only those columns on the run
being transitioned. Nil fields preserve their values; an empty `ApprovedSHA`
clears approval. `IncrementReviewRound` atomically advances the persisted round
without exposing a transaction to callers. A patch cannot both set and increment
the round. PR numbers must be positive and review rounds nonnegative.

PR creation can persist `pr_number` and `review_round`, reviewer approval can
persist `approved_sha`, and retry can grant a round before the transaction
commits. The returned run and event snapshot include those updates. Invalid
patches and metadata/event-write failures leave all writes rolled back.
Patches contain no lifecycle fields, run ID, SQL, or callbacks, so they cannot
change another run or bypass transition validation.

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

`OperationFailed` moves any nonterminal run to `NEEDS_ATTENTION` with a required
coded failure. The scheduler uses it for failed claims, preparation, and Git/PR
system steps, including failures that occur before an agent phase exists. It
preserves the current phase and component-owned metadata and records the normal
`run.needs_attention` event. Terminal runs reject it.

`Workflow.WithRunOperation` holds a per-run gate while a scheduler operation
performs external effects. Every `Transition` acquires the same gate. The
operation callback receives a fresh snapshot and a context to pass to its own
transitions, which can proceed under the held gate. Other callers, including
takeover and stop, wait for the operation or their context cancellation. Use the
runtime's shared `Workflow` instance and do not retain the callback context
beyond the operation. Once a takeover or stop has persisted, a scheduler operation
reads that new state before deciding whether to proceed.

## Review verdict persistence

MetadataPatch.Review carries the restored review completion with its attempt ID
and report. Workflow validates attempt/run/round ownership, restoration,
contamination and the pinned target before accepting approval or changes required.
Report persistence, attempt completion, approved SHA, lifecycle and event share
one transaction. MetadataPatch.ReviewRejection similarly commits a nonretryable
report/failed attempt with its attention transition; event failure rolls back both.
Run snapshots include the latest durable reviewer settings/report/diagnostics for
status output and transition events. M1 histories without a review remain readable.

## Fix completion

MetadataPatch.Fix carries the completed fix attempt identity. Workflow verifies
run/round ownership, a successful report and the durable pinned push journal in
the same transaction that advances one review round, clears approval and emits
fix.completed. Run snapshots include the latest fix and its full attempt history,
so earlier findings and responses remain visible after another review.

## Manual merge and maintenance

`PRMerged` can complete any nonterminal run with a persisted PR, including an
early merge. The scheduler first durably records the merge and stop intent and
verifies that owned processes have exited. Terminal stopped runs reject merge
completion. The separate `merge_cleanup` journal persists maintenance progress
outside the coding lifecycle. Run snapshots include that journal for status,
events, and UI. `pr.merge_observed` records merge intent; `merge.cleanup_updated`
records individual maintenance boundaries and pending errors.
