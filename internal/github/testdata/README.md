# GitHub adapter fixtures and live test

JSON responses were recorded with the authenticated `gh api` CLI on 2026-10-04
from `rcpassos/mergeyard`. Only fields consumed by the adapter are retained; user
and repository metadata are omitted. Bodies, labels, states, timestamps, URLs,
and dependency summaries are unchanged.

- `open-issues.json`: `repos/rcpassos/mergeyard/issues?state=open&per_page=100`,
  reduced to issues #18 and #6 in response order.
- `open-blockers.json`: `repos/rcpassos/mergeyard/issues/7/dependencies/blocked_by`
  (#3, open).
- `closed-blockers.json`: `repos/rcpassos/mergeyard/issues/6/dependencies/blocked_by`
  (#2, closed).
- `closed-issue.json`: `repos/rcpassos/mergeyard/issues/2`.

Tests concatenate recorded arrays to simulate multiple `gh api --paginate` pages
and combine open and closed blockers. Malformed payloads, pull requests, hostile
text, and command failures are generated in tests rather than recorded fixtures.
The body of #6 still says `Blocked by: #2`, while GitHub's native summary correctly
reports zero unresolved blockers.

## Opt-in live integration test

Normal tests require neither GitHub credentials nor network access. The live test
requires an authenticated `gh` executable and a **dedicated** repository named
`<owner>/mergeyard-integration-test`. It never reads Mergeyard configuration or
uses the current Git remote to choose a repository.

Prepare an open issue and an existing repository label named
`mergeyard-integration-test`, without applying that label to the test issue.
Optionally add native dependencies on one open and one closed issue to exercise
blocker filtering against live GitHub. The test lists issues, reads the issue's
state and native blockers, then adds and removes the test label. Cleanup attempts
to remove the label if a later assertion fails. Run only one live test against the
same issue at a time.

```sh
MERGEYARD_GITHUB_INTEGRATION=1 \
MERGEYARD_GITHUB_TEST_REPO=<owner>/mergeyard-integration-test \
MERGEYARD_GITHUB_TEST_ISSUE=<number> \
go test ./internal/github -run '^TestGitHubIntegration$' -count=1 -v
```

## Client behavior

`github.New(nil)` uses `exec.CommandContext` to execute `gh` directly. Tests inject
a `CommandRunner` at this process boundary. All operations accept a context;
callers should set a deadline to bound slow processes as well as retry waits.

The client reads all REST pages, excludes pull requests from the issue queue, and
rejects missing/invalid issue numbers, creation times, and states. Blockers are
read only from the native `dependencies/blocked_by` endpoint and filtered by
state. `Issue.Dependencies` exposes the native unresolved count when supplied;
a missing summary is represented by nil.

Label additions use JSON stdin; removals escape the label as one URL path
segment. Repository and issue-number arguments are validated before executing
`gh`. Issue bodies never become command arguments or stdin.

Transient transport failures, HTTP 408/429/5xx, and rate-limited HTTP 403 responses
receive at most three attempts with 250ms and 500ms context-aware backoff. Other
failures return immediately. `*github.Error` exposes a stable `Code` and unwraps
the original error:

- `github.invalid_input`
- `github.invalid_response`
- `github.not_logged_in`
- `github.forbidden`
- `github.not_found`
- `github.rate_limited`
- `github.unavailable`
- `github.command_failed`
- `github.canceled`
