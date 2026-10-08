# Local dashboard foundation

`mergeyard start` serves the dashboard on the configured loopback port (default
`http://127.0.0.1:7331`) and runs the scheduler. The runtime exposes the global
claim gate; pausing it leaves existing runs alone.

## Server boundary

`internal/web.New` takes the shared event bus and a concurrent scheduler
implementing `Pause(context.Context) error`, `Resume(context.Context) error`,
and `Paused() bool`. The scheduler owns its pause/resume events.

`web.Listen("127.0.0.1:<port>")` rejects hostnames and non-loopback bindings.
`Server.Serve(ctx, listener)` handles cancellation, SSE disconnection and HTTP
shutdown. The caller closes the runtime and event bus after Serve returns.

The dashboard serves `GET /`, the scheduler fragment at `GET /scheduler`,
embedded files at `GET /static/`, and SSE at `GET /events`. Scheduler actions
are `POST /scheduler/pause` and `POST /scheduler/resume`.

`web.NewWithOperations` additionally exposes the CLI API, sharing the scheduler,
workflow, and workspace with the dashboard:

- `GET /api/status`: workspace identity, scheduler pause state, run snapshots,
  and the current CSRF token;
- `GET /api/runs/<run-id>/watch`: the current live tmux session reference;
- `POST /api/runs/<run-id>/stop`: interrupt the phase, update labels, and stop
  the run while preserving its code.

CLI pause/resume use the existing scheduler endpoints with
`Accept: application/json`, which returns JSON instead of a browser redirect.
The CLI checks workspace identity before operating, disables HTTP proxies and
redirect following, and uses a 30-second request timeout.

Every POST requires an exact `Origin: http://127.0.0.1:<port>` and a random
per-process token, supplied as the `csrf_token` form field or `X-CSRF-Token`
header. Missing origins are rejected, including requests from CLI clients.
Host checks also protect GET requests against DNS rebinding. Restarting the
server invalidates tokens; refresh open dashboard pages after a restart.

Failed scheduler actions return HTTP 500 with a safe diagnostic code in the
response body and `X-Mergeyard-Error-Code` header. The dashboard displays that
code. Structured logs retain the action, code, and underlying cause; the cause
is not sent to the browser. Uncoded or malformed errors use
`internal.scheduler_control`.

SSE carries the event type as `event`, the durable numeric ID as `id`, and the
complete JSON event as `data`. Fresh connections start with live events.
Reconnects with `Last-Event-ID` replay durable history and deduplicate live
events. Streams send heartbeat comments and disconnect slow consumers, allowing
the browser to reconnect and replay. The browser refreshes scheduler state when
the stream connects and on pause/resume events.

## Assets

Run `make assets` to install the versions locked in `web/package-lock.json`
and build Tailwind/daisyUI CSS, HTMX, Lucide SVGs, and the dashboard script.
`make build` rebuilds assets before compiling Go. CI uses Node 24 for this step.
Commit the generated `web/static/` files together with source changes.

Templates and generated assets are embedded in Go. `go build ./cmd/mergeyard`
works directly from a checkout without Node, using the committed assets. The
distributed binary needs no Node, asset directory, CDN, or external fonts.
Third-party asset licenses are included under `web/static/licenses/`.

## Runtime pages

`GET /`, `/queue`, `/runs/{id}`, and `/settings` are server-rendered pages.
HTMX requests to the same URLs return the `#content` fragment; responses vary
by `HX-Request` and are never cached. Navigation and ordinary forms also work
without JavaScript. Missing run IDs return 404.

`web.NewDashboard` takes the event bus, shared scheduler control, and
`DashboardOptions`: runtime database, workspace, scheduler engine, effective
config and its source path, and an optional doctor callback. The engine must
use the runtime's workflow and scheduler control. The constructor also enables
the shared CLI API. `New` remains available for the foundation without runtime data.

Run `Server.RunUpdates(ctx)` in a goroutine. It discovers GitHub queue issues
immediately and at `poll_interval`, including while paused or at capacity.
Discovery performs no claims. It preserves the last successful snapshot on
failure and shows a stale-data notice. Discovery errors fail closed: an issue
with unknown dependency status is never displayed as ready. Run lifecycle data
always comes from SQLite, so claims and phase/PR transitions immediately
supersede stale issue snapshots. The doctor callback runs once with a bounded
context outside HTTP requests. Cancel and join RunUpdates before closing the
runtime.

SSE notifications refresh the current page for run, phase, PR, scheduler,
queue and diagnostic changes. Each persisted implement attempt emits
`phase.attempt_started`, including retries, so attempt details and logs refresh
together. Reconnecting refreshes the snapshot, including
when a run completes while the browser is disconnected. Run output has its own
`GET /runs/{id}/output` fragment, polled every two seconds until the run ends.
It reads at most the last 128 KiB and 200 events from the latest attempt's
`events.jsonl`, renders text/tool names/result summaries, and limits rendered
text to 32 KiB. Malformed or incomplete events are skipped. Paths and symlinks
cannot escape the runtime workspace. Templates escape all issue, event, and
log content. The timeline shows the latest 100 durable run events, newest first.

Queue sections are Running, Ready, Blocked, Needs attention, and Draft
PRs (review pending). Pending M1 runs automatically continue into an independent
review. Run detail shows the reviewer settings/session, pinned SHA, verdict,
findings and restoration diagnostics. Review events refresh the page over SSE. Attention
and manual runs also consume slots. Harness-waiting runs retain their slots and
show account availability and effective reset provenance. Run and settings history
distinguishes interrupted executions and explicit grants from account gates where
no execution started. Ready-to-merge runs release their slot and wait for the user's
merge. Recent runs are limited to the latest 20 ended runs. The settings page
displays resolved values, configuration/workspace paths, doctor results, and
usage-limit recovery history; it has no configuration write endpoint.

`POST /runs/{id}/stop` uses the same `Scheduler.Stop` as the CLI. It coordinates
with in-flight operations, stops running phase sessions, removes ready and
running labels, and adds attention when work exists before marking the run
STOPPED. It preserves worktrees, branches, and PRs. Repeating Stop on an
already-stopped run returns without changing labels or emitting another event,
so it cannot interfere with a newer run for the same issue. Existing Host,
Origin, CSRF, and safe error-code checks apply.

Stop intent is persisted and emitted as `run.stop_requested`. If a process or
GitHub operation fails, the run remains nonterminal with a Retry Stop action;
later scheduler ticks retry the pending Stop before any phase advancement.
Attempts retain their original model, effort, and skills. Empty persisted
model/effort mean harness defaults; migrated attempts without a skills snapshot
show that the skills were not recorded rather than using today's config.

`mergeyard start` uses the configured workspace and port, runs doctor and
reconciliation, and serves both dashboard pages and the CLI API alongside the
scheduler. It starts queue discovery and exposes the startup doctor report on
Settings. Shutdown cancels and joins scheduler, discovery, diagnostics, and
browser workers before closing runtime storage.

`POST /runs/{id}/retry` and `POST /api/runs/{id}/retry` both use
`Scheduler.Retry`. Run detail offers Retry for `NEEDS_ATTENTION`, `FAILED`, and
credit-blocked `WAITING_FOR_HARNESS`. The scheduler supplies `retry_eligible` from
the same policy used by the dashboard and CLI. Stop, takeover, merge maintenance,
a probe already owned by the run, an unavailable credit-detection capability, or
another run owning the required harness probe suppress the action. Saved
implementation/fix publication and CI-only recovery can proceed without a model
probe. Ordinary timed waiting never offers Retry. The API returns the actual run
snapshot, including the next phase, pending/rejected retry intent, additional
review round, or renewed CI deadline. Detail and CLI status retain retry history;
`run.retry_requested`, `run.retry_rejected`, and approval invalidation refresh
pages over SSE. Retry uses the existing Host, Origin, CSRF, workspace-identity,
and safe error response boundaries. `harness.probe_unavailable` and
`harness.credit_detection_unavailable` return HTTP 409 with their safe human
message in the body and code in `X-Mergeyard-Error-Code`. The dashboard displays
the conflict message, including refresh/retry guidance, while underlying causes
remain in structured logs. Repeated requests for an already selected probe remain
idempotent even though the UI suppresses a new Retry action. It preserves work
and Stop behavior.

`harness.credits_exhausted`, `harness.probe_reserved`, `harness.probe_released`,
`harness.credit_recovered`, and `harness.available` refresh credit availability,
probe progress, Retry controls, and recovery history over SSE. Probe-related
harness events can carry the selected run ID; see `docs/workflow.md` for the event
contract. A credit block remains indefinite after a temporary reset expires.
Expired resets remain in durable history but no longer display as a future probe
launch delay.

`POST /runs/{id}/takeover` and `POST /api/runs/{id}/takeover` share
`Scheduler.Takeover`. Run detail offers Take over run when its prerequisites
are recorded, explains missing worktree/session identity, and shows pending
preparation. It displays an escaped, copyable `mergeyard takeover` command only after
process exit and review restoration have completed. The API returns executable,
argument vector, and worktree; it does not launch an interactive terminal.
The copied command uses the running Mergeyard executable, an absolute configuration
path, and the daemon's original working directory, so workspace identity is checked
even when pasted elsewhere or when configuration paths are relative. It resumes
the exact implementer identity through the same ownership-aware CLI launcher;
the identity remains visible in the run detail. A missing configuration path
withholds the command and displays an actionable explanation.
Normal interactive permissions are selected explicitly for either harness.
`run.takeover_requested` and `run.manual` refresh the dashboard over SSE.
Takeover uses the existing Host, Origin, CSRF, loopback, and CLI workspace checks;
known prerequisite errors include actionable messages. Manual runs retain their
slot and never launch automated agent phases after restart.

If a PR merges while a CLI or dashboard-command takeover is open, the dashboard records completion
and shows pending cleanup. Interactive ownership protects the worktree and
branch through control-plane restarts; Git cleanup resumes after the session
exits. Independent issue/label cleanup continues while Git cleanup is deferred.

`POST /runs/{id}/handback` and `POST /api/runs/{id}/handback` call the shared
handback operation with the same request protections and interactive ownership
gate as the CLI. Manual run detail offers **Hand back run**, discloses commit/push
and review-round behavior, and requires the user to exit interactive execution.
The run snapshot and dashboard retain selected phase, pinned manual commit,
pending publication/recovery errors and the additional round grant. Handback
history and lifecycle events refresh over SSE. A pending handback cannot launch
an interactive resume or automated phase; restart reconciles publication first.
