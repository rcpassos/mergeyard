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
embedded files at `GET /static/`, and SSE at `GET /events`. Every dashboard
route except `/static/` needs the owner session header described below, and SSE
answers HTTP 403 without it; the `/api/` routes keep their own workspace-secret
proof. Scheduler actions are `POST /scheduler/pause` and `POST /scheduler/resume`.

`web.NewWithOperations` additionally exposes the CLI API, sharing the scheduler,
workflow, and workspace with the dashboard:

- `GET /api/status`: workspace identity, scheduler pause state, run snapshots,
  and the current CSRF token;
- `GET /api/runs/<run-id>/watch`: the current live tmux session reference;
- `POST /api/runs/<run-id>/stop`: interrupt the phase, update labels, and stop
  the run while preserving its code;
- `POST /api/runs/<run-id>/requeue`: return the run's stopped publications to
  the queue;
- `POST /api/scheduler/pause` and `POST /api/scheduler/resume`: the same
  scheduler control the dashboard uses, answering JSON;
- `POST /api/harnesses/<harness>/check`: the same availability check the
  dashboard offers;
- `POST /api/dashboard/session`: mint one single-use dashboard link for
  `mergeyard open`.

The CLI checks workspace identity before operating, disables HTTP proxies and
redirect following, and uses a 30-second request timeout.

### Workspace secret

Only the workspace owner can be trusted on the loopback port: another local
user can bind it whenever the runtime is down. The runtime and CLI therefore
share a secret stored at `<workspace>/runtime.secret` with owner-only
permissions (`0600`). `mergeyard start` writes a fresh secret on every startup,
so a value read before a restart can no longer authenticate. The CLI reads the
same file for each command.

Every `/api` request carries a fresh random nonce, the request time, and an
HMAC-SHA256 proof of the secret over the method, target, nonce and time. The
secret itself never travels over the port. The runtime rejects an unproven API
request with HTTP 401 and `runtime.unauthenticated`, and it proves the same
secret in the `X-Mergeyard-Proof` response header. That proof covers the exact
status and body, so a relay cannot edit an answer, and an unproven request
never receives a proof, so the runtime is not a proof oracle. The CLI refuses a
reply that does not prove the secret and binds the request to the run it asked
about. A missing secret tells the user to stop any running Mergeyard, start the
current version, unreadable permissions name the `chmod 600` fix, and a port
that cannot prove the secret is reported as `runtime.unauthenticated` with
recovery guidance.

### Dashboard session

The dashboard is locked to the workspace owner. `mergeyard start` prints a
dashboard link carrying a single-use token, and opens it when `open_browser` is
enabled; `mergeyard open` mints a fresh link through the proof-protected API.
The exchange hands the owner session to the browser in the redirect fragment.
Fragments are never sent in a request, and the dashboard script moves the value
into origin-scoped storage for the exact scheme, host and port before removing
it from the address bar. Every dashboard request then carries it in the
`X-Mergeyard-Session` header. Tokens expire and cannot be replayed. The session
rotates with the process, so a restarted runtime locks old browser sessions
out; the lock page points at `mergeyard open`.

The runtime sets no dashboard cookie. Cookies are not isolated by port (RFC
6265 §8.5) and SameSite treats every port on a host as the same site, so a
cookie for `127.0.0.1` reaches whichever local process holds any loopback port.
Storage and headers are scoped to the full origin instead: a page served from
another loopback port receives no cookie (there is none) and no header (the
browser attaches it only when the dashboard script addresses this runtime's
exact scheme, host and port). The runtime also refuses the session at any other
authority, and no CORS headers exist for another origin to relay it.

Without that session, a document request answers with the lock page and no page
renders the CSRF token; a mutation answers HTTP 403. `/static/` stays public so
the lock page is styled. The lock page loads the dashboard script, which
attaches the stored session and completes the page with the requested content;
a script-free browser stays on the lock page and should use the CLI instead. A
local process that never followed the link can read nothing and control
nothing.

Every POST still requires an exact `Origin: http://127.0.0.1:<port>` and a
random per-process token, supplied as the `csrf_token` form field or
`X-CSRF-Token` header. Missing origins are rejected, including requests from CLI
clients, which take the token from the proof-protected status snapshot. Host
checks also protect GET requests against DNS rebinding. Restarting the server
invalidates tokens; refresh open dashboard pages after a restart.

The dashboard link is printed to the runtime's terminal. On macOS, `mergeyard
start` and `mergeyard open` ask LaunchServices for the default HTTP browser with
an AppleScript `open location` command delivered over stdin. On Linux they write
the link into an owner-only handoff page under
`$HOME/mergeyard-dashboard-handoff/` (a non-hidden `0700` directory holding a
`0600` page) and launch the resolved default web browser explicitly with
`gio`, `gtk-launch`, or the browser command from its desktop entry, so a
confined browser such as the Firefox snap can read the page and an HTML editor
association is never used. On Linux the launcher runs detached, in its own
session and process group, and Mergeyard never waits for it: `mergeyard open`
returns as soon as the launcher starts, and stopping the runtime never closes
a browser it opened. If no HTTP browser can be resolved, the command
reports that instead of falling back to `xdg-open`. Other platforms fall back to
`<workspace>/.dashboard-links/`. In every case the launcher arguments contain a
path or a stdin marker, never the token. The token remains single-use and
short-lived, and the session it hands over dies with the process, so a stolen
link only races the browser once.

Failed scheduler actions return HTTP 500 with a safe diagnostic code in the
response body and `X-Mergeyard-Error-Code` header. The dashboard displays that
code. Structured logs retain the action, code, and underlying cause; the cause
is not sent to the browser. Uncoded or malformed errors use
`internal.scheduler_control`.

SSE carries the event type as `event`, the durable numeric ID as `id`, and the
complete JSON event as `data`. Fresh connections start with live events.
Reconnects with `Last-Event-ID` replay durable history and deduplicate live
events. UI-only notifications (`queue.updated`, `diagnostics.updated`) are
transient: they are delivered live without an `id` frame, are never written to
SQLite, and never advance the replay cursor. Streams send heartbeat comments
and disconnect slow consumers, allowing the browser to reconnect and replay.
The browser refreshes scheduler state when the stream connects, on pause/resume
events, and on discovery notifications for the visible queue or settings page.

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
by `HX-Request` and are never cached. A navigation without the owner session
header gets the boot layout: the dashboard script attaches the stored session
and requests the same URL with `HX-Request`, so pages and forms work with
JavaScript enabled. Script-free browsers get the lock page; the CLI remains the
script-free control path. Missing run IDs return 404.

`web.NewDashboard` takes the event bus, shared scheduler control, and
`DashboardOptions`: runtime database, workspace, scheduler engine, effective
config and its source path, and an optional doctor callback. The engine must
use the runtime's workflow and scheduler control. The constructor also enables
the shared CLI API. `New` remains available for the foundation without runtime data.

Run `Server.RunUpdates(ctx)` in a goroutine. It discovers GitHub queue issues
immediately and at `poll_interval`, including while paused or at capacity.
Discovery performs no claims and reads the scheduler's shared label-filtered poll
listing, reusing a snapshot for up to two poll intervals so the dashboard and
the scheduler do not both fetch at each boundary. Blocker reads are reused while
an issue's listing snapshot is unchanged. A refused issue that no longer carries
a Mergeyard label uses the same snapshot until its refusal changes, the listing
sees it again, or a 15-minute window expires. A failed reread keeps the last
snapshot, or records the issue's scoped failure when no snapshot exists, and
backs off instead of retrying on every pass. Each refused read is bounded so a
slow request cannot consume the refresh budget reserved for the rest of
discovery. A read that the refresh budget ended before the full read window is
merely slow and is read again on the next refresh, instead of hiding the issue
for a whole window. A timeout that GitHub reports before the budget ends the
read backs off for the full window, whatever the number of repositories.

The refused reads share the refresh budget with the rest of the pass: they may
spend at most half of it, and each repository may spend a share of what is
left. Once that share is spent the refresh keeps the last readable snapshot of
the refusals it did not reach and still returns every repository's queue. The
refusals of a repository are read in turn, so a refusal that always spends the
repository's share cannot keep the refusals behind it out of the queue for
good. The local lookups that assemble the queue do not share the budget
reserved for GitHub, so a spent budget costs the refresh its fresh refusals
rather than its whole queue, and a repository the refresh could not list is
reported on its own instead of failing the queue.

A GitHub failure scoped to one
repository or run does not hide the others: readable repositories refresh, and
the failed repository or issue is shown in the needs-attention section with its
diagnostic code until a later pass succeeds. Other discovery errors fail closed
and preserve the last successful snapshot with a stale-data notice. If the
refresh budget expires while the shared listing is still being fetched,
discovery keeps the last successful snapshot and its sync time without
reporting an error. An issue with unknown dependency status is never displayed
as ready. Run lifecycle data always
comes from SQLite, so claims and phase/PR transitions immediately supersede stale
issue snapshots. The doctor callback runs once with a bounded context outside
HTTP requests. Cancel and join RunUpdates before closing the runtime.

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
harness recovery history; it has no configuration write endpoint.

`POST /runs/{id}/stop` uses the same `Scheduler.Stop` as the CLI. It coordinates
with in-flight operations, stops running phase sessions, removes ready and
running labels, and adds attention when work exists before marking the run
STOPPED. It preserves worktrees, branches, and PRs. A preparing run whose
setup was interrupted releases the leftovers it owns, so the next run for the
same issue prepares; leftovers that hold work are kept, and the issue is
flagged for attention with a comment naming them. Repeating Stop on an
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
launch delay. Current run JSON omits an expired credit-wait reset;
`harness_wait_history` retains its observed timestamp and provenance.

`POST /runs/{id}/takeover` and `POST /api/runs/{id}/takeover` share
`Scheduler.Takeover`. Run detail offers Take over run when its prerequisites
are recorded, explains missing worktree/session identity, and shows pending
preparation. It displays an escaped, copyable `mergeyard takeover` command only after
process exit and review restoration have completed. The API returns the prepared
run snapshot; it never returns an executable, argument vector or directory, and
it does not launch an interactive terminal. The CLI rebuilds the resume
invocation locally from the run's stored role settings, its recorded implementer
session and the managed worktree layout, then verifies the worktree exists. An
authenticated answer that carries command-shaped fields cannot choose what runs.
The copied command uses the running Mergeyard executable, an absolute configuration
path, and the daemon's original working directory, so workspace identity is checked
even when pasted elsewhere or when configuration paths are relative. It resumes
the exact implementer identity through the same ownership-aware CLI launcher;
the identity remains visible in the run detail. A missing configuration path
withholds the command and displays an actionable explanation.
Normal interactive permissions are selected explicitly for either harness.
`run.takeover_requested` and `run.manual` refresh the dashboard over SSE.
Takeover uses the existing Host, Origin, CSRF, loopback, workspace-secret, and CLI
workspace checks; known prerequisite errors include actionable messages. Manual
runs retain their slot and never launch automated agent phases after restart.

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

`POST /runs/{id}/requeue` and `POST /api/runs/{id}/requeue` return the run's
stopped publications to the queue through the shared scheduler operation. Run
detail lists every stopped report comment, attention comment and pull request
body with the reason it stopped, and offers **Requeue publications** while that
list is not empty. The list disappears once the publications are requeued, and
the reason returns if the cause is still broken. The API answers with the
requeued publications and their reasons. `publication.requeued` refreshes pages
over SSE. The endpoint uses the existing Host, Origin, CSRF, and CLI workspace
identity protections, and an unknown run answers `internal.run_not_found`.

`POST /harnesses/{harness}/check` calls the shared `Scheduler.CheckHarness`
operation. JSON clients receive the durable check snapshot; browser forms refresh
the dashboard. Harness availability offers **Check availability** only for a
credit block with supported detection, no owning probe, and no future timed wait.
The form discloses account quota use and the minimal request's lack of engineering
work. Check remains available with no eligible affected runs; recovery does not
revive stopped runs. The endpoint retains Host, Origin, CSRF, and CLI workspace
identity protections. Conflicting checks/Retry return `harness.probe_unavailable`;
a future timed reset returns `harness.check_waiting`, both HTTP 409. Status JSON
includes `check_eligible` and the latest check's ID, status, result, and timestamps.
`harness.check_started` and `harness.check_completed` refresh pages over SSE.
Checks run outside repository worktrees with skills disabled, use the shared
native completion gate, and retain ownership until their execution ends. Startup,
login, and session discovery alone cannot release paused work. Captures and
execution journals are retained under `harness-checks/<check-id>` in the workspace.
Automatic doctor diagnostics never perform availability requests.

Availability-check preparation failures and unreadable output after a confirmed
exit finish the check without proof, retain the credit restriction, and release
unused probe ownership. Ambiguous process status retains ownership and records
an actionable result via `harness.check_updated`. Per-check failures are logged
without preventing unrelated checks, run reconciliation, or new claims.
`harness.check_failed` returns HTTP 409 with the safe recovery guidance.

Codex checks first perform a bounded, read-only `codex mcp list --json` inspection
under the check's invocation settings. No servers are initialized and no model
request is made by this inspection. The adapter explicitly disables every
configured MCP server, including required servers and names containing dots, and
the final invocation is persisted before launch. Inspection failures launch no
model request. Authentication settings remain in the original account environment;
inspection output and account environment values are not persisted or logged.
An opt-in native configuration regression runs without credentials, model calls,
or server startup:

```sh
MERGEYARD_CODEX_CONFIG_INTEGRATION=1 go test ./internal/harness -run '^TestCodexCheckDisablesInheritedMCPServers$' -count=1
```
