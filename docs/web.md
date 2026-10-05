# Local dashboard foundation

`mergeyard start` serves the dashboard on the configured loopback port (default
`http://127.0.0.1:7331`) and runs the M1 scheduler. The runtime exposes the global
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
