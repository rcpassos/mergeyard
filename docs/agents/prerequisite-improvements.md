# Implementation prerequisites and test preflight

Status: implemented and locally verified, 2026-10-07.

This scope comes from the retrospective on issue #62 and [PR #74](https://github.com/rcpassos/mergeyard/pull/74).
That session prepared research tooling but could not observe the required live
usage-limit failure. Test execution also encountered sandbox restrictions before
the full suite could run successfully.

## Ownership

| Deliverable | Owner | Location |
|---|---|---|
| Rule for checking ticket-required external conditions | Reusable agent skills | One shared reference read by `/implement` and `/implement-spec` |
| Executable checks for Mergeyard's offline test environment | Mergeyard repository | Python standard-library script under `scripts/`, invoked first by `make test` |

The skill rule decides whether work can proceed. The repository script checks
the concrete capabilities its tests need. Each deliverable can be implemented
and verified independently.

## Reusable prerequisite rule

Apply the rule when an acceptance criterion requires a real observation,
external access, or a specific environment. Ordinary code-only tickets incur
no additional probes.

Before implementation:

1. Identify the required condition and the evidence that would establish it.
2. Inspect relevant existing evidence first. Distinguish captured observations,
   source expectations, synthetic fixtures, and assumptions.
3. When a live probe is needed, use existing ticket/session authorization and
   state its invocation and time bounds. Existing authorization remains valid;
   a missing authorization requires clarification before the dependent action.
4. If the required condition is unavailable or unverified, stop the affected
   ticket before building preparation tooling. Report the unmet criterion,
   observed evidence, and the next capture step or condition needed to resume.
5. Preparation tooling can proceed when explicitly scoped as separate work.
   Its completion does not satisfy the original live-evidence criterion.

`/implement-spec` must apply the same rule before an implementer starts a ticket.
An unavailable condition stops that ticket and its dependent work; unrelated
unblocked tickets can continue. Updating `/implement` alone is insufficient
because `/implement-spec` dispatches implementers directly to TDD.

The reusable execution guidance also requires an owned failed test run to be
stopped and confirmed exited before its replacement starts. This is agent
execution guidance, not a new full-suite supervisor in Mergeyard.

### Completion criteria

Scenario checks and review of both entry points must establish:

1. An ordinary code-only ticket launches no prerequisite probe.
2. Relevant existing evidence can establish a required condition without a new
   live request; source expectations and synthetic fixtures cannot substitute
   for a required live observation.
3. A needed live probe reuses existing authorization and has explicit bounds.
4. An unavailable condition produces a concrete blocker and resume step before
   implementation or preparation work begins.
5. Separately scoped preparation work retains the original unresolved criterion.
6. A blocked ticket in `/implement-spec` does not stop unrelated unblocked work
   or authorize dependent work.
7. Both entry points reach one authoritative rule; test-run replacement guidance
   preserves process ownership and prevents overlapping retries.

## Mergeyard test preflight

Provide a standalone script, proposed as `scripts/test_preflight.py`, that can
run before Go compilation. Invoke it first from the existing `make test` target
and document its direct use and remedies in the development instructions.

Check the capabilities used by the normal offline suite:

- Go meeting the repository's declared minimum, and Python 3.9 or later.
- Effective temporary-directory and Go build-cache access, honoring configured
  paths including `TMPDIR`, `GOTMPDIR`, and `GOCACHE` where applicable.
- Local Git operations and execution of an owned temporary executable script.
- Binding an ephemeral localhost port.
- An actual isolated tmux launch that runs the process-identity checks used by
  the session wrapper and produces an observable sentinel.

The tmux check covers both absolute `/bin/ps` use and `ps` through `PATH`,
including the process and boot identities expected by the existing session code.
Finding an executable or reading its version alone does not establish launch
capability. Use an owned socket and temporary directory for every probe.

`make test` requires the full offline environment: missing or unusable tmux
fails early. Direct focused test commands retain their existing behavior.

Reject these inherited live-integration flags when their value is exactly `1`:

- `MERGEYARD_GITHUB_INTEGRATION`
- `MERGEYARD_GITHUB_PR_INTEGRATION`
- `MERGEYARD_SCHEDULER_INTEGRATION`

The existing explicitly invoked live integration commands remain available.
The preflight diagnoses problems and gives concrete remedies. It honors
configured paths rather than relocating caches or retrying automatically.
A warm read-only module cache is not rejected merely for being read-only.
Python bytecode-cache access is unnecessary because the test recipes disable
bytecode writes.

The preflight makes no service calls, authentication checks, or downloads.
It does not use `doctor.Check`, which loads application configuration and checks
real accounts and repositories. Small local probe patterns may be reused without
expanding the production doctor API.

Use a 30-second overall deadline, with at most five additional seconds for
cleanup. On success, failure, timeout, or interruption, terminate owned probe
processes and remove owned sockets and temporary resources. Preserve existing
sessions, credentials, configuration, repositories, and caches.

### Completion criteria

1. The standalone script runs before any Go compilation; `make test` runs it
   before starting the suite.
2. Each failed prerequisite returns nonzero, names the failed capability, and
   gives a remedy. A failing preflight prevents suite startup.
3. Missing and installed-but-unusable tmux both fail early; a successful check
   observes the sentinel after process-identity checks.
4. Each enabled live flag is rejected; normal offline invocation needs no
   credentials, service access, or paid model request.
5. Configured paths are respected, and a warm read-only module cache does not
   fail solely because it is read-only.
6. Success, failure, timeout, and interruption all meet the ownership, cleanup,
   and deadline requirements.
7. Tests exercise the public script and Make interfaces with fake executables
   and temporary paths. One real isolated tmux smoke check verifies actual
   launch capability. Tests do not depend on private helpers or live accounts.
8. Existing lint, build, offline-suite, and CI checks pass with the full required
   environment. CI uses the same preflight through `make test`.

## Scope boundaries

Test-progress reporting, automatic environment repairs, a full-suite supervisor,
a shared executable prerequisite framework, and ticket-generation changes are
outside this work. Mergeyard's runtime harness recovery behavior is unchanged.
Issue #62 remains open until its required real failure evidence is captured.

Keep this design and its implementation separate from PR #74.

## Implementation and skill deployment

`scripts/test_preflight.py` provides the standalone check, and `make test` invokes
it before script and Go tests. The public CLI/Make checks are in
`scripts/test_preflight_test.py`.

The portable skill sources live under `agent-skills/`: `skills/implement`,
`skills/implement-spec`, and `references/implementation-prerequisites.md`.
The shared reference sits outside both skill folders. Copies are deployed under
the same relative layout in `~/.agents/`; update these three source files together
before redeploying. The installed entry points preserve their original invocation
metadata. This bundle adds no skill discovery surface or executable framework.

An independent read-only decision rehearsal checked six scenarios: ordinary
offline work, a successful capture with an exhausted probe allowance, reusable
real failure evidence, synthetic evidence with separate preparation work,
mixed blocked/unblocked spec tickets, and replacement of a still-running failed
test. All selected the expected work, blockers and ownership actions.

The bundled skill validator could not run because PyYAML is unavailable, and its
schema excludes the existing `disable-model-invocation` field. Validation instead
confirmed byte-for-byte preservation of the original frontmatter, resolution of
both entry points' shared-reference links, and the independent scenario outcomes.

Local validation passed: Python 3.9 syntax checks, the 17 public preflight checks
(including a real isolated tmux smoke check), `make test`, `make lint`, `make build`,
and the committed dashboard-asset comparison. Unchanged Go packages reused their
valid cached test results. The existing CI workflow reaches the same preflight
through `make test`; no remote CI run was started by this implementation task.

The two-axis review found one shutdown-cleanup bug and one fixture-simplification
recommendation. Both were addressed and the cleanup regression passed. The
preflight now retains an owned foreground server, disables client auto-start,
and confirms termination before removing its socket. A denied shutdown command
causes a failure after bounded owned-process cleanup, rather than false readiness.
