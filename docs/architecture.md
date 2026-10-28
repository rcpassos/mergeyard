# Architecture and contributor guide

Mergeyard runs as one local Go process. It discovers approved GitHub issues,
prepares isolated worktrees, and coordinates implementation, independent review,
fixes, and CI. The user merges the resulting pull request.

## Documentation source of truth

The operational `docs/*.md` pages linked below describe current behavior. The
[ADRs](adr/) record design decisions; [CONTEXT.md](../CONTEXT.md) defines domain
terms. Keep the relevant behavior page and ADRs current when changing the
implementation.

[PRD.md](PRD.md) is a frozen historical specification, including the original
milestones. It does not override the current behavior docs. Research reports
under `docs/research/` and the [recovery acceptance record](m3-acceptance.md)
preserve evidence for particular versions and scenarios. Proposed work such as
the [OpenCode design](opencode-design.md) states its implementation status;
an agreed design does not mean the feature has shipped.

## Component boundaries

| Component | Responsibility | Details |
| --- | --- | --- |
| `cmd/mergeyard`, `internal/app` | Load configuration, lock the workspace, open SQLite, and wire the shared runtime | [CLI operations](cli.md), [configuration](configuration.md) |
| `internal/scheduler` | Decide what can run, coordinate external effects, observe attempts, and reconcile interrupted work | [Scheduler](scheduler.md) |
| `internal/workflow`, `internal/events`, `internal/store` | Validate lifecycle transitions and persist run state and events atomically in SQLite | [Run lifecycle and events](workflow.md) |
| `internal/git`, `internal/workspace` | Own managed bases, run worktrees, branches, and local artifacts; publish and clean up work safely | [Managed Git operations](git.md) |
| `internal/github` | Read issues, native dependencies, PRs, and checks; update labels and publish reports through `gh` | [Scheduler](scheduler.md), [GitHub fixtures](../internal/github/testdata/README.md) |
| `internal/harness`, `internal/runner` | Translate role settings and native results; launch and observe attempts through tmux | [Harness execution](scheduler.md), [usage limits](usage-limits.md) |
| `internal/web`, `web/` | Serve the local dashboard, live events, and run controls | [Dashboard and HTTP API](web.md) |

GitHub owns the work queue, issue dependencies, PRs, and CI results. SQLite owns
runtime state and durable journals used to resume interrupted operations. The
workspace retains managed repositories, phase inputs, logs, and reports. Agent
processes run in tmux and can outlive a control-plane restart.

Lifecycle writes go through the shared `Workflow.Transition`; CLI and dashboard
controls use the same scheduler operations as automation. The scheduler
reconciles persisted work before claiming more issues. Git and GitHub adapters
bound external calls with deadlines and preserve work when ownership or remote
state is uncertain.

## Following a run

1. The scheduler checks the issue's ready label, trust, native blockers,
   capacity, and implementer availability. It resolves both roles' settings at
   claim and preserves them for the run ([ADR 0001](adr/0001-role-settings-resolved-at-claim.md)).
2. Managed Git prepares a worktree. The implementer works in its own
   conversation; Mergeyard validates the report, commits and pushes the work,
   and creates or discovers the draft PR.
3. An independent reviewer checks the published head. Blocking findings return
   to the original implementer for fixes and another review, within the
   configured round limit.
4. Approval for the current head leads to CI observation. Repairable failures
   return through fixes and independent review. Matching approval and passing
   checks allow Mergeyard to mark the PR ready for the user's merge.
5. Merge observation completes the run and journals cleanup separately. Stop,
   Retry, takeover, and handback reconcile existing work and preserve its
   history. Supported usage limits pause affected work until reset or cooldown.

The [scheduler reference](scheduler.md) owns orchestration and recovery rules.
The [workflow reference](workflow.md) owns transition, metadata, and event
contracts. The [CLI reference](cli.md) owns command behavior, and the
[dashboard reference](web.md) owns browser and HTTP behavior. Link to these
contracts when extending another surface rather than copying their rules.

## Working on the project

- [Development](development.md): build tools, focused local checks, and CI.
- [Setup](setup.md) and [doctor](doctor.md): configuration and dependency checks.
- [Configuration](configuration.md): defaults, overrides, role settings, and budgets.
- [Usage limits and credits](usage-limits.md): supported signals and recovery controls.
- [Releases](releases.md): publishing and release checks.
- [Website](website.md): the separate public site in `site/`.
