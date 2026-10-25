# Mergeyard

Mergeyard coordinates coding agents to turn approved issues into reviewed, merge-ready pull requests. The user retains the merge decision.

## Language

**Harness**: The coding-agent application selected to perform a role's work, such as Claude Code, Codex, or OpenCode. The model is a separate role setting.

**Availability scope**: The set of work affected by one restriction on an allowance, credit balance, or budget. One harness can have several scopes, and different funding paths can share a restricted resource.

**Usage limit**: A temporary restriction on available usage within an availability scope, with a reported reset time or an assumed cooldown.

**Limit signal**: A harness's native evidence that an execution ended because of a usage limit or exhausted credits, as distinct from the outcome Mergeyard classifies from it.
_Avoid_: Native limit format, limit error

**Exhausted-credit block**: A restriction caused by exhausted credits or a cap without scheduled recovery, whose recovery requires an explicit user action. A budget cap with a verified scheduled reset is a usage limit.

**Recovery probe**: A user-requested attempt to establish whether an availability scope blocked by exhausted credits is usable again. It can perform the next work for a selected run or make a minimal availability check without engineering work.

**Role settings**: The harness, model, effort and skills one role (implementer or reviewer) uses throughout a run. They are resolved when the issue is claimed.

**Harness override**: An issue label that selects which configured harness performs one role for that issue's run, in place of the repository's configured harness.
_Avoid_: Agent label, harness label

**Tier**: One of `low`, `medium`, or `high`, with model and effort settings specific to a role and harness. An issue label selects the implementer's or reviewer's tier independently; role configuration or a harness override selects the harness.

**Workspace secret**: The owner-only file the runtime writes inside the workspace and the CLI reads, so both can prove they belong to the same workspace before any API request or interactive launch.
_Avoid_: API key, token (the browser CSRF token is separate)

**Dashboard session**: The owner-only browser credential set by the single-use link printed by `mergeyard start` (or `mergeyard open`). It lives in origin-scoped browser storage, travels only to the runtime's exact scheme, host and port as the `X-Mergeyard-Session` header, and rotates with the runtime process.

**Dashboard handoff page**: The owner-only local HTML page that redirects the browser to a single-use dashboard link, so the token never enters a process command line. Linux keeps it in a non-hidden home directory so confined browsers can read it; macOS passes the URL to the default HTTP browser over stdin instead.

**Detached browser**: The browser Mergeyard opens for the dashboard. It outlives the command that opened it: `mergeyard open` returns at once, and stopping `mergeyard start` never closes a browser the user is watching.
_Avoid_: Child browser, owned browser

**Queue group**: The bucket the dashboard sorts an open run or a queued issue into. There are six groups, in display order, and every open run and queued issue belongs to exactly one; ended runs leave the queue.
- **Needs attention**: something is stuck until the user acts, including a scoped GitHub failure, a merged run whose cleanup has not finished, and an issue flagged for attention.
- **Waiting for your merge**: the PR is approved and ready; only the user's merge remains.
- **Running**: Mergeyard or the user is actively moving the run forward, including a manual takeover and waiting for CI.
- **Waiting for harness**: the run is paused by a usage limit or an exhausted-credit block.
- **Ready**: an eligible issue not yet claimed.
- **Blocked**: an issue that cannot be claimed yet, because of open blockers or a refusal.
_Avoid_: Section, column, lane

**Ended run**: A run that has left the queue: its state is terminal and, when a merge was observed, its cleanup has finished.

**Scoped GitHub failure**: A GitHub read or write failure attributed to one repository or run. It is reported on the dashboard and skips only its subject; local-state failures still stop dispatch everywhere.

**Publication**: The delivery of a run's saved evidence to GitHub: a report comment, an attention comment, or the verification body of the pull request. A publication that fails permanently stops and keeps its reason until a human requeues it.

**Requeue**: A human request to return a run's stopped publications to the delivery queue because the cause is fixed. It restarts their backoff, delivers the frozen content again, and never duplicates a comment that already reached the pull request.
