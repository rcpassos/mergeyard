# Mergeyard

Mergeyard coordinates coding agents to turn approved issues into reviewed, merge-ready pull requests. The user retains the merge decision.

## Language

**Usage limit**: A temporary restriction on a harness's available usage, with a reported reset time or an assumed cooldown.

**Limit signal**: A harness's native evidence that an execution ended because of a usage limit or exhausted credits, as distinct from the outcome Mergeyard classifies from it.
_Avoid_: Native limit format, limit error

**Exhausted-credit block**: A restriction caused by exhausted credits or a spend cap, whose recovery requires an explicit user action rather than a scheduled reset.

**Recovery probe**: A user-requested attempt to establish whether a harness blocked by exhausted credits is usable again. It can perform the next work for a selected run or make a minimal availability check without engineering work.

**Role settings**: The harness, model, effort and skills one role (implementer or reviewer) uses throughout a run. They are resolved when the issue is claimed.

**Harness override**: An issue label that selects which configured harness performs one role for that issue's run, in place of the repository's configured harness.
_Avoid_: Agent label, harness label

**Tier**: One of `low`, `medium`, or `high`, with model and effort settings specific to a role and harness. An issue label selects the implementer's or reviewer's tier independently; role configuration or a harness override selects the harness.

**Workspace secret**: The owner-only file the runtime writes inside the workspace and the CLI reads, so both can prove they belong to the same workspace before any API request or interactive launch.
_Avoid_: API key, token (the browser CSRF token is separate)

**Dashboard session**: The owner-only browser credential set by the single-use link printed by `mergeyard start` (or `mergeyard open`). It lives in origin-scoped browser storage, travels only to the runtime's exact scheme, host and port as the `X-Mergeyard-Session` header, and rotates with the runtime process.

**Dashboard handoff page**: The owner-only local HTML page that redirects the browser to a single-use dashboard link, so the token never enters a process command line. Linux keeps it in a non-hidden home directory so confined browsers can read it; macOS passes the URL to the default HTTP browser over stdin instead.

**Scoped GitHub failure**: A GitHub read or write failure attributed to one repository or run. It is reported on the dashboard and skips only its subject; local-state failures still stop dispatch everywhere.
