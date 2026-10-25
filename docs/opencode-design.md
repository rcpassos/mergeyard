# OpenCode integration design

Status: design decisions agreed in the interview; compatibility probes and implementation remain. OpenCode support is not implemented.

The implementation spec is published as [GitHub issue #272](https://github.com/rcpassos/mergeyard/issues/272), labeled `ready-for-agent`. It includes the user-confirmed test seams, acceptance coverage, and implementation decisions.

## Agreed scope

- OpenCode is a third harness, alongside Claude Code and Codex, to give users more harness choices.
- OpenCode can perform either role. Implementer and reviewer select their harness independently, including OpenCode/OpenCode and mixed pairings with existing harnesses.
- The goal is the existing Mergeyard role workflow with another harness choice.
- OpenCode owns provider configuration and credentials. Mergeyard selects the role's model and effort, using OpenCode's provider-qualified model identity and mapping effort to a native variant.
- Mergeyard may manage a local OpenCode server if needed to preserve the structured phase-result contract. Its lifecycle must not require the user to configure an additional service.
- Support must cover both OpenCode Go subscriptions and pay-as-you-go usage.
- Restrictions follow the affected allowance, balance, or budget rather than blocking the whole OpenCode harness. Independently available funding paths continue, subject to the existing concurrency limit. Shared balances remain shared restrictions.
- Recovery checks target the same availability scope as the restriction. A successful Go request cannot clear a restriction on unrelated pay-as-you-go credits.
- Subscription limits and budget caps with a verified scheduled reset use timed waits. Depleted credits require explicit recovery. Unknown errors retain ordinary failure handling.
- Mergeyard honors the owner's native Extra Usage setting, including paid overflow when enabled. It does not enable Extra Usage or automatically change a run's provider/model.
- Initial support targets released OpenCode V2. Doctor must distinguish the installed major and enforce a minimum version established by compatibility probes.
- For OpenCode, an omitted model is resolved to an effective provider/model when the issue is claimed, and that actual identity is preserved throughout the run. A claim whose default cannot be resolved is refused with an actionable explanation.
- Existing Mergeyard role settings remain the user-facing configuration. The adapter supplies native agent configuration for each phase; initial support adds no OpenCode-native preset selector.
- The OpenCode adapter may use an attempt-owned typed reporting tool in place of a harness-native schema-constrained final message. Mergeyard's canonical phase-schema validation is authoritative. Success requires independent native completion evidence and a valid report bound to the current attempt; missing, malformed, or stale reports fail the attempt.

[ADR 0008](adr/0008-availability-follows-the-restricted-resource.md) records the availability boundary and recovery distinction. [ADR 0009](adr/0009-resolve-opencode-default-model-at-claim.md) records effective model resolution. [ADR 0010](adr/0010-opencode-reports-through-an-attempt-owned-tool.md) records the report boundary. These are agreed design changes; current runtime restrictions are still harness-wide.

This revises the PRD's earlier deferral of OpenCode. No release milestone has been selected.

## Existing design constraints

- ADR 0001 resolves harness, model, effort, and skills at claim and preserves those role settings throughout the run. A harness change during an existing run is outside the existing design.
- ADR 0002 defines tiers separately for each role and harness.
- The harness adapter translates commands and results; the runner owns execution.
- Current adapters must support structured results and session resume. Exact-session takeover and restart recovery are part of the existing workflow.
- Unknown native failures remain ordinary failures. Usage-limit detection requires captured evidence before it can be enabled.

## Behavior carried forward from existing harnesses

- Implement and fix run unattended using the effective native permission configuration; attempts record the permissions actually applied.
- Review may run tests and builds, but cannot alter repository files, the index, commits, branches, or remote state. Disable native editing tools where possible and retain the existing snapshot/check/restore enforcement and verdict rejection for unauthorized changes.
- Skills remain optional per-role settings. The adapter must explicitly request configured skills on fresh and resumed phases; discovery alone is insufficient. Unsupported settings are refused, not silently dropped.
- Doctor checks configured skills using native discovery locations, reporting missing or unverifiable skills consistently with existing harnesses. Mergeyard does not install or synchronize them.
- Model and effort values remain native pass-through settings rather than a Mergeyard-maintained catalog.

## Documentation findings

Checked 2026-10-24 using Context7 and the current official documentation. These are documentation findings, not live compatibility evidence: OpenCode is not installed in this workspace's execution environment.

- The released V2 CLI has [public installation instructions](https://opencode.ai/v2/docs/). Version [2.1.4](https://github.com/anomalyco/opencode/releases/tag/v2.1.4) is a research candidate, not yet a validated minimum. V1 remains a separate update track.
- The [V2 CLI](https://opencode.ai/v2/docs/cli/) documents headless execution and explicit session selection. V1 examples must not be assumed compatible; consult the [migration guide](https://opencode.ai/v2/docs/migrate-v1/).
- JSON event output alone does not establish a schema-validated phase result. Released V2's [command definition](https://github.com/anomalyco/opencode/blob/v2.1.4/packages/cli/src/commands/commands.ts#L385) has no schema-output flag, and its [prompt input](https://github.com/anomalyco/opencode/blob/v2.1.4/packages/schema/src/prompt-input.ts) has no report-schema field. Current [V2 API documentation](https://opencode.ai/v2/docs/api) does not establish native schema-constrained final reports.
- Correction to the early research: `@opencode-ai/sdk/v2` schema-output examples on the V1 documentation site describe the legacy API, not the released V2 product. V2 uses `@opencode/client`; its [migration guide](https://opencode.ai/v2/docs/migrate-v1/#server-api-and-clients) requires porting V1 integrations. A managed V2 server is allowed by the design but is not an established solution to report generation.
- A candidate adapter-owned report route exists: a local per-attempt [plugin](https://opencode.ai/v2/docs/plugins/) can register a [typed reporting tool](https://opencode.ai/v2/docs/build/plugins/#tools) and write an artifact at a fixed attempt-owned path outside the worktree. OpenCode supplies the plugin runtime; no additional runtime install or tracked repository edit is required by this mechanism. This is a schema-described tool call, not a native structured final assistant message, and remains unprobed.
- The [released tool runtime](https://github.com/anomalyco/opencode/blob/v2.1.4/packages/core/src/tool/runtime.ts#L26) validates tool input, but a raw-schema compilation failure can bypass that validation. Mergeyard's own canonical schema validation must remain authoritative for any plugin-produced report.
- The [V2 run command](https://opencode.ai/v2/docs/cli/commands/#run) supports standalone execution instead of the default shared background service. Standalone process ownership does not imply isolated database, credentials, or project configuration.
- [V2 permissions](https://opencode.ai/v2/docs/permissions/) distinguish allow, ask, and deny. Automatic approval preserves effective explicit denials; native policy precedence and adapter-supplied reviewer rules need verification.
- [V2 skills](https://opencode.ai/v2/docs/skills/) support native explicit loading. Discovery alone is not evidence that a configured role skill was invoked.
- [Go](https://opencode.ai/v2/docs/console/go/#usage-beyond-limits) supports optional Extra Usage that draws on the Console pay-as-you-go balance after the subscription allowance is exhausted. A native billing fallback can therefore occur without changing the role's selected model or harness.
- [Console billing](https://opencode.ai/v2/docs/console/billing/) and [budgets](https://opencode.ai/v2/docs/console/budgets/) distinguish depleted credits from monthly budget caps. Budget caps have a scheduled reset, unlike an empty prepaid balance. Go extra usage and direct pay-as-you-go requests can share the same balance.
- Existing Mergeyard restrictions and recovery probes are harness-wide. Keeping independently available OpenCode funding paths usable would require narrower restriction scopes and recovery checks that target the affected scope. A successful Go response must not clear an unrelated pay-as-you-go credit block.

## Design frontier

No product decisions remain open from the interview. The OpenCode reporting-tool exception is accepted with strict canonical validation and independent native completion evidence; it does not authorize success based on prose or the presence of an arbitrary JSON file.

The technical investigations below must establish how to meet the agreed behavior; any required compromise reopens the affected decision. No release milestone was selected.

## Investigation required by the agreed design

- Establish stable identities for affected allowances, shared credit balances, and budgets from the effective provider/workspace/model configuration and native evidence. Do not assume a provider identifier always equals an availability scope.
- Preserve early session identity, structured reports, native completion evidence, stop, restart, exact-session takeover, and independent role conversations.
- Capture rejected-request fixtures for Go limits, depleted pay-as-you-go funds, and scheduled budget caps before enabling their native classifiers. Establish reliable reset metadata and ambiguous-signal behavior.
- Verify Go Extra Usage interactions: a depleted shared balance can affect direct pay-as-you-go and Go overflow, while remaining subscription allowance may still be usable.
- Ensure recovery cannot clear another scope's block and that narrower availability does not bypass the existing run-concurrency policy.
- Verify native skill invocation and unattended permissions without silently overriding explicit denials. Do not assume OpenCode's built-in plan agent enforces Mergeyard's review contract.
- Isolate any managed OpenCode server from unrelated interactive sessions and runs; verify process ownership, worktree selection, cancellation, and restart behavior for the selected version.
- Prove reporting-plugin loading, canonical schema compatibility, invalid-argument correction, missing-report failure, duplicate/stale report handling, and interrupted/resumed report persistence. Report paths and attempt identities must be controlled by Mergeyard rather than supplied by model arguments.

## Acceptance requirements

- OpenCode works independently in both roles, including all mixed pairings with Claude Code and Codex, through implementation, bounded review/fix, CI repair, and merge readiness.
- Role settings, resolved OpenCode model identity, session identity, restrictions, and recovery targeting survive control-plane restart.
- Takeover resumes the exact implementer conversation; stop, handback, retry, and interrupted execution preserve existing workflow guarantees.
- Default-model resolution, native variants, configured skills, permissions, and review mutation enforcement have compatibility evidence for the supported V2 version.
- Each successful attempt has both independent native completion evidence and a valid current-attempt report from the reporting tool. Missing, malformed, or stale reports fail. A submitted report alone cannot establish success, and plugin-side validation cannot replace canonical Mergeyard validation.
- Captured rejected-request evidence establishes Go limits, depleted pay-as-you-go credits, and scheduled budget caps. Unknown failures never acquire a guessed restriction classification.
- Independently available Go/PAYG work remains eligible under existing concurrency rules. Shared credit depletion affects every path actually drawing on that balance; a probe cannot clear another scope's restriction.
- Native Extra Usage is honored without Mergeyard changing billing settings or silently selecting another provider/model.
- Normal CI uses offline fixtures and fake execution. Paid provider calls are not a normal test-suite requirement.

## Implementation sequence

1. Probe a released V2 candidate for the typed reporting tool and canonical report validation, effective model resolution, session persistence, native skills, unattended permissions, server ownership, cancellation, and restart. Capture and sanitize native failure evidence for both funding modes.
2. Extend durable availability scope and targeted recovery while preserving existing single-account harness behavior and legacy state. Include shared balances and overlapping timed/credit restrictions.
3. Implement the V2 adapter, claim-time effective model resolution, registry/configuration/doctor support, and existing lifecycle contracts. Prefer the simplest verified execution route that preserves the agreed guarantees.
4. Expose scope, resolved settings, and recovery targeting through existing plan/status/dashboard/report surfaces. Update setup, configuration, examples, and supported-version documentation.
5. Verify offline adapter and integration cases, run `make lint`, and run only specific new/changed tests locally. After implementation is pushed, use CI and `gh pr checks` for the full Go suite, following AGENTS.md.

The investigation is a prerequisite to implementation choices, not permission to weaken the accepted report, lifecycle, or funding behavior.

## Approved implementation tickets

Each ticket includes its relevant diagnostics and tests. The dependencies below match the published native blocking links; tickets without blockers form the initial frontier.

| Ticket | Delivers | Blocked by |
| --- | --- | --- |
| [#273](https://github.com/rcpassos/mergeyard/issues/273) | Explicit durable recovery targets for existing harnesses | None |
| [#274](https://github.com/rcpassos/mergeyard/issues/274) | Released V2 reporting and execution compatibility evidence | None |
| [#275](https://github.com/rcpassos/mergeyard/issues/275) | Go/PAYG rejection evidence and availability scope identities | None |
| [#276](https://github.com/rcpassos/mergeyard/issues/276) | Explicit-model OpenCode implementation from issue to draft PR | #274 |
| [#277](https://github.com/rcpassos/mergeyard/issues/277) | Frozen defaults, harness overrides, tiers, and variants | #276 |
| [#278](https://github.com/rcpassos/mergeyard/issues/278) | Configured skills on fresh and resumed phases | #276 |
| [#279](https://github.com/rcpassos/mergeyard/issues/279) | Review, bounded fixes, and CI repair across pairings | #276 |
| [#280](https://github.com/rcpassos/mergeyard/issues/280) | Exact-session takeover, handback, and interrupted-run recovery | #276 |
| [#281](https://github.com/rcpassos/mergeyard/issues/281) | Scoped Go quota and scheduled-budget waiting | #275, #276 |
| [#282](https://github.com/rcpassos/mergeyard/issues/282) | Scoped PAYG credit blocks and explicit recovery probes | #273, #281 |
| [#283](https://github.com/rcpassos/mergeyard/issues/283) | Native Extra Usage and shared/overlapping funding restrictions | #282 |
| [#284](https://github.com/rcpassos/mergeyard/issues/284) | Supported onboarding and cross-feature acceptance | #277, #278, #279, #280, #283 |
