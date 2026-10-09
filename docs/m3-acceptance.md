# M3 combined recovery acceptance

Scope: [#72](https://github.com/rcpassos/mergeyard/issues/72), following the
closed [availability-check #68](https://github.com/rcpassos/mergeyard/issues/68)
and [manual/restriction interaction #71](https://github.com/rcpassos/mergeyard/issues/71)
tickets. The approved scheduler/runtime seams and safety rules come from
[the M3 specification #60](https://github.com/rcpassos/mergeyard/issues/60).

## Shipped capabilities and live evidence

| Harness | Temporary-limit detection | Exhausted-credit detection | Same-conversation manual handback |
| --- | --- | --- | --- |
| Claude | Disabled; failures consume the ordinary attempt budget. [#61](https://github.com/rcpassos/mergeyard/issues/61) evidence and [#81](https://github.com/rcpassos/mergeyard/issues/81) binding remain open. | Disabled; failures consume the ordinary attempt budget. [#80](https://github.com/rcpassos/mergeyard/issues/80) evidence and [#83](https://github.com/rcpassos/mergeyard/issues/83) binding remain open. | Prior [Q10 observations](research/harness-spikes.md#q10--interactive-takeover-and-headless-handback). |
| Codex | Disabled; failures consume the ordinary attempt budget. [#62](https://github.com/rcpassos/mergeyard/issues/62) evidence and [#82](https://github.com/rcpassos/mergeyard/issues/82) binding remain open. | Disabled; failures consume the ordinary attempt budget. [#80](https://github.com/rcpassos/mergeyard/issues/80) evidence and [#83](https://github.com/rcpassos/mergeyard/issues/83) binding remain open. | [#63 report](research/codex-m3/report.md), [sanitized native evidence](research/codex-m3/evidence.json), and [reproduction recipe](research/codex-m3/README.md). |

Codex's captured observation used `codex-cli 0.156.1`, one disposable linked
worktree and the exact same conversation identity. Headless execution exited
before interactive resume. Interactive execution exited before headless
handback. The final native completion recovered the conversation-only manual
marker under explicitly reapplied headless settings. Normal interactive
`on-request` / `workspace-write` permissions were observed. This is continuity
evidence for that version and fixture, not a new usage-limit or credit capture.

M3 uses the detection-disabled alternative explicitly accepted by #72. Neither
production adapter currently enters automatic usage-limit waiting or a credit
block. `usage_limits` settings do not enable native detection. Check availability
and Retry-as-probe stay unavailable. Successful Claude `allowed_warning`
records, synthetic failures, and documentation-only formats are not rejected
request evidence and do not satisfy the open native binding gates.

`TestProductionHarnessLimitFailuresConsumeAttemptBudget` verifies both shipped
adapters in implement, review, and fix: two unsuccessful executions spend a
configured two-attempt budget, require attention, leave harness availability
unchanged, and cannot invoke Check availability.

## Combined offline acceptance

The [combined recovery matrix](../internal/scheduler/combined_recovery_test.go)
uses real temporary SQLite, four distinct local bare Git repositories and their
managed worktrees, an isolated tmux server, fake GitHub, fake harness executables,
and the injected clock. Only fake adapters return normalized restrictions. This
does not claim native detection has been enabled or live-tested.

`TestCombinedRecoveryAcrossRepositories` runs both mixed harness pairings under
temporary limits and exhausted-credit blocks. Each scenario establishes:

- The originating implementation retains its edits, role identity and slot.
  Credit-originating attention also retains its slot.
- Another repository's other-harness implementation completes, then waits
  before its blocked reviewer starts. Account waiting spends no review attempt.
- Work using the other harness for both roles reaches `READY_TO_MERGE` with
  approval and passing CI for its published head. It remains open for human merge.
- Readiness releases global and repository slots. An active run at the repository
  cap holds its next issue; the global cap holds an otherwise eligible repository.
- Manual takeover retains the slot and dirty manual work. Stop releases capacity
  while preserving its worktree. No new blocked-implementer issue is claimed.
- Handback publishes manual work and waits on the blocked next phase. Timed reset
  resumes automatically; explicit Retry selects one credit probe. Recovery
  resumes the exact implementer and a separate reviewer without spending a new
  engineering attempt allowance or creating duplicate PRs/commits.

The [subprocess fixture](../internal/scheduler/combined_restart_test.go) kills
and reaps a real control plane, then reopens the same locked workspace at three
cross-feature boundaries: observing manual/waiting/live independent work;
after handback push before its journal finishes; and after resumed execution
launch before the launcher returns. The suite checks the adopted live session,
manual-state persistence, preserved files, final PR/commit counts, recovered
probe identity, and exactly nine actual executable launches per scenario.

`TestOrphansRemainVisibleAndPreservedBehindDispatchGates` exercises claims,
worktrees and live sessions together under pause, repository disablement, and
full capacity. Repeated ticks and Reconcile keep all findings visible, emit each
unchanged finding once per scheduler lifetime, preserve files/processes, and
neither reset labels nor adopt ambiguous work.

## Existing flow and safety coverage

| Originating ticket / guarantee | Offline coverage retained by the full suite |
| --- | --- |
| [#64](https://github.com/rcpassos/mergeyard/issues/64): implementation waiting | `TestTemporaryLimitPreservesWorkAndResumesAfterRestart`, `TestTemporaryLimitSurvivesControlPlaneKill`, `TestTemporaryLimitGatesAcrossRepositoriesAndRetainsSlots` (including global concurrency one). |
| [#66](https://github.com/rcpassos/mergeyard/issues/66): review/fix waiting | `TestReviewAndFixUsageLimitsSurviveControlPlaneKill`, `TestReviewAndFixWaitAllowancesAreIndependent`, `TestUsageLimitedReviewRetainsPinnedHead`. |
| [#67](https://github.com/rcpassos/mergeyard/issues/67): selected-run credit recovery | `TestCreditProbeSurvivesControlPlaneKill`, `TestCreditProbeConcurrentSelectionStopAndWaitingRetry`, stale proof and native completion regressions. |
| [#68](https://github.com/rcpassos/mergeyard/issues/68): independent availability checks | `TestHarnessCheckSurvivesControlPlaneKill`, stopped-run checks, concurrent Check/Retry gate and request-protection tests. |
| [#69](https://github.com/rcpassos/mergeyard/issues/69), [#70](https://github.com/rcpassos/mergeyard/issues/70), [#71](https://github.com/rcpassos/mergeyard/issues/71): manual control and handback | Takeover process/restore crash tests, `TestHandbackExistingPRAlwaysGetsIndependentReviewAndOneDurableRound`, `TestBlockedHandbackRecoversAfterControlPlaneKill`, `TestCreditProbeTakeoverRecoversAfterControlPlaneKill`, manual/Retry/Stop races. |
| Reviewed-head and CI gates | CI contract/matrix and kill/restart tests require independent approval for the current published head and matching CI evidence; changed heads invalidate readiness. |
| No force push / human merge / stopped-work preservation | Git ownership, divergent-head and handback refusal tests; ready-to-merge and early-merge recovery tests; Stop preservation and redispatch regressions. |
| CLI/browser protections | Existing loopback, Host, Origin, CSRF, workspace identity, terminal escaping, exact conversation and interactive ownership tests. |

These operational flows remain documented in their originating tickets; #72
adds combined verification and completes their existing diagnostic surfaces.
Status reports the control plane's loaded effective configuration, source,
resolved workspace, caps and role settings. Run/status/settings retain reset
sources, reasons, probe history and explicit grants; Settings also displays
handback grants for runs without a harness-wait history.

## Reproduction

Run from the repository root with the offline preflight prerequisites available:

```sh
go test ./internal/scheduler -run '^(TestCombinedRecoveryAcrossRepositories|TestOrphansRemainVisibleAndPreservedBehindDispatchGates|TestProductionHarnessLimitFailuresConsumeAttemptBudget|TestStatusExposesLoadedEffectiveConfigurationAndSource)$' -count=1 -timeout=5m
go test ./cmd/mergeyard -run '^TestCLIStatus' -count=1
make test
make lint
```

Normal tests require no credentials, network service, or paid model calls. The
preflight rejects inherited live integration flags. Existing opt-in live GitHub
acceptance checks require a dedicated test repository. No live GitHub acceptance
or paid harness capture is needed for this detection-disabled acceptance path.
