# Mergeyard — Product Requirements Document

**Status:** Implementation-ready MVP specification  
**Version:** 0.6  
**Product:** Mergeyard  
**Tagline:** Turn issues into merged code.

---

## 1. Product Summary

Mergeyard is a local-first control plane that turns GitHub issues into reviewed, merge-ready pull requests using existing coding agents.

GitHub is the source of truth for work. Mergeyard continuously discovers eligible issues, claims them, prepares an isolated Git worktree, and runs a fixed two-agent loop:

1. the **implementer** (agent A) implements the issue, and Mergeyard opens a draft pull request;
2. the **reviewer** (agent B) reviews the pull request and reports findings;
3. the implementer fixes the findings and reports what it changed;
4. the reviewer checks again;
5. steps 3–4 repeat until the reviewer approves and CI passes, or the round limit is reached;
6. Mergeyard marks the pull request ready to merge, and **the user merges it manually**.

Mergeyard is not a coding agent, model gateway, IDE, terminal emulator, or remote-development platform. It coordinates existing harnesses: Claude Code and Codex in the MVP.

Mergeyard runs as one local Go process on the user's machine and exposes a browser dashboard on localhost. All work executes on that machine.

The core mental model is:

```text
GitHub      = WHAT work exists
Mergeyard   = WHEN and HOW work is orchestrated
Implementer = WHO writes and fixes the code (agent A)
Reviewer    = WHO checks the code (agent B)
User        = WHO merges
```

A **run** is the complete lifecycle for one GitHub issue, from claim until the pull request is merged or the run is stopped.

---

## 2. Problem

A productive AI-assisted workflow often splits work between two agents: one writes the code, a second, independent one reviews it, and the two iterate until the review is clean.

Without orchestration, the developer repeatedly has to:

1. inspect GitHub issues;
2. determine whether dependencies are resolved;
3. select the next task;
4. create branches/worktrees;
5. start the implementing agent with the issue context, model, effort, and skills;
6. monitor the process;
7. push code and create a pull request;
8. start a separate reviewing agent;
9. relay review findings back to the implementing agent;
10. relay the fix report back to the reviewer;
11. repeat until the review is clean;
12. wait for CI and relay failures;
13. merge and clean up;
14. repeat for the next eligible issue.

Mergeyard automates this coordination, keeps both agents interchangeable, and lets the user inspect or take over at any time. The user keeps the final merge decision.

---

## 3. Goals

The MVP must:

- be simple to set up and use: one repository and two agent choices are enough to start;
- use GitHub issues as the work queue;
- avoid creating a second project-management system;
- run one or more issues concurrently in isolated worktrees on the local machine;
- support Claude Code and Codex for either role;
- keep the implementer and reviewer in independent agent sessions;
- loop implementer fixes and reviewer checks until the pull request is clean, within a bound;
- leave the merge to the user;
- persist runtime state locally;
- survive control-plane restarts without killing running agents;
- handle harness usage limits by waiting instead of failing;
- expose a compact browser dashboard as the primary UI;
- expose a small CLI for setup and operational commands;
- stop safely when state is ambiguous;
- require no cloud Mergeyard service.

---

## 4. Product Principles

### 4.1 Simple by default

A working setup needs only a repository name. Everything else has a documented default. `mergeyard init` and `mergeyard repo add` write the configuration so the user rarely edits YAML by hand.

### 4.2 GitHub owns project truth

GitHub owns:

- issue identity and body;
- open/closed state;
- labels;
- dependency/blocker relationships;
- pull requests and their comments;
- branches visible to collaborators;
- CI/check state;
- merge state.

Mergeyard owns only orchestration/runtime state.

### 4.3 Harnesses do the engineering work

Mergeyard must not recreate Claude Code or Codex. A harness adapter translates a normalized phase request into a harness invocation and translates the result back into Mergeyard's normalized result format.

### 4.4 Two roles, two independent agents

Every run has exactly two roles:

- **Implementer** — implements the issue and fixes review findings and CI failures.
- **Reviewer** — reviews the pull request and checks the implementer's fixes.

Each role keeps its own agent conversation for the whole run. The two roles never share an agent session.

### 4.5 The user merges

Mergeyard never merges pull requests in the MVP. It brings a pull request to a reviewed, CI-green, ready-for-review state and waits for the user to merge it on GitHub.

### 4.6 Skills are first-class but optional

Skills may be attached to a role, such as `code-review-rcp` for the reviewer. The scheduler does not know what a skill means. Harness adapters translate skill names into harness-specific behavior.

### 4.7 Autonomous by default, interruptible by design

Healthy runs progress without user input. Ambiguous or unsafe situations move to `NEEDS_ATTENTION` rather than guessing.

### 4.8 Standard tools before custom infrastructure

The MVP prefers:

- GitHub CLI;
- Git CLI;
- tmux;
- SQLite;
- server-rendered HTML;
- HTMX/SSE.

It does not introduce a worker daemon, message broker, distributed queue, custom RPC protocol, or SPA.

---

## 5. Terminology

| Term | Meaning |
|---|---|
| Repository | A configured GitHub repository watched by Mergeyard. |
| Issue | The GitHub issue that represents one unit of work. |
| Run | One orchestration lifecycle for one issue. |
| Workflow | The fixed MVP lifecycle: implement → review ↔ fix → CI → ready to merge. |
| Role | `implementer` or `reviewer`. Each role has an agent, model, effort, and skills. |
| Agent | The harness type assigned to a role: `claude` or `codex`. |
| Phase | One agent-backed step: `implement`, `review`, or `fix`. |
| Round | One review by the reviewer. Round 1 is the first review. |
| Phase attempt | One execution of a phase. Retries create additional attempts. |
| Agent session | The harness conversation that belongs to one role in one run. Resumed across that role's phases. |
| Process session | The tmux session that hosts one phase attempt's process. |
| Worktree | The isolated Git worktree owned by one active run. |
| Claim | The durable transition that removes the issue from the ready queue and marks it as handled by Mergeyard. |
| Takeover | A paused state in which the user works in the run's worktree and agent session directly. |

---

## 6. Supported Platforms and Assumptions

MVP support:

- macOS;
- Linux.

Windows is not an MVP target because the process model assumes POSIX tooling and tmux.

All runs execute on the machine running Mergeyard. Remote execution is a future feature (section 45).

Required tools:

- `git`;
- `gh`, authenticated;
- `tmux`;
- `claude` and/or `codex`, logged in, for the agents assigned to roles;
- the project's own runtime/build dependencies.

The MVP assumes one logged-in account per harness. Mergeyard does not manage, select, or rotate harness accounts.

Mergeyard validates dependencies but does not install packages, language runtimes, harnesses, or global skills.

---

## 7. GitHub Contract

### 7.1 Repository authorization

Mergeyard only operates on explicitly configured repositories.

### 7.2 Labels

Three labels, all configurable:

```text
ready-for-agent         input: the issue may be picked up
agent-running           output: Mergeyard has claimed the issue
agent-needs-attention   output: a human must act
```

An issue is never dispatched merely because it is open. The ready label is an explicit authorization signal that the issue is sufficiently specified and trusted for agent execution.

Detailed phase state lives in Mergeyard/SQLite, not in labels.

### 7.3 Claim transition

When Mergeyard claims an issue it must, in this order:

1. create the local run record in a `CLAIMING` state;
2. add the `running` label;
3. remove the `ready` label;
4. persist the successful claim;
5. begin workspace preparation.

If GitHub label mutation fails, the run must not proceed.

The MVP supports only one Mergeyard control plane managing a given repository at a time. Cross-instance locking is explicitly unsupported.

### 7.4 Completion transition

After the user merges the pull request:

- the `running` and `needs_attention` labels are removed when present;
- the issue is expected to close through the PR's closing reference, or is closed by Mergeyard if still open;
- the worktree is cleaned up;
- the run becomes `COMPLETED`.

### 7.5 Attention transition

For any situation needing human action, including failures without a usable workspace:

- remove `running`;
- add `agent-needs-attention`;
- preserve the worktree, branch, and diagnostics;
- do not re-add `ready` automatically.

Retry is always an explicit user action after this transition.

---

## 8. Eligibility and Dependencies

An issue is eligible only when all of the following are true:

- it is open;
- it has the ready label;
- it does not have the running or needs-attention labels;
- it has no unresolved GitHub blockers according to the GitHub adapter;
- no active Mergeyard run exists for the same repository + issue;
- the repository is enabled;
- global concurrency has capacity;
- repository concurrency has capacity, when a repository limit is set;
- the implementer's harness is not usage-limited (section 12).

### Dependency source

The MVP reads dependency/blocker relationships from GitHub through the GitHub adapter. It does **not** infer dependencies from free-form issue text such as `blocked by #123`.

A release can be modeled as a normal issue blocked by every issue it requires; no separate release concept exists.

### Deterministic scheduling

Eligible issues are sorted by:

1. repository configuration order;
2. GitHub issue creation time ascending;
3. issue number ascending as the final tie-breaker.

The MVP does not attempt AI-based prioritization.

### Blocked issues

Blocked issues appear in the dashboard but do not create a run until they become eligible.

---

## 9. Scheduler and Concurrency

### Polling

Default polling interval: `30s`. Only one scheduler reconciliation tick may run at a time.

### Concurrency limits

Two limits are enforced:

- global active-run limit (default `1`);
- optional per-repository active-run limit.

An active run consumes one slot from claim through any non-terminal state **except `READY_TO_MERGE`**, including `MANUAL`, `NEEDS_ATTENTION`, and waiting for CI. A run waiting for the user to merge does no agent work and does not block new issues.

The MVP counts **runs**, not processes or phase attempts. It does not schedule based on CPU/RAM usage.

### Pausing the scheduler

Global pause means:

- no new issues are claimed;
- existing runs continue;
- CI monitoring and merge detection continue.

For an individual run the supported controls are takeover, stop, and retry.

---

## 10. Execution and Workspace

All runs execute on the local machine. Internally, execution goes through a small runner interface with a single local implementation, so remote execution can be added later without changing workflow logic. The runner is not user-configurable in the MVP.

```go
type Runner interface {
    Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
    StartSession(ctx context.Context, req SessionRequest) (SessionRef, error)
    SessionStatus(ctx context.Context, ref SessionRef) (SessionStatus, error)
    StopSession(ctx context.Context, ref SessionRef) error
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
    RemovePath(ctx context.Context, path string) error
}
```

### Layout

Mergeyard uses managed checkouts instead of an arbitrary developer working directory. Default workspace: `~/.mergeyard`.

```text
~/.mergeyard/
  state.db
  repos/
    <owner>-<repo>/
      base/
  worktrees/
    <owner>-<repo>/
      <run-id>/
  runs/
    <run-id>/
      phases/
      logs/
```

### Base checkout

On first use, Mergeyard clones the repository into `repos/<owner>-<repo>/base/`.

Before each new run it:

1. verifies the origin matches the configured repository;
2. fetches origin and prunes stale remote refs;
3. verifies the base branch exists;
4. resets the base checkout to the remote base branch when no active worktree conflicts.

The base checkout is never used for agent edits. A dirty base checkout prevents new work for that repository until reconciled.

### Worktree

Each run receives exactly one worktree at `worktrees/<owner>-<repo>/<run-id>`.

### Branch naming

Default: `mergeyard/issue-<number>`.

If that branch already exists remotely:

- if it belongs to an existing/reconcilable Mergeyard run, reuse it;
- otherwise move to `NEEDS_ATTENTION` instead of overwriting it.

### Git authentication

Mergeyard uses the machine's existing Git and `gh` credentials. `mergeyard doctor` verifies read access, base-branch resolution, and push access for every configured repository.

### Cleanup

After the pull request is merged:

- remove the worktree;
- prune worktree metadata;
- delete the local branch;
- never delete the remote branch unless configured.

Stopped, manual, and needs-attention runs preserve their worktrees.

---

## 11. Harness Adapters

The harness adapter describes **how to invoke and interpret a coding harness**. It does not own process execution.

```go
type HarnessAdapter interface {
    Type() string
    Capabilities() HarnessCapabilities
    ValidateConfig(role RoleConfig) error
    BuildInvocation(ctx PhaseContext, role RoleConfig) (Invocation, error)
    ParseResult(ctx PhaseContext, artifacts PhaseArtifacts) (PhaseResult, error)
}
```

```text
Phase (implement / review / fix)
    ↓
Harness adapter -> Invocation (new or resumed agent session)
    ↓
Runner + tmux -> Process
    ↓
Structured output / artifacts
    ↓
Harness adapter -> Normalized PhaseResult
```

MVP adapter types:

- `claude` — Claude Code;
- `codex` — Codex CLI.

Either adapter may be assigned to either role. The executable defaults to `claude` / `codex` on `PATH` and may be overridden in configuration.

### Capabilities

Adapters declare capabilities:

```text
model_selection
effort_selection
skill_selection
structured_output
session_resume
usage_limit_detection
```

`session_resume` is required for MVP adapters. Configuration that requests an unsupported capability, such as an effort level on a harness without effort selection, fails validation before scheduling begins.

### Agent sessions

Each role in a run owns one agent session:

- the **implementer session** starts in `implement` and is resumed for every `fix`;
- the **reviewer session** starts in the first `review` and is resumed for every later review.

Each phase attempt still runs as a new, non-interactive harness process that resumes the role's conversation by its harness session ID. Mergeyard records the session ID for each role.

If a resume fails (session missing, corrupted, or out of context), Mergeyard starts a new agent session for that role, relies on the full phase input file for context, records a `harness.session_resume_failed` warning, and continues.

The implementer and reviewer never share an agent session.

---

## 12. Usage Limits

Subscription-backed harnesses enforce usage limits, such as rolling 5-hour or weekly windows. Hitting one is a temporary capacity condition, not a phase failure, and must not send the run to `NEEDS_ATTENTION` by itself.

### Detection

- When a phase attempt exits unsuccessfully, the adapter classifies whether the cause was a usage limit using the harness's exit code, structured output, or known error messages in the phase log, and extracts the reset time when the harness reports one.
- This classification applies only to failed exits. Phase completion is still determined by exit metadata and the structured result (section 13).
- Detection patterns are adapter-specific and maintained with the adapter. Unrecognized failures follow the normal phase-failure path. An adapter without the `usage_limit_detection` capability treats every failure as a normal phase failure.

### Scope

A limit applies to one harness type (`claude` or `codex`), because the MVP assumes one logged-in account per harness. It does not affect the other harness.

### Handling

When an attempt is classified as usage-limited, Mergeyard:

1. marks the phase attempt `usage_limited`;
2. records the harness as limited until the reported reset time, or until `now + usage_limits.cooldown` (default `30m`) when no reset time is available;
3. moves the run to `WAITING_FOR_HARNESS`, preserving its worktree, branch, PR, and agent sessions;
4. when the limit expires, starts a new attempt of the same phase, resuming the role's agent session.

Usage-limited attempts do not count toward phase attempts or review rounds. The new attempt's input states that the previous attempt was interrupted and that the worktree may contain partial changes. The review read-only rule still applies: an interrupted review's worktree changes are restored.

While a harness is limited:

- runs needing that harness wait in `WAITING_FOR_HARNESS`;
- runs in other phases, such as CI wait or phases using the other harness, continue;
- new issues whose implementer uses that harness are not claimed;
- waiting runs keep their concurrency slot.

### Bound

After `usage_limits.max_waits` (default `3`) consecutive usage-limited attempts for the same phase, the run moves to `NEEDS_ATTENTION` with `harness.usage_limit_waits_exhausted`. This prevents endless waiting when reset times are wrong or detection misfires.

---

## 13. Phase Contracts

### 13.1 Input

Before every phase attempt Mergeyard writes a context file outside the worktree:

```text
~/.mergeyard/runs/<run-id>/phases/<phase>-<round>-<attempt>/input.md
```

The harness prompt references that file. The full input is written even when the agent session is resumed, so a fresh session can always continue from it.

| Phase | Input contains |
|---|---|
| `implement` | repository, issue number/title/body/URL, base branch, worktree path, constraints, output contract |
| `review` | the above plus PR number/URL, the diff range to review, and on later rounds: previous findings and the implementer's fix report |
| `fix` | the above plus the latest blocking review findings and/or failing CI checks with failure excerpts |

Issue, PR, review, and CI text must be written to files or passed through stdin. It must never be interpolated into shell command strings.

Mergeyard does not merge or reinterpret repository instruction files such as `AGENTS.md` or `CLAUDE.md`. The harness consumes them naturally.

### 13.2 Result

Every phase returns a machine-readable result independent of harness prose.

Adapters use the harness's native structured output where available. When it is not, the prompt instructs the agent to write:

```text
~/.mergeyard/runs/<run-id>/phases/<phase>-<round>-<attempt>/result.json
```

Common fields:

```json
{
  "schema_version": 1,
  "status": "success",
  "summary": "Short phase summary"
}
```

#### Implement result

Statuses: `success`, `blocked`, `failed`. `blocked` means the agent cannot safely continue without human input.

#### Review result

```json
{
  "schema_version": 1,
  "status": "changes_required",
  "summary": "Two blocking findings",
  "findings": [
    {
      "id": "R1-F1",
      "severity": "blocking",
      "title": "Missing validation",
      "details": "VAT input accepts an invalid format.",
      "file": "src/checkout.ts",
      "line": 42
    }
  ]
}
```

Statuses: `approved`, `changes_required`, `blocked`, `failed`.

Severities: `blocking`, `warning`, `note`. Only `blocking` findings prevent approval. On later rounds the reviewer returns the complete current list of blocking findings, not only new ones.

#### Fix result

```json
{
  "schema_version": 1,
  "status": "success",
  "summary": "Fixed R1-F1; disputed R1-F2",
  "responses": [
    { "finding_id": "R1-F1", "resolution": "fixed", "note": "Added VAT format check." },
    { "finding_id": "R1-F2", "resolution": "disputed", "note": "Covered by middleware in api/validate.ts." }
  ]
}
```

Statuses: `success`, `blocked`, `failed`. Resolutions: `fixed`, `disputed`. The reviewer decides on disputed findings in the next round.

### 13.3 Missing or invalid result

If the harness exits successfully but the result is missing or invalid, the attempt is failed and the run moves to `NEEDS_ATTENTION` unless a phase retry remains (`max_attempts`, default `1`).

---

## 14. Process Model

The MVP uses **tmux** to host every phase attempt. This allows:

- the control plane to restart without killing agent processes;
- the user to watch a live phase;
- phase logs to remain associated with a stable session.

### Session naming

```text
mergeyard-<short-run-id>-<phase>-<round>-<attempt>
```

Names are sanitized and kept below tmux limits.

### Phase wrapper

Mergeyard starts a generated wrapper script inside tmux. The wrapper:

1. changes to the run worktree;
2. applies only the environment explicitly configured;
3. executes the harness invocation;
4. streams stdout/stderr to the phase log file;
5. records the exit code in phase metadata;
6. exits when the harness exits.

Mergeyard determines completion from exit metadata plus the structured result. It does not infer completion from terminal text.

### Control-plane shutdown

Stopping Mergeyard does not stop running tmux sessions. On restart, Mergeyard reconciles stored sessions against tmux and GitHub state.

---

## 15. Workflow

The MVP is **not** a generic workflow engine. Every run follows one fixed lifecycle:

```text
IMPLEMENT (implementer)
   ↓
COMMIT + PUSH + OPEN DRAFT PR
   ↓
REVIEW round N (reviewer) ──changes_required──▶ FIX (implementer) ──▶ push ──▶ REVIEW round N+1
   ↓ approved
WAIT FOR CI ──failed──▶ FIX (implementer, CI failures) ──▶ push ──▶ REVIEW round N+1
   ↓ passed
READY TO MERGE (PR marked ready for review)
   ↓ user merges on GitHub
CLEANUP → COMPLETED
```

Agent-backed phases: `implement`, `review`, `fix`.

System steps: workspace preparation, Git operations, PR creation/update, PR comments, CI monitoring, merge detection, cleanup.

### Rounds and the bound

- A **round** is one review. `max_rounds` (default `5`) limits reviews per run.
- Every fix is followed by a review, including fixes for CI failures, because the code changed.
- A fix is started only if another review round is available. If the reviewer requests changes, or CI fails, after round `max_rounds`, the run moves to `NEEDS_ATTENTION` (`review.max_rounds_exceeded`).

```text
round 1: changes_required → fix → round 2
round 2: approved → CI failed → fix → round 3
round 3: approved → CI passed → READY_TO_MERGE
```

### Review is read-only

Mergeyard records the worktree Git state before a review. Repository changes made by the reviewer are discarded: Mergeyard restores the pre-review state and records a warning.

### PR conversation

By default (`pr_comments: true`), Mergeyard posts the loop to the pull request so it is visible on GitHub:

- after each review: one comment with the round number, verdict, and findings;
- after each fix: one comment with the implementer's summary and per-finding responses.

Mergeyard posts these comments from the structured results; agents do not need GitHub write access. Findings are always stored locally and shown in the dashboard.

---

## 16. Git and Commit Behavior

Every run starts from the fetched remote base branch.

Harnesses may create commits but are not required to. After each `implement` or `fix`:

- if there are staged/unstaged tracked changes, Mergeyard creates a commit;
- only non-ignored untracked files are included;
- if `implement` produced no diff from the base branch, the run moves to `NEEDS_ATTENTION` (`git.no_changes`);
- if `fix` produced no changes and disputed every finding, the next review proceeds normally.

Default commit message: `mergeyard: #<issue> <issue title>`. The commit uses the machine's Git identity.

Mergeyard never force-pushes. If the remote branch diverges unexpectedly, the run moves to `NEEDS_ATTENTION`.

---

## 17. Pull Request Contract

After a successful `implement`:

1. push the run branch;
2. discover an existing open PR for that branch or create one as a **draft**;
3. persist PR number/URL;
4. start review round 1.

If the repository does not support draft PRs (for example private repositories on GitHub Free), Mergeyard opens a normal PR.

Default title: `#<issue> <issue title>`.

Default body:

- `Closes #<issue>`;
- implementation summary;
- a Mergeyard-generated footer with the run ID.

Mergeyard updates its generated section without overwriting user-written content. If more than one open PR exists for the branch, the run moves to `NEEDS_ATTENTION`.

---

## 18. CI Flow

Mergeyard monitors GitHub checks on the PR head commit after reviewer approval.

Normalized CI states: `pending`, `passed`, `failed`, `unknown`.

- `passed` → `READY_TO_MERGE`.
- `failed` → `fix` with the failed check names, failure excerpts, and URLs, if a review round remains; otherwise `NEEDS_ATTENTION`.
- If no checks are reported for the head commit after 2 minutes, CI is treated as passed.

Mergeyard respects branch protection and required checks. It does not rerun flaky checks automatically.

---

## 19. Ready to Merge

When the reviewer has approved the current head commit and CI has passed, Mergeyard:

1. records the approved commit SHA;
2. marks the draft PR as ready for review;
3. moves the run to `READY_TO_MERGE`, which releases its concurrency slot.

The dashboard shows `Waiting for your merge`.

Mergeyard never merges. While waiting:

- when GitHub reports the PR merged, Mergeyard cleans up and completes the run;
- if the PR head changes after approval (for example the user pushes a commit), the dashboard shows a `changed after approval` warning; Mergeyard does not re-review automatically;
- if the PR is closed without merging, the run moves to `NEEDS_ATTENTION` (`pr.closed_unmerged`).

---

## 20. Takeover

Mergeyard must not embed a terminal in the browser.

Automated phases run non-interactively, so attaching to their tmux session only lets the user watch. Taking control means continuing the agent conversation interactively.

### Watch

`mergeyard watch <run-id>` attaches read-only to the live phase's tmux session. The dashboard shows the equivalent command.

### Take over

`mergeyard takeover <run-id>`:

1. stops the running phase process gracefully, if any;
2. moves the run to `MANUAL`;
3. starts the implementer's harness interactively in the worktree, resuming the implementer's agent session (for example `claude --resume <session-id>` or `codex resume <session-id>`).

The dashboard's `Take over` action performs steps 1–2 and shows a copyable command for step 3.

No automated phase starts while the run is `MANUAL`.

### Hand back

The user selects `Hand back` (or runs `mergeyard handback <run-id>`). Mergeyard then:

1. verifies no Mergeyard-owned phase process is running;
2. inspects the worktree, branch, and PR state;
3. commits and pushes any manual changes;
4. starts the next required step: `implement` if no PR exists yet, otherwise a review round.

The implementer's later fixes resume the same agent session, including the user's manual conversation. The user is responsible for exiting the interactive harness before handing back.

---

## 21. Run State Machine

Run state and phase are separate fields.

### Run states

```text
CLAIMING
PREPARING
ACTIVE
WAITING_FOR_CI
WAITING_FOR_HARNESS
READY_TO_MERGE
MANUAL
NEEDS_ATTENTION
FAILED
STOPPED
COMPLETED
```

`ACTIVE` is paired with a phase: `implement`, `review`, or `fix`.

Terminal states: `FAILED`, `STOPPED`, `COMPLETED`. `NEEDS_ATTENTION` is non-terminal because the user may retry.

### Transitions

| From | Trigger | To |
|---|---|---|
| none | eligible issue claimed | CLAIMING |
| CLAIMING | GitHub claim succeeds | PREPARING |
| PREPARING | worktree ready | ACTIVE/implement |
| ACTIVE/implement | success, PR opened | ACTIVE/review |
| ACTIVE/review | approved | WAITING_FOR_CI |
| ACTIVE/review | changes required, rounds remain | ACTIVE/fix |
| ACTIVE/review | changes required, no rounds remain | NEEDS_ATTENTION |
| ACTIVE/fix | success, pushed | ACTIVE/review |
| WAITING_FOR_CI | CI passes | READY_TO_MERGE |
| WAITING_FOR_CI | CI fails, rounds remain | ACTIVE/fix |
| WAITING_FOR_CI | CI fails, no rounds remain | NEEDS_ATTENTION |
| READY_TO_MERGE | PR merged | COMPLETED |
| READY_TO_MERGE | PR closed unmerged | NEEDS_ATTENTION |
| ACTIVE/any | harness usage limit detected | WAITING_FOR_HARNESS |
| WAITING_FOR_HARNESS | harness limit expires | ACTIVE/same phase (new attempt) |
| WAITING_FOR_HARNESS | usage-limit waits exhausted | NEEDS_ATTENTION |
| any non-terminal | user takes over | MANUAL |
| MANUAL | user hands back | ACTIVE/next phase |
| ACTIVE/any | `blocked`, invalid result, or retries exhausted | NEEDS_ATTENTION |
| NEEDS_ATTENTION | user retries | reconciled next state |
| any non-completed | user stops | STOPPED |
| any | unrecoverable internal failure | FAILED |

Transitions are enforced centrally. UI and CLI actions must not mutate arbitrary states.

---

## 22. Stop, Retry, and Recovery

### Stop

`mergeyard stop <run-id>`:

1. interrupts a running phase process gracefully;
2. waits for a grace period, then kills the tmux session if still active;
3. preserves the worktree, branch, and PR;
4. removes the `running` label;
5. adds `needs-attention` unless no work was ever created;
6. marks the run `STOPPED`.

Stop never deletes code.

### Retry

Retry is allowed from `NEEDS_ATTENTION` and `FAILED`. Retry:

- reuses the same run ID, branch, worktree, and agent sessions;
- creates a new phase attempt;
- does not reset user changes;
- reconciles Git/PR/CI state first and determines the next safe phase;
- grants one additional review round when the run stopped on `review.max_rounds_exceeded`.

### Infrastructure retries

Short-lived infrastructure operations such as GitHub polling may use bounded automatic retries with backoff. Agent failures do not retry indefinitely.

---

## 23. Restart and Reconciliation

At startup, Mergeyard reconciles SQLite, GitHub, Git, and tmux state before dispatching new work.

For every non-terminal run it verifies:

- issue state and labels;
- worktree and branch state;
- PR state;
- tmux process session state;
- CI/merge state when applicable;
- persisted harness limits.

Cases:

- **Process still running:** restore the run as active and keep monitoring.
- **Process finished while offline:** read exit metadata and the result, then continue.
- **Harness limited:** keep the run in `WAITING_FOR_HARNESS` until the limit expires.
- **PR merged while offline:** complete the run and clean up.
- **Orphaned claim:** an issue with `agent-running` but no reconcilable local run is not dispatched. It is shown as an orphaned claim requiring manual action.

The MVP does not promise run reconstruction after SQLite deletion. Labels prevent duplicate dispatch, but local history may be lost.

---

## 24. Runtime State (SQLite)

SQLite is runtime/bookkeeping storage, not project truth.

### `runs`

```text
id
repository
issue_number
state
current_phase
review_round
branch
worktree_path
pr_number
implementer_agent
implementer_session_id
reviewer_agent
reviewer_session_id
approved_sha
created_at
updated_at
completed_at
last_error_code
last_error_message
```

Invariant: at most one non-terminal run per repository + issue.

### `phase_attempts`

```text
id
run_id
phase
role
round
attempt
agent
model
effort
status            -- running | succeeded | failed | usage_limited | stopped
resumed_session   -- bool
process_session
input_path
result_path
log_path
exit_code
started_at
ended_at
error
```

### `harness_limits`

```text
harness_type
limited_until
reset_time_source   -- reported | default_cooldown
detected_at
phase_attempt_id
```

Invariant: at most one row per harness type.

### `events`

```text
id
run_id
type
payload_json
created_at
```

---

## 25. Events and Logging

Core events:

```text
scheduler.paused
scheduler.resumed
harness.usage_limited
harness.available
run.claimed
run.preparing
run.manual
run.handed_back
run.needs_attention
run.failed
run.stopped
run.completed
phase.started
phase.output
phase.completed
phase.failed
pr.created
pr.ready_for_review
pr.merged
review.completed
fix.completed
ci.updated
```

Each event is written to SQLite, the structured application log, and the browser via SSE.

Full phase output is written to `~/.mergeyard/runs/<run-id>/logs/<phase>-<round>-<attempt>.log`. The dashboard shows a bounded tail.

Mergeyard must not intentionally log environment-variable values or credentials. It does not provide a complete secret-redaction engine for harness output.

---

## 26. Configuration

### Source of truth

YAML is the configuration source of truth. The CLI writes it for common changes (`init`, `repo add`). The browser Settings page is read-only.

Search order:

1. `--config <path>`;
2. `./mergeyard.yaml`;
3. `~/.config/mergeyard/config.yaml`.

### Precedence

```text
built-in defaults
  ↓
global config
  ↓
repository config
```

Issue-level overrides are not supported. Configuration is validated at startup; invalid configuration prevents the scheduler from starting. Changes require a restart and apply to newly claimed runs.

### Minimal configuration

```yaml
repositories:
  - repo: rcpassos/mergeyard
```

With no other settings, both roles use `claude`, the base branch is the repository's default branch, and global concurrency is `1`. `mergeyard init` asks which agent to use for each role.

### Typical configuration

```yaml
implementer:
  agent: codex
  effort: medium

reviewer:
  agent: claude
  model: opus
  effort: high
  skills: [code-review-rcp]

repositories:
  - repo: rcpassos/mergeyard
```

### Full configuration with defaults

```yaml
version: 1

port: 7331
open_browser: true
workspace: ~/.mergeyard

poll_interval: 30s
concurrency: 1            # global active runs

labels:
  ready: ready-for-agent
  running: agent-running
  needs_attention: agent-needs-attention

agents:                   # optional executable overrides
  claude: { executable: claude }
  codex:  { executable: codex }

implementer:
  agent: claude
  model: null             # harness default
  effort: null            # harness default
  skills: []
  max_attempts: 1

reviewer:
  agent: claude
  model: null
  effort: null
  skills: []
  max_attempts: 1

max_rounds: 5
pr_comments: true

usage_limits:
  cooldown: 30m
  max_waits: 3

repositories:
  - repo: company/api
    base_branch: main     # default: repository default branch
    concurrency: 1        # optional per-repository limit
    enabled: true

  - repo: company/frontend
    reviewer:             # per-repository role override
      agent: codex
      effort: high
```

A repository-level `implementer` or `reviewer` block overrides only the fields it sets.

Model identifiers are passed through to the harness. Mergeyard does not maintain a model catalog.

---

## 27. Web Application

Default address: `http://127.0.0.1:7331`. The dashboard is the primary interface.

### Dashboard

- scheduler running/paused state;
- concurrency usage;
- harness availability, including usage-limited harnesses and their reset time;
- running runs with phase and round;
- ready, blocked, and needs-attention issues;
- PRs waiting for the user's merge;
- recent completed runs.

### Queue

```text
Running
Waiting for harness
Ready
Blocked
Needs attention
Waiting for your merge
```

### Repository page

Repository, base branch, role configuration, concurrency, counts, active runs, eligible queue, unresolved blockers, sync/health errors.

### Run detail

- issue link, title, body summary;
- run ID, state, phase, round, attempt;
- worktree path and branch;
- implementer and reviewer: agent, model, effort, skills, session ID;
- PR link;
- review rounds: findings, implementer responses, verdicts;
- CI state;
- timeline/events;
- bounded log tail;
- last error with error code.

State-dependent actions:

```text
Watch (copy command)
Take over
Hand back
Stop
Retry
Open issue
Open PR
Copy worktree path
```

### Settings/Diagnostics

Read-only: effective config path and values, doctor results, binary versions, workspace path.

---

## 28. Web Interaction Model

- server-rendered Go templates;
- HTMX for actions and partial refreshes;
- SSE for runtime updates;
- Tailwind + daisyUI;
- Lucide SVG icons.

Routes:

```text
GET  /
GET  /queue
GET  /repositories/{owner}/{repo}
GET  /runs/{id}
GET  /settings
GET  /events

POST /scheduler/pause
POST /scheduler/resume
POST /runs/{id}/takeover
POST /runs/{id}/handback
POST /runs/{id}/stop
POST /runs/{id}/retry
```

State-changing requests require CSRF protection and loopback-origin checks.

---

## 29. CLI

The CLI handles setup and operations. It is not a TUI.

```text
mergeyard                     start (same as `mergeyard start`)
mergeyard init                interactive setup: writes config, checks tools
mergeyard repo add <owner/repo>
mergeyard status
mergeyard doctor
mergeyard open
mergeyard pause
mergeyard resume
mergeyard watch <run-id>
mergeyard takeover <run-id>
mergeyard handback <run-id>
mergeyard stop <run-id>
mergeyard retry <run-id>
mergeyard reconcile
```

### `mergeyard init`

1. detects `git`, `gh`, `tmux`, `claude`, `codex`;
2. asks which agent to use as implementer and reviewer, offering only installed ones;
3. asks for the first repository;
4. creates the GitHub labels in that repository if missing, after confirmation;
5. writes the config file and runs `doctor`.

### `mergeyard start`

1. locate and validate config;
2. acquire the single-instance lock;
3. open/migrate SQLite;
4. validate dependencies;
5. reconcile existing runs;
6. start HTTP/SSE server and scheduler;
7. optionally open the browser.

### `mergeyard reconcile`

Re-runs reconciliation and reports orphaned claims, worktrees, and sessions. It does not silently reset orphaned claims.

---

## 30. Doctor

`doctor` checks:

### Machine

- configuration parses and validates;
- workspace writable;
- dashboard port available;
- `git`, `tmux` present;
- `gh` present and authenticated;
- every harness assigned to a role is present and logged in;
- requested capabilities (model, effort, skills) are supported by each assigned harness.

### Per repository

- origin accessible;
- base branch exists;
- fetch works;
- push permission available;
- labels exist;
- reviewer/implementer skills available, where the adapter can verify it.

Results are grouped as `error`, `warning`, or `unverifiable`.

---

## 31. Startup, Shutdown, and Single Instance

Only one Mergeyard process may own a workspace at a time, enforced by a lock file.

On SIGINT/SIGTERM:

1. stop claiming new issues;
2. stop scheduler ticks;
3. persist pending state;
4. leave running tmux sessions alive;
5. close HTTP/SSE and SQLite cleanly.

The next startup performs reconciliation.

---

## 32. Security and Trust Model

### Local web boundary

The MVP binds only to loopback addresses. Remote dashboard exposure is unsupported.

### Issue trust boundary

Agents run with the user's local permissions. An issue body is effectively executable intent once it receives the ready label. The ready label is a trusted-maintainer authorization boundary.

Mergeyard does not sandbox agent code or protect the machine from malicious instructions in an approved issue.

### Provider secrets

Mergeyard relies on harness-native login and existing Git/GitHub authentication. It stores no model-provider credentials.

### Shell safety

Untrusted issue text, PR text, review findings, and CI output are never interpolated into shell commands. Dynamic text is passed through files or stdin. Command arguments are escaped by the runner.

### Browser actions

State mutations require CSRF tokens and origin validation.

---

## 33. Error Taxonomy

Errors have stable codes for UI, CLI, and tests.

Categories:

```text
config.*
github.*
workspace.*
git.*
harness.*
phase.*
review.*
ci.*
pr.*
reconcile.*
internal.*
```

Examples:

```text
git.no_changes
git.branch_conflict
git.push_rejected
harness.not_logged_in
harness.session_resume_failed
harness.usage_limited
harness.usage_limit_waits_exhausted
phase.result_missing
phase.result_invalid
phase.blocked
review.max_rounds_exceeded
pr.multiple_open
pr.closed_unmerged
reconcile.orphaned_claim
```

Every `NEEDS_ATTENTION`/`FAILED` run exposes a human-readable message and a stable error code.

---

## 34. Reliability Invariants

Mergeyard must enforce:

- no unbounded loops: review rounds, phase attempts, and usage-limit waits are bounded;
- no duplicate active run for one repository + issue;
- no shared agent session between implementer and reviewer;
- no reviewer changes accepted into the branch;
- no automated progression while a run is `MANUAL`;
- no merge by Mergeyard;
- no `READY_TO_MERGE` without reviewer approval of the head commit and passing CI;
- no dispatch for unresolved blockers;
- no new claim when the implementer's harness is usage-limited;
- no force-push;
- no automatic deletion of a non-completed run's worktree;
- no automatic reset of user commits or branch divergence;
- no shell interpolation of issue/PR/review/CI text;
- no silent reset of orphaned claims.

When an invariant cannot be proven, prefer `NEEDS_ATTENTION`.

---

## 35. Technology Stack

### Backend

- Go;
- `net/http` / `http.ServeMux`;
- `html/template`;
- `os/exec`;
- YAML configuration.

### Frontend

- server-rendered HTML;
- HTMX;
- Tailwind CSS + daisyUI;
- Lucide SVG icons;
- SSE.

### Storage

- SQLite through `database/sql`;
- no ORM;
- a pure-Go SQLite driver.

### External tools

- `gh` for GitHub operations;
- `git` for repository/worktree operations;
- `tmux` for process sessions.

### Distribution

`go:embed` for templates/static assets. Target experience:

```text
brew install mergeyard
mergeyard init
mergeyard
```

No Node runtime is required for end users. Node may be used at build time for Tailwind/daisyUI.

---

## 36. Project Structure

```text
mergeyard/
├── cmd/mergeyard/main.go
├── internal/
│   ├── app/              # startup/shutdown, single-instance lock
│   ├── config/           # load, defaults, validate, write (init/repo add)
│   ├── scheduler/        # discovery, eligibility, concurrency, claims
│   ├── workflow/         # fixed lifecycle, rounds, state transitions
│   ├── github/           # issues, blockers, labels, PRs, comments, CI, merge detection
│   ├── git/              # managed clone, branch, worktree, commits
│   ├── workspace/        # managed paths/artifacts
│   ├── harness/
│   │   ├── harness.go
│   │   ├── claude.go
│   │   └── codex.go
│   ├── runner/
│   │   ├── runner.go
│   │   └── local.go
│   ├── sessions/tmux.go
│   ├── store/
│   ├── events/
│   └── web/
├── web/
│   ├── templates/
│   └── static/
├── migrations/
├── mergeyard.example.yaml
├── go.mod
└── go.sum
```

---

## 37. Pre-Implementation Spikes

These facts about the harnesses must be verified against current versions before the adapters are built. Record findings in `docs/`.

1. **Headless resume:** how to resume a session non-interactively with a new prompt (`claude -p --resume <id>`, `codex exec resume <id>`), and how to obtain the session ID from a new session.
2. **Structured output:** native JSON / JSON-schema output options for each harness in non-interactive mode.
3. **Usage limits:** exit codes, messages, and reset-time format when a limit is hit, in non-interactive mode.
4. **Skills:** how each harness invokes a named skill non-interactively.
5. **Model and effort:** flags for model and reasoning effort in each harness.
6. **Interactive resume:** the command that reopens a headless session interactively for takeover.

---

## 38. Delivery Plan

### M1 — Issue to draft PR

- `init`, `repo add`, config loading/validation with defaults;
- single-instance lock;
- GitHub polling, ready-label eligibility, blockers;
- claim labels;
- managed checkout and worktree;
- tmux process sessions and phase wrapper;
- one harness adapter for the implementer, with structured output and session ID capture;
- commit/push and draft PR creation;
- SQLite state and restart reconciliation;
- basic dashboard and run detail;
- `doctor`, `status`, `watch`, `stop`.

Stops after the draft PR is opened.

### M2 — Review loop to ready-to-merge

- second harness adapter;
- reviewer role, review result, read-only enforcement;
- fix phase with per-finding responses;
- agent session resume for both roles, with fallback to a fresh session;
- bounded rounds;
- PR comments;
- CI monitoring with CI failures fed into fixes;
- draft → ready for review, merge detection, cleanup;
- needs-attention flows and retry.

### M3 — Hardening

- multiple repositories with global and per-repository concurrency;
- usage-limit detection, waiting, and resumption;
- takeover and hand back;
- orphaned-claim detection and reconciliation edge cases;
- effective-config diagnostics.

---

## 39. Acceptance Criteria

The MVP is not accepted merely because the dashboard renders. The following behaviors must pass repeatably.

### Setup

From a machine with `git`, `gh`, `tmux`, and one harness installed and logged in, `mergeyard init` followed by `mergeyard` produces a running dashboard watching one repository, with no hand-edited YAML.

### Dispatch

Given an open issue with `ready-for-agent`, no blockers, and available capacity:

- exactly one run is created and the issue is claimed;
- one worktree is created from the remote base branch;
- the implementer starts in tmux;
- the control plane may restart without killing that process;
- code is committed and pushed;
- exactly one draft PR is created or discovered.

### Duplicate prevention

Repeated scheduler ticks and restarts do not create a second active run, branch, worktree, or PR for the same issue.

### Review loop

- The reviewer runs in a separate agent session from the implementer and cannot change the branch.
- Changes required → the implementer fixes in its resumed session and reports per-finding responses → the reviewer re-checks in its resumed session.
- The loop ends on approval plus passing CI, or in `NEEDS_ATTENTION` after `max_rounds`.
- With `pr_comments: true`, each review and fix appears as a PR comment.
- When a session cannot be resumed, the phase continues in a fresh session with a recorded warning.

### CI

A CI failure after approval triggers a fix and another review round when rounds remain; otherwise `NEEDS_ATTENTION`.

### Ready to merge

After approval and passing CI, the PR is marked ready for review, the run stops consuming a concurrency slot, and Mergeyard does not merge. When the user merges, the run completes, labels are removed, and the worktree is cleaned up.

### Usage limits

When a harness reports a usage limit during a phase:

- the run moves to `WAITING_FOR_HARNESS`, not `NEEDS_ATTENTION`;
- no new issue whose implementer uses that harness is claimed until the limit expires;
- after the reported reset time, or the default cooldown when none is reported, the same phase restarts without consuming an attempt or round;
- after `max_waits` consecutive limits for one phase, the run moves to `NEEDS_ATTENTION`;
- a restart while limited keeps the run waiting and resumes it on schedule.

### Takeover

`takeover` stops automation, moves the run to `MANUAL`, and opens the implementer's session interactively in the worktree. Mergeyard does not progress the run until `handback`.

### Recovery

A restart during implement, review, fix, CI wait, harness wait, or ready-to-merge reconstructs the correct state from SQLite + tmux + GitHub without duplicating work.

---

## 40. Testing Strategy

### Unit tests

- config defaults, precedence, validation, and role overrides;
- eligibility rules and scheduler ordering;
- concurrency accounting, including the `READY_TO_MERGE` slot release;
- state-transition guards;
- round counting and the CI-failure path;
- usage-limit wait counting;
- path/branch/session naming;
- error-code mapping;
- result-schema validation.

### Harness adapter contract tests

Fake executables simulate:

- successful result;
- missing result;
- invalid JSON;
- non-zero exit;
- long-running process;
- output streaming;
- session resume success and failure;
- usage-limit failure with and without a reported reset time.

Normal CI must not require paid model calls.

### Integration tests

Real Git repository, worktrees, tmux, wrapper scripts, and process restart/reconciliation.

### GitHub integration tests

A dedicated test repository for labels, issues, draft PRs, comments, checks, and merge detection. Live tests must not run against arbitrary configured repositories.

---

## 41. Explicit Non-Goals for the MVP

Do not build:

- a coding model or model proxy;
- a new issue tracker;
- configurable workflows or workflow DAGs;
- automatic merging;
- automatic issue decomposition/planning;
- remote/SSH execution;
- adapters beyond Claude Code and Codex;
- multiple harness accounts per harness;
- a desktop application, TUI, embedded terminal, or IDE;
- a React/Vue/Svelte SPA;
- WebSockets unless SSE proves insufficient;
- browser-based config editing;
- machine provisioning;
- global-skill synchronization;
- multi-user accounts/RBAC;
- GitLab/Linear support;
- locking across multiple Mergeyard instances;
- force-push or rebase of unexpected divergence.

---

## 42. Future Opportunities

After the core loop is reliable:

- remote SSH runners, so runs continue while the laptop is closed;
- OpenCode and other harness adapters;
- optional auto-merge;
- multiple harness accounts with usage-limit-aware rotation;
- configurable workflows and planning/spec phases;
- GitHub Projects integration and webhooks;
- issue creation/decomposition;
- GitLab/Linear adapters;
- notifications;
- token/cost analytics;
- browser-based config editing;
- team/multi-user mode.

---

## 43. Architecture

```text
                              GitHub
        issues / blockers / labels / PRs / comments / CI / merges
                                 │
                                 │ gh
                                 ▼
                ┌─────────────────────────────────┐
                │      Mergeyard (local Go)       │
                │                                 │
                │ Config + validation             │
                │ Scheduler + claims              │
                │ Workflow: implement/review/fix  │
                │ GitHub adapter                  │
                │ Harness adapters                │
                │ Workspace/worktree manager      │
                │ tmux process sessions           │
                │ SQLite + events                 │
                │ HTTP + SSE                      │
                └───────────────┬─────────────────┘
                                │
                     managed repo / worktrees
                                │
                              tmux
                                │
                  ┌─────────────┴─────────────┐
                  ▼                           ▼
          Implementer session         Reviewer session
          (claude or codex)           (claude or codex)


             Browser dashboard: http://127.0.0.1:7331
             Observe / take over / hand back / stop / retry
```

---

## 44. Decision Summary

These decisions are locked for the MVP and must not be reinterpreted without changing this PRD:

1. GitHub is the source of truth for work; SQLite is runtime state only.
2. The browser dashboard is the primary UI; the CLI handles setup and operations; no TUI.
3. Mergeyard runs as one local process and executes all runs on the local machine. Remote execution is a future feature behind an internal runner interface.
4. Claude Code and Codex are the only MVP harnesses; either can fill either role.
5. Every run has two roles: implementer (implements and fixes) and reviewer (reviews and re-checks).
6. Each role keeps one agent session per run, resumed across its phases; implementer and reviewer never share a session. A failed resume falls back to a fresh session.
7. Every phase attempt runs as a new non-interactive process inside tmux.
8. Every phase returns a normalized structured result, from native structured output when available, otherwise `result.json`.
9. The workflow is fixed: implement → draft PR → review ↔ fix → CI → ready to merge.
10. Every fix, including CI fixes, is followed by a review. Reviews per run are bounded by `max_rounds`.
11. Review and fix reports are posted as PR comments by default.
12. Mergeyard never merges. It marks the PR ready for review and waits for the user.
13. `READY_TO_MERGE` runs do not consume a concurrency slot.
14. Configuration is YAML with defaults; `init` and `repo add` write it; a repository name is the minimum configuration.
15. Concurrency is global plus optional per-repository.
16. Three labels: `ready-for-agent`, `agent-running`, `agent-needs-attention`.
17. One logged-in account per harness; Mergeyard does not manage harness accounts.
18. A harness usage limit pauses that harness until reset and requeues the interrupted phase without consuming attempts or rounds; waiting is bounded.
19. Takeover resumes the implementer's agent session interactively in the worktree; hand back resumes automation.
20. Unexpected Git/PR/session state moves to `NEEDS_ATTENTION` instead of being guessed or overwritten.
21. Stopped, manual, and needs-attention worktrees are preserved.
22. Mergeyard never bypasses branch protection or required checks.
23. One Mergeyard instance per repository.
24. The ready label is the authorization boundary for executing issue instructions with the user's permissions.
