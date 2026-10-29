# Development

Start with the [architecture overview](architecture.md) for component boundaries
and the current behavior references.

For a source build, see [installation](install.md#build-from-source). Use Go
1.26.10 or newer, Node.js 24 for dashboard assets, and Python 3.9+ for script
checks. The public website has its own [preview guide](website.md).

## Checks and generated assets

```sh
make lint   # gofmt, go vet, and an unused-code check
make build  # rebuild dashboard assets with Node.js 24, then compile Go
```

CI runs the complete Go suite in four parallel jobs. For local changes, run
`make lint` and only the specific tests you wrote or changed:

```sh
go test -run '<TestName>' ./<package>/
```

Push your branch and check `gh pr checks` on its pull request for the full
suite. The standalone `make test` command runs the offline preflight, script
checks, all Go tests, and research checks; it can take many minutes.

Commit generated `web/static/` files, `site/theme.css` and `site/favicon.svg`
with dashboard source changes.

`make lint` also runs the pinned Staticcheck unused-code check (`U1000`), so code
left behind by a change fails lint instead of accumulating. It downloads Staticcheck
on first use, so the first lint run needs network access to the Go module proxy.

The public project website is maintained separately in `site/` and deployed to
GitHub Pages. See [website development and deployment](website.md) for
local preview and the one-time Pages setup.

`make test` requires Go meeting `go.mod`, Python 3.9+, Git, tmux, writable
temporary/build-cache paths, executable temporary files, localhost ports, and
process inspection. Its preflight checks these before Go compilation, including
an isolated tmux launch. Run it directly for diagnostics:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_preflight.py
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p '*_test.py' -v
```

The preflight has a 30-second deadline and up to five seconds for owned-resource
cleanup; `--timeout <seconds>` can shorten the deadline. It honors configured
cache/temp paths and makes no service calls, authentication checks, repairs, or
downloads. A warm read-only module cache is allowed. Missing tools, denied local
ports/process inspection, and unusable tmux fail with a capability and remedy.
The smoke check owns a foreground tmux server and confirms termination before
reporting readiness, including bounded cleanup when its shutdown command fails.
The shutdown command gets at most two of the five cleanup seconds; three seconds
are reserved for forced termination, reaping, and owned-resource removal.
For a denied host build cache, choose an accessible task cache, for example
`GOCACHE=/tmp/mergeyard-go-cache make test`. For sandbox restrictions, rerun only
in an authorized environment that permits the named capability, after the failed
run exits. Direct focused test commands retain their existing behavior.

`make test` rejects `MERGEYARD_GITHUB_INTEGRATION=1`,
`MERGEYARD_GITHUB_PR_INTEGRATION=1`, and `MERGEYARD_SCHEDULER_INTEGRATION=1`.
Unset them for the offline suite. Deliberate live integration checks use the
existing recipes in [GitHub fixtures](../internal/github/testdata/README.md) and
[scheduler tests](scheduler.md#tests).

## Releases

Pushing a version tag such as `v0.1.0` triggers tests, builds all four platform
archives, and publishes them with checksums, the installer, and release notes.
See [release management](releases.md) for publishing and local release checks.
