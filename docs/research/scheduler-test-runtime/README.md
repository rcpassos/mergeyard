# Scheduler test runtime (#295)

The fast scheduler package passed all **158 tests in 15.952 seconds** on
2026-10-29 (Go 1.26.10, macOS/arm64, uncached test execution). The separate
integration package contains 300 tests.
The one-minute target applies to `go test ./internal/scheduler/`. The full
recursive suite still includes the real-process integration package.

## Test layout

```text
internal/scheduler/
├── *_test.go          scheduler policy, native artifact parsing, durable journals
├── testkit/           shared fake boundaries and real-process fixture helpers
└── integration/       real Git, hooks, worktrees, tmux, and actual process death
```

The split preserves the existing test coverage in CI's `./...` selection. It
moves expensive integration work into an explicit package rather than skipping
it or hiding it behind an opt-in flag. The existing live GitHub test retains its
original opt-in flag.

The three largest process-restart matrices now use explicit outcomes and actual
SQLite reopen/crash boundaries. They retain all 36 fix-recovery, 40 usage-limit,
and 18 missing-session combinations. Nine representative cases still kill an
actual control-plane process: three fix boundaries, five usage-limit boundaries,
and one missing-session boundary. Other real Git and process assertions remain
in the integration suite.

Use `NewScenario` for scheduler decisions. Queue native results, held sessions,
missing-conversation diagnostics, or failures through its runner. The real
harness adapters still build invocations and validate their native artifacts.
Git refs and sessions survive reopening the actual store; controller-local state
does not. Use `TmuxFlow` when a test's assertion requires real Git, wrapper
behavior, ownership inspection, inherited leases, or OS process death.

Queue deadline tests use `testing/synctest`. The unchanged five-second timeout
and backoff case took 5.02s/5.01s normally and 0.02s/0.02s under virtual time.
Production deadlines and timeout attribution remain unchanged.

## Diagnosis

Baseline commit: `cc084fe3ce64a725daa48cd952d12346eed7d35f`.
The successful [CI run][ci] supplied complete `go test -json -count=1` timings:
454 passed tests, one skipped live integration test, and **674.34 seconds of
summed top-level test time** across four concurrent shards. The sum is accumulated
work, not a measured serial local runtime. The scheduler package took
151.365s, 182.161s, 161.064s, and 179.892s in the four shards.

A local serial profile was interrupted after 89 tests had passed with 530.74s
of accumulated test time. The ordinary issue-to-draft/restart test reproduced
at 3.40s and 3.54s:

```sh
go test -json -count=2 -run '^TestIssueToDraftPRAndRepeatedTicksAfterRestart$' ./internal/scheduler/integration/
```

The minimized claim-to-draft flow took 1.054s/1.059s with real tmux and Git.
Replacing tmux with direct execution of the same shell fixture took
1.133s/1.043s. In a second paired comparison, direct execution with real Git
cost 1.117s/1.038s, while substituting the Git boundary cost 0.446s/0.457s.
Fixture construction alone took 0.19–0.21s, Git preparation 0.28–0.30s, and
publication 0.21–0.22s. Replacing tmux alone was insufficient.

The original fixture had 231 textual `localFlow(` occurrences, including its
definition. Table-driven tests multiplied those invocations. The 35 original
`time.Sleep`/`time.After` sites mostly polled or guarded failures; the two
minute-long sleeps parked helper processes that their parents immediately
killed. Counting them as two minutes of unconditional suite delay was incorrect.

### Slowest 20 baseline tests

These are top-level timings, excluding nested subtests to avoid double counting.
All 454 baseline timings are in [ci-timings.json](ci-timings.json). The top 20
accounted for 232.60s, or 34.5% of accumulated time.

| Test | Seconds |
| --- | ---: |
| `TestFixRecoversAfterControlPlaneKill` | 36.45 |
| `TestReviewAndFixUsageLimitsSurviveControlPlaneKill` | 24.92 |
| `TestStopPreparingRunKeepsOrReleasesTheSubmodulesOfAnInterruptedCheckout` | 15.86 |
| `TestMissingSessionRecoverySurvivesControlPlaneKill` | 15.09 |
| `TestHarnessPairingsRejectInvalidPriorOutcomes` | 11.89 |
| `TestCombinedRecoveryAcrossRepositories` | 11.17 |
| `TestHandbackBlockedReviewerPreservesGrantAndMixedRoles` | 10.76 |
| `TestMergeStopsChildrenAfterWrapperExitedWhileOffline` | 10.36 |
| `TestHarnessPairingsWaitBeforeReviewOrFixLaunch` | 10.30 |
| `TestQueueScopesRefusedIssueDeadlineAndBacksOff` | 10.06 |
| `TestReviewAndFixWaitAllowancesAreIndependent` | 9.49 |
| `TestTakeoverActiveCreditProbeRetainsBlockAndRestoresReviewer` | 9.35 |
| `TestMissingSessionFreshResultAndUnrelatedStartupRemainBounded` | 9.31 |
| `TestReviewAndFixLimitsLeaveOrdinaryAttemptsAvailable` | 8.35 |
| `TestClearingConfiguredListsWaitsForLiveOwnedStepsAfterRestart` | 7.28 |
| `TestRetryCommittedBaseRepairResumesPublication` | 7.05 |
| `TestMergeCleanupRemovesSlowWorktreeBeyondMaintenanceBudget` | 6.51 |
| `TestHungGitPublicationTimesOutAndTheNextTickProceeds` | 6.30 |
| `TestBaseFailuresHonorCIActionPriorityAndUncertainty` | 6.08 |
| `TestHandbackExistingPRAlwaysGetsIndependentReviewAndOneDurableRound` | 6.02 |

## Concurrency and ownership

Real fixtures now give each managed Git instance its own copied environment,
including its isolated configuration file and local remote rewrite. This removes
process-wide `t.Setenv` from ordinary fixtures. The regression
`TestManagersKeepConcurrentCommandEnvironmentsIsolated` checks simultaneous
managers using different remotes and authors, including defensive copying of
configuration overrides. A second regression checks that inline `core.sshCommand`
overrides on a reused workspace have independent probe-cache entries.

Parallel execution exposed a workspace-lock release bug. An in-flight fork can
retain the lock's open file description until exec. Closing the runtime's file
descriptor alone then leaves ownership held by that inherited copy. The new
`TestCloseReleasesOwnershipWithInheritedDescriptor` reproduces this deterministically
with `dup`: it failed in 0.13s before the fix. Runtime shutdown now explicitly
unlocks after SQLite closes; failed startup releases ownership the same way.
Live-owner exclusion and failed-startup tests still pass.

Interactive leases deliberately survive in children, so their ownership behavior
is preserved. Integration tests that transfer those leases stay serial. The
real fixture caps concurrent scenarios and verifies that owned process groups
have exited before deleting temporary workspaces. Blanket high concurrency was
rejected after it caused real-process deadlines to fail under local load.

## Validation

```sh
make lint
go test -count=1 ./internal/scheduler/
go test -run '<changed integration test>' ./internal/scheduler/integration/
```

CI continues to run the full recursive suite, including both scheduler packages,
in four shards. The fast-package measurement is a scope-specific result; it is
not a claim that all integration work now runs in 16 seconds.

The temporary profiling probes were removed from the active test tree and from
this report's deliverables. No debug instrumentation is retained.

[ci]: https://github.com/rcpassos/mergeyard/actions/runs/40596095428
