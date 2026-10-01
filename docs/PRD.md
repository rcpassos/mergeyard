# Mergeyard — Product Requirements Document

**Status:** Implementation-ready MVP specification  
**Version:** 0.5  
**Product:** Mergeyard  
**Tagline:** Turn issues into merged code.

---

## 1. Product Summary

Mergeyard is a local-first control plane for orchestrating software-engineering agents across local and remote development environments.

GitHub is the source of truth for work. Mergeyard continuously discovers eligible issues, claims them, prepares an isolated Git worktree on a configured runner, invokes the configured coding harness for each workflow phase, creates and reviews pull requests, waits for CI, applies bounded fix loops, and optionally squash-merges successful work.

Mergeyard is not a coding agent, model gateway, IDE, terminal emulator, or remote-development platform. It coordinates existing harnesses such as Claude Code, Codex, and OpenCode.

The control plane runs as one local Go process and exposes a browser dashboard on localhost. Mergeyard supports three execution modes:

- **Local-only:** all runs execute on the control-plane machine; no remote server or SSH runner is required.
- **Remote-only:** all runs execute on explicitly configured SSH runners; the local machine hosts the control plane and dashboard.
- **Hybrid:** some repositories execute locally and others on configured SSH runners, coordinated by the same control plane.

The built-in `local` runner exists automatically and is the implicit default when neither a repository runner nor a global `default_runner` is specified. Remote SSH runners are optional and provider-agnostic: they may be any pre-provisioned Linux machine reachable over SSH, without a cloud-provider account or integration.

The core mental model is:

```text
GitHub    = WHAT work exists
Mergeyard = WHEN and HOW work is orchestrated
Harness   = WHO performs an agent-backed phase
Runner    = WHERE the entire run executes
```

A **run** is the complete lifecycle for one GitHub issue. A run is assigned to one runner when claimed and remains on that runner until it reaches a terminal state. Harnesses may differ by phase; the runner does not change mid-run in the MVP.

---

## 2. Problem

A modern AI-assisted development workflow commonly uses different models and harnesses for different stages:

- planning/specification with a strong reasoning model;
- implementation with a cheaper or faster model;
- review with a stronger model or higher effort;
- CI repair with an implementation-oriented model;
- manual intervention when an agent needs guidance.

Without orchestration, the developer repeatedly has to:

1. inspect GitHub issues;
2. determine whether dependencies are resolved;
3. select the next task;
4. choose an execution machine;
5. create branches/worktrees;
6. start the correct harness;
7. provide issue context, prompt, model, effort, and skills;
8. monitor the process;
9. push code and create a pull request;
10. start an independent review session;
11. apply review fixes;
12. wait for CI;
13. repair CI failures;
14. merge and clean up;
15. repeat for the next eligible issue.

Mergeyard exists to automate this coordination while keeping the underlying tools interchangeable and allowing the user to inspect or take control at any time.

---

## 3. Goals

The MVP must:

- use GitHub issues as the work queue;
- avoid creating a second project-management system;
- run one or more issues concurrently in isolated worktrees;
- support local-only, remote-only, and hybrid execution with a built-in local default and optional SSH runners;
- support multiple harnesses through adapters;
- allow different harness/model/effort/skills by phase;
- keep implementation and review in independent harness sessions;
- persist runtime state locally;
- survive control-plane restarts without losing remote running processes;
- expose a compact browser dashboard as the primary UI;
- expose a small CLI for operational commands;
- make every automated loop bounded;
- stop safely when state is ambiguous;
- require no cloud Mergeyard service.

---

## 4. Product Principles

### 4.1 GitHub owns project truth

GitHub owns:

- issue identity and body;
- open/closed state;
- labels;
- dependency/blocker relationships;
- pull requests;
- branches visible to collaborators;
- CI/check state;
- merge state.

Mergeyard owns only orchestration/runtime state.

### 4.2 Harnesses do the engineering work

Mergeyard must not recreate Claude Code, Codex, OpenCode, or future harnesses. A harness adapter translates a normalized phase request into a harness invocation and translates the phase result back into Mergeyard's normalized result format.

### 4.3 Runners own execution location

A runner is the machine on which the repository, worktree, harness, build tools, tests, containers, and phase processes execute.

Initial runner types:

- `local` — the machine running Mergeyard;
- `ssh` — an optional, pre-provisioned remote Linux machine reachable through system SSH.

The runner named `local` is built in and requires no `runners.local` declaration. Configuring SSH runners does not change the default by itself. Execution mode follows repository routing; there is no separate mode switch. A remote-only configuration routes every repository to SSH runners, while the built-in local runner remains available without receiving work.

### 4.4 A run never migrates between runners in the MVP

Once issue `#142` is claimed on runner `devbox`, every phase for that run executes on `devbox` and uses the same managed repository/worktree.

This avoids source synchronization, environment drift, and cross-machine session ownership. The initially resolved runner is persisted when the run is claimed and is retained through implementation, review, fixes, CI wait, manual handoff, retries, restart/reconciliation, and cleanup. Configuration changes affect only newly claimed runs; they do not reroute an existing run. If its runner is unavailable, report the problem and wait for recovery; if it is no longer configured, require intervention. Neither case permits falling back to another runner.

### 4.5 Configuration is explicit

Behavior that materially changes execution must be visible in effective-configuration diagnostics. The documented built-in local default provides a usable starting point; selecting remote execution requires an explicit global or repository runner setting.

### 4.6 Skills are first-class but optional

Skills may be attached to phases, but the scheduler does not know the semantics of a skill system. Harness adapters translate logical skill names into harness-specific behavior.

### 4.7 Autonomous by default, interruptible by design

Healthy runs progress without user input. Ambiguous or unsafe situations move to `NEEDS_ATTENTION` rather than guessing.

### 4.8 Standard tools before custom infrastructure

The MVP prefers:

- GitHub CLI;
- Git CLI;
- system SSH;
- tmux;
- SQLite;
- server-rendered HTML;
- HTMX/SSE.

It does not introduce a worker daemon, message broker, distributed queue, custom RPC protocol, or SPA unless the standard tools become a demonstrated limitation.

---

## 5. Terminology

| Term | Meaning |
|---|---|
| Repository | A configured GitHub repository watched by Mergeyard. |
| Issue | The GitHub issue that represents one unit of work. |
| Run | One orchestration lifecycle for one issue. |
| Workflow | A named execution profile describing enabled phases and their configuration. |
| Phase | One step in the fixed MVP lifecycle, such as implementation or review. |
| Phase attempt | One execution of an agent-backed phase. Retries create additional attempts. |
| Runner | The machine/environment where a run executes. |
| Harness | The coding-agent CLI/application used for an agent-backed phase. |
| Harness profile | A named configuration for a harness adapter/executable. |
| Session | A persistent tmux session containing one phase attempt. |
| Worktree | The isolated Git worktree owned by one active run. |
| Claim | The durable transition that removes the issue from the ready queue and marks it as being handled by Mergeyard. |
| Manual handoff | A paused orchestration state in which the user takes control of the run's worktree/session. |

---

## 6. Supported Platforms and Assumptions

### Control plane

MVP support:

- macOS;
- Linux.

Windows is not an MVP target because the session and shell model assumes POSIX tooling and tmux.

### Local runner

The built-in local runner runs on the control-plane machine and requires the same POSIX tool assumptions when used. Local-only operation is supported on both macOS and Linux without a remote server.

### SSH runner

The optional SSH runner targets Linux only in the MVP. Remote-only operation does not require local harness CLIs, local tmux, or a local execution workspace; control-plane dependencies still apply.

### Required local tools

At minimum:

- `git`;
- `gh`;
- `ssh` when SSH runners are configured;
- `tmux` for the local runner when it is used;
- the configured harness CLIs for local execution.

### Required remote tools

Only when SSH runners are used, each remote execution machine requires at minimum:

- POSIX shell;
- `git`;
- `tmux`;
- every harness used by repositories assigned to that runner;
- the project's own runtime/build dependencies.

Remote machines are pre-provisioned by the user. Mergeyard validates dependencies but does not install operating-system packages, language runtimes, Docker, harnesses, or global skills in the MVP.

---

## 7. GitHub Contract

### 7.1 Repository authorization

Mergeyard only operates on explicitly configured repositories.

### 7.2 Ready label

Default input label:

```text
ready-for-agent
```

An issue is never dispatched merely because it is open. The ready label is an explicit authorization signal that the issue is sufficiently specified and trusted for agent execution.

### 7.3 Claim labels

Default output labels:

```text
agent-running
agent-needs-attention
agent-failed
```

Phase-specific GitHub labels are not required for the MVP; detailed phase state lives in Mergeyard/SQLite.

### 7.4 Claim transition

When Mergeyard successfully claims an issue it must, in this order:

1. create the local run record in a `CLAIMING` state;
2. add the configured `running` label;
3. remove the configured `ready` label;
4. persist the successful claim;
5. begin workspace preparation.

If GitHub label mutation fails, the run must not proceed.

The MVP supports only one Mergeyard control plane managing a given repository at a time. Cross-instance distributed locking is explicitly unsupported.

### 7.5 Completion transition

After successful merge:

- the `running`, `needs_attention`, and `failed` labels are removed when present;
- the issue is expected to close through the PR's closing reference or is explicitly closed by Mergeyard if still open;
- the run becomes `COMPLETED`.

### 7.6 Failure transition

For recoverable/user-action failures:

- remove `running`;
- add `agent-needs-attention`;
- preserve the worktree and branch;
- do not re-add `ready` automatically.

For terminal infrastructure/configuration failures where no useful workspace exists:

- remove `running`;
- add `agent-failed`;
- preserve diagnostics.

Retry is always an explicit user action after these states.

---

## 8. Eligibility and Dependencies

An issue is eligible only when all of the following are true:

- it is open;
- it has the configured ready label;
- it does not have the configured running/needs-attention/failed labels;
- it has no unresolved GitHub blockers according to the GitHub adapter;
- no active Mergeyard run exists for the same repository + issue;
- the repository is enabled;
- global concurrency has capacity;
- repository concurrency has capacity;
- the assigned runner has configured concurrency capacity and is healthy.

### Dependency source

The MVP reads dependency/blocker relationships from GitHub through the GitHub adapter. It does **not** infer dependencies from free-form issue text such as `blocked by #123` unless GitHub itself exposes that relationship through the adapter.

### Deterministic scheduling

Eligible issues are sorted by:

1. repository configuration order;
2. GitHub issue creation time ascending;
3. issue number ascending as the final tie-breaker.

The MVP does not attempt AI-based prioritization.

### Blocked issues

Blocked issues appear in the dashboard but do not create a run until they become eligible.

---

## 9. Release Issues

A release may be represented as a normal GitHub issue whose GitHub blockers are all issues required for that release.

Example:

```text
Release 1.5
  blocked by #101
  blocked by #102
  blocked by #103
```

Mergeyard does not implement a separate release database or release scheduler.

If the release issue eventually becomes unblocked and has `ready-for-agent`, it enters the same eligibility queue as any other issue.

---

## 10. Scheduler and Concurrency

### Polling

Default polling interval:

```text
30s
```

Only one scheduler reconciliation tick may run at a time.

### Concurrency limits

Three limits are enforced:

- global active-run limit;
- per-repository active-run limit;
- per-runner active-run limit.

An active run consumes one slot from claim through any non-terminal state, including manual handoff and waiting for CI.

The MVP counts **runs**, not CPU processes or phase attempts. It does not dynamically schedule based on CPU/RAM usage.

### Runner routing

Resolve the runner for a new run in this order, using the first specified value:

```text
repository.runner
        ↓ if omitted
global default_runner
        ↓ if omitted
built-in local runner
        ↓
validate and persist resolved runner at claim
        ↓
same runner for the full run lifecycle
```

`default_runner` is a top-level configuration key. Omitting it is equivalent to `default_runner: local`. Omitting both `default_runner` and the entire `runners` mapping is valid for local-only operation; other required repository, harness, and workflow configuration still applies.

A repository's `runner` overrides the global default, including `runner: local` when the global default is an SSH runner. Merely declaring an SSH runner does not select it. An explicit unknown runner ID or invalid runner definition is a configuration error, never a reason to silently use `local`.

Persisted runner assignment takes precedence over current routing settings for existing runs. Runner health and capacity determine whether work can start on that runner; they do not select an alternative. No automatic fallback, load-based routing, or phase-level runner overrides are supported in the MVP.

### Pausing the scheduler

Global pause means:

- no new issues are claimed;
- existing runs continue;
- CI monitoring/reconciliation for existing runs continues.

There is no generic run-level "freeze process" operation. For an individual run the supported controls are manual handoff, stop, and retry.

---

## 11. Runner Model

Runner and harness concerns are deliberately separated.

A runner provides command/session/file primitives. It does not know what Claude Code, Codex, or OpenCode mean.

Conceptual interface:

```go
type Runner interface {
    ID() string
    Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
    StartSession(ctx context.Context, req SessionRequest) (SessionRef, error)
    SessionStatus(ctx context.Context, ref SessionRef) (SessionStatus, error)
    StopSession(ctx context.Context, ref SessionRef) error
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
    RemovePath(ctx context.Context, path string) error
    Health(ctx context.Context) RunnerHealth
}
```

### Local runner

Executes commands directly on the control-plane machine. Mergeyard automatically provides the reserved runner ID `local` with:

```yaml
# Built-in effective defaults; this declaration is optional.
runners:
  local:
    type: local
    workspace: ~/.mergeyard
    max_concurrency: 1
```

An optional `runners.local` entry customizes its workspace and manually configured concurrency; omitted fields retain the built-in defaults. The ID `local` cannot be redefined as an SSH runner. Its home directory and workspace resolve on the control-plane machine.

The presence of the built-in runner does not require local execution readiness in remote-only mode. Validate execution dependencies for runners assigned to repositories or retained by existing runs.

### SSH runner

Executes commands through the system `ssh` binary and an SSH-config-compatible host name. SSH runners are optional, explicitly declared, and independent of machine provider. Workspace paths and home directories resolve on the remote machine. Mergeyard does not require provider names, plans, or provisioning APIs.

Example:

```yaml
runners:
  devbox:
    type: ssh
    host: mergeyard-devbox
    workspace: ~/.mergeyard
    max_concurrency: 4
```

`host` may be an SSH config alias. Mergeyard should rely on the user's existing `~/.ssh/config`, keys, agent, ProxyJump configuration, and host-key policy rather than inventing another SSH credential system.

SSH execution must be non-interactive (`BatchMode`) for automation. Password prompts are treated as configuration errors.

### Runner health

A runner is `HEALTHY`, `DEGRADED`, or `UNAVAILABLE`.

A runner is not eligible for new dispatch if it cannot:

- execute a basic command;
- access its workspace;
- find `git` and `tmux`;
- find the harnesses required by its assigned repositories.

Optional CPU/RAM metrics may be displayed but do not affect scheduling in the MVP.

---

## 12. Managed Repository and Worktree Layout

Mergeyard uses managed checkouts instead of depending on an arbitrary developer working directory.

For each runner:

```text
<workspace>/
  repos/
    <owner>-<repo>/
      base/
  worktrees/
    <owner>-<repo>/
      <run-id>/
  runs/
    <run-id>/
      input/
      phases/
      logs/
      metadata/
```

Default workspace:

```text
~/.mergeyard
```

### Base checkout

On first use, Mergeyard clones the configured GitHub repository into the runner's managed `base/` checkout.

Before each new run it:

1. verifies the origin matches the configured repository;
2. fetches origin;
3. prunes stale remote refs;
4. verifies the configured base branch exists;
5. resets the managed base checkout to the current remote base branch only when that checkout has no active worktree conflict.

The managed base checkout must never be used for agent edits.

### Worktree

Each run receives exactly one worktree.

Default path:

```text
<workspace>/worktrees/<owner>-<repo>/<run-id>
```

The worktree remains on the resolved runner for the entire run.

### Branch naming

Default:

```text
mergeyard/issue-<number>
```

If that branch already exists remotely:

- if it belongs to an existing/reconcilable Mergeyard run, reuse it;
- otherwise stop with `NEEDS_ATTENTION` instead of overwriting it.

### Cleanup

Successful completed runs:

- remove the worktree;
- prune local worktree metadata;
- optionally delete the local branch;
- never delete the remote merged branch unless configured.

Failed, stopped, manual, or needs-attention runs preserve their worktrees by default.

---

## 13. Git Authentication on Runners

Every runner must be able to clone/fetch/push the configured repositories without interactive prompts.

Mergeyard does not copy GitHub credentials to runners and does not enable SSH agent forwarding automatically.

Recommended approaches include a preconfigured SSH key or other Git credential mechanism on the runner.

`mergeyard doctor` must verify, for each configured repository/runner pair:

- read access to the origin;
- ability to resolve the base branch;
- push access when the workflow requires PR creation.

---

## 14. Harness Adapter Model

The harness adapter describes **how to invoke and interpret a coding harness**. It does not directly own process execution; the runner/session layer executes the invocation.

Conceptual interface:

```go
type HarnessAdapter interface {
    Type() string
    Capabilities() HarnessCapabilities
    ValidateConfig(profile HarnessProfile) error
    BuildInvocation(ctx PhaseContext, profile HarnessProfile) (Invocation, error)
    ParseResult(ctx PhaseContext, artifacts PhaseArtifacts) (PhaseResult, error)
}
```

This separation is intentional:

```text
Workflow phase
    ↓
Harness adapter -> Invocation
    ↓
Runner/session manager -> Process
    ↓
Artifacts/result
    ↓
Harness adapter -> Normalized PhaseResult
```

Initial adapter types:

- `claude`;
- `codex`;
- `opencode`.

### Harness profiles

A harness profile is a named configured instance of an adapter.

Example:

```yaml
harnesses:
  opencode-go:
    type: opencode
    executable: opencode

  codex-review:
    type: codex
    executable: codex
```

This permits multiple profiles for one harness type without coupling workflow names to executables.

### Capabilities

Adapters declare capabilities such as:

```text
model_selection
effort_selection
skill_selection
structured_output
native_session_id
```

Configuration that requests an unsupported required capability fails validation before scheduling begins.

### Harness session reuse

By default, every automated phase attempt starts a **new harness process/session**.

Implementation and review must never share one harness conversation/session.

The MVP does not depend on harness-native conversation resume for normal automation. A harness-native session ID may be recorded as metadata when available.

---

## 15. Session Model

The MVP uses **tmux as the session backend for both local and SSH runners**.

This creates one execution model across machines and allows:

- the control plane to restart without killing agent processes;
- users to attach to live sessions;
- phase logs to remain associated with a stable session;
- remote runs to continue while the laptop sleeps or disconnects.

### Session naming

```text
mergeyard-<short-run-id>-<phase>-<attempt>
```

Names must be sanitized and remain below tmux naming limits.

### Phase wrapper

Mergeyard starts a generated wrapper script inside tmux. The wrapper:

1. changes to the run worktree;
2. applies only the environment explicitly configured for the phase;
3. executes the harness invocation;
4. streams stdout/stderr to a phase log file;
5. records the process exit code in phase metadata;
6. exits the tmux command when the harness exits.

Mergeyard determines phase completion from the wrapper exit metadata plus any required result artifact; it does not infer completion from terminal text.

### Control-plane shutdown

Stopping Mergeyard does not automatically stop active tmux sessions.

On restart, Mergeyard reconciles stored sessions against tmux and GitHub state.

---

## 16. Fixed MVP Workflow

The MVP is **not** a generic DAG/workflow engine.

A workflow is a named configuration profile for a fixed lifecycle:

```text
IMPLEMENT
   ↓
CREATE/UPDATE PR
   ↓
REVIEW (optional)
   ↓
FIX <-> REVIEW (bounded)
   ↓
WAIT FOR CI
   ↓
CI FIX (optional, bounded)
   ↓
READY TO MERGE
   ↓
MERGE or WAIT FOR MANUAL MERGE
```

Agent-backed phases:

- `implement`;
- `review`;
- `fix`;
- `ci_fix`.

System phases:

- workspace preparation;
- Git operations;
- PR creation/update;
- CI monitoring;
- merge;
- cleanup.

Generic user-defined phase graphs are a future feature.

---

## 17. Phase Input Contract

Before every agent-backed phase attempt Mergeyard writes a normalized context file outside the Git worktree:

```text
<workspace>/runs/<run-id>/phases/<phase>/<attempt>/input.md
```

The harness prompt references that file and the expected output path.

The input contains, as applicable:

- repository name;
- issue number/title/body/URL;
- base branch;
- worktree path;
- phase objective;
- PR number/URL;
- review findings from the previous cycle;
- failing CI checks and available failure excerpts;
- explicit constraints;
- required output contract.

Issue/PR text must be written to files or passed through stdin. It must not be interpolated unsafely into shell command strings.

### Repository instruction files

Mergeyard does not merge or reinterpret harness-specific repository instruction files such as `AGENTS.md` or `CLAUDE.md`. They remain in the repository and may be consumed naturally by the selected harness.

---

## 18. Phase Result Contract

Automated orchestration requires machine-readable outcomes independent of harness prose.

Each agent-backed phase attempt is given an expected output path:

```text
<workspace>/runs/<run-id>/phases/<phase>/<attempt>/result.json
```

The adapter prompt must instruct the harness to create it.

### Common fields

```json
{
  "schema_version": 1,
  "status": "success",
  "summary": "Short phase summary"
}
```

### Implementation/fix/CI-fix statuses

Allowed:

```text
success
blocked
failed
```

`blocked` means the agent cannot safely continue without human input or missing external information.

### Review result

```json
{
  "schema_version": 1,
  "status": "changes_required",
  "summary": "Two blocking findings",
  "findings": [
    {
      "severity": "blocking",
      "title": "Missing validation",
      "details": "VAT input accepts an invalid format.",
      "file": "src/checkout.ts",
      "line": 42
    }
  ]
}
```

Allowed review statuses:

```text
approved
changes_required
blocked
failed
```

Finding severity:

```text
blocking
warning
note
```

Only `blocking` findings prevent progression.

### Missing/invalid result

If the harness exits successfully but a required result file is missing or invalid, the phase attempt is considered failed and the run moves to `NEEDS_ATTENTION` unless a configured phase retry remains.

---

## 19. Skills

Skills are logical execution inputs associated with agent-backed phases.

Mergeyard supports:

- no skills;
- reusable skillsets;
- repository-contained skills;
- runner-installed global skills;
- Matt Pocock-style skills;
- user-defined skills.

Example:

```yaml
skillsets:
  implementation:
    - executing-plans

  review:
    - code-review
```

Adapters translate those names into harness-specific behavior.

### Remote skill synchronization

The MVP does **not** synchronize global skill directories from the control plane to SSH runners.

If a workflow references a global skill, that skill must already be available to the configured harness on that runner. Repository-contained skills naturally travel with the repository.

`doctor` should validate skill availability where the adapter can do so reliably; otherwise it reports the check as unverifiable rather than assuming success.

---

## 20. Git and Commit Behavior

### Starting point

Every run starts from the fetched remote base branch, not an arbitrary local branch state.

### Dirty managed checkout

A dirty managed base checkout is an error and prevents new work for that repository until reconciled.

### Agent commits

Harnesses may create commits, but they are not required to.

Before a branch is pushed after an implementation/fix phase, Mergeyard checks the worktree:

- if there are staged/unstaged tracked changes, Mergeyard creates a final commit;
- if untracked files exist, only non-ignored files are included;
- if no diff from the base branch exists after implementation, the run moves to `NEEDS_ATTENTION` with reason `no_changes`.

Default generated commit message:

```text
mergeyard: #<issue> <issue title>
```

The commit uses the Git identity already configured on the runner.

### Force push

Mergeyard must not force-push by default.

If the remote branch diverges unexpectedly, the run moves to `NEEDS_ATTENTION`.

---

## 21. Pull Request Contract

After successful implementation:

1. push the run branch;
2. discover an existing open PR for that branch or create one;
3. persist PR number/URL;
4. continue with review/CI according to workflow.

Default PR title:

```text
#<issue> <issue title>
```

Default PR body contains:

- link/reference to the issue;
- `Closes #<issue>` when the repository is the same;
- implementation phase summary;
- a small Mergeyard-generated footer identifying the run ID.

Mergeyard updates its generated section without overwriting user-written PR body content outside that section.

If more than one open PR exists for the branch, the run moves to `NEEDS_ATTENTION`.

---

## 22. Review Flow

Review must use a new, independent harness session from implementation.

The review phase inspects the PR branch against the configured base branch.

### Read-only contract

Review is logically read-only.

Mergeyard records the worktree Git state before review. After review:

- the result file may change outside the worktree;
- repository changes created by the reviewer are not accepted;
- if the review modified the repository, Mergeyard restores the pre-review worktree state and records a warning.

### Review cycles

Example:

```yaml
review:
  enabled: true
  max_cycles: 2
```

Cycle semantics:

```text
review cycle 1
  ├─ approved -> CI
  └─ changes_required -> fix cycle 1 -> review cycle 2

review cycle 2
  ├─ approved -> CI
  └─ changes_required -> NEEDS_ATTENTION
```

`max_cycles` counts review attempts, not fix attempts.

### Review audit trail

By default Mergeyard stores review findings locally and shows them in the dashboard.

Optional configuration may publish one generated PR comment per review cycle. Publishing review comments is disabled by default for the MVP to avoid noisy repositories.

---

## 23. CI Flow

Mergeyard monitors GitHub checks for the run PR.

Normalized CI states:

```text
pending
passed
failed
unknown
```

### Required checks

Mergeyard respects the repository/GitHub merge state and never bypasses branch protection or required checks.

### CI failure handling

If CI fails and `ci_fix.enabled` is false:

```text
NEEDS_ATTENTION
```

If CI fix is enabled:

1. collect failed check names and available failure excerpts/URLs;
2. run `ci_fix` in a new harness session;
3. commit/push changes if any;
4. wait for CI again;
5. stop after `max_attempts` is exhausted.

Example:

```yaml
ci_fix:
  enabled: true
  max_attempts: 2
```

Mergeyard does not indefinitely rerun flaky checks in the MVP.

---

## 24. Merge Behavior

When all of the following are true:

- automated review is approved or review is disabled;
- CI is passed according to repository requirements;
- PR is open and mergeable;
- run is not manual/stopped/needs-attention;

Mergeyard transitions to `READY_TO_MERGE`.

### Auto-merge enabled

Default strategy:

```text
squash
```

Mergeyard requests a normal GitHub merge and does not bypass protections.

### Auto-merge disabled

The run remains `READY_TO_MERGE` and occupies its active run slot until GitHub reports the PR merged or the user stops the run.

The dashboard clearly shows `Waiting for manual merge`.

After a manual GitHub merge, reconciliation completes cleanup automatically.

---

## 25. Manual Handoff

Mergeyard must not embed a terminal in the browser.

### Attach

Every running agent-backed phase exposes:

```text
Attach to session
```

The canonical CLI action is:

```text
mergeyard attach <run-id>
```

It attaches the user's terminal to the current tmux session, locally or through SSH.

The browser may show/copy the equivalent command. Optional OS-specific terminal launching is convenience behavior, not a core requirement.

### Enter manual mode

Before attaching interactively, Mergeyard transitions the run to `MANUAL` and stops automated progression for that run.

No automated phase is started while the run is manual.

### Return to automation

The user explicitly selects `Return to automation`.

Mergeyard then:

1. verifies runner connectivity;
2. inspects the worktree/branch/PR state;
3. verifies no Mergeyard-owned automated phase process is still running;
4. records any manual commits/changes;
5. starts the **next required automated action in a new phase session**.

Mergeyard does not attempt to infer whether a generic harness is "idle" inside an interactive conversation. The user is responsible for ending/detaching the manual interaction before returning control.

---

## 26. Run State Machine

Run state and phase are separate fields.

### Run states

```text
CLAIMING
PREPARING
ACTIVE
WAITING_FOR_CI
READY_TO_MERGE
MANUAL
NEEDS_ATTENTION
FAILED
STOPPED
COMPLETED
```

`ACTIVE` is paired with a phase such as `implement`, `review`, `fix`, or `ci_fix`.

### Terminal states

```text
FAILED
STOPPED
COMPLETED
```

`NEEDS_ATTENTION` is non-terminal because the user may retry.

### Important transitions

| From | Trigger | To |
|---|---|---|
| none | eligible issue claimed | CLAIMING |
| CLAIMING | GitHub claim succeeds | PREPARING |
| PREPARING | worktree/session inputs ready | ACTIVE/implement |
| ACTIVE/implement | success | ACTIVE/review or WAITING_FOR_CI |
| ACTIVE/review | approved | WAITING_FOR_CI |
| ACTIVE/review | changes required and cycles remain | ACTIVE/fix |
| ACTIVE/fix | success | ACTIVE/review |
| WAITING_FOR_CI | CI passes | READY_TO_MERGE |
| WAITING_FOR_CI | CI fails and repair enabled | ACTIVE/ci_fix |
| ACTIVE/ci_fix | success | WAITING_FOR_CI |
| READY_TO_MERGE | PR merged | COMPLETED |
| any active | user takes control | MANUAL |
| MANUAL | return to automation | reconciled prior/next state |
| recoverable active | bounded loop exhausted / semantic ambiguity | NEEDS_ATTENTION |
| NEEDS_ATTENTION | user retries | reconciled active state |
| any non-completed | user stops | STOPPED |
| unrecoverable internal failure | terminal error | FAILED |

The implementation should enforce transitions centrally; UI and CLI actions must not directly mutate arbitrary states.

---

## 27. Stop, Retry, and Recovery Semantics

### Stop

`mergeyard stop <run-id>`:

1. sends a graceful interrupt to a currently running Mergeyard-owned phase session;
2. waits for the configured grace period;
3. terminates the tmux session if still active;
4. preserves the worktree and branch;
5. removes the GitHub running label;
6. adds needs-attention unless no work was ever created;
7. marks the local run `STOPPED`.

Stop never deletes code automatically.

### Retry

Retry is allowed from `NEEDS_ATTENTION` and selected `FAILED` states.

Retry:

- reuses the same run ID, branch, runner, and worktree;
- creates a new phase attempt;
- does not reset user changes automatically;
- first reconciles Git/PR/CI state and determines the next safe phase.

### Infrastructure retries

Short-lived infrastructure operations such as SSH connection establishment or GitHub polling may use bounded automatic retries with backoff.

Agent semantic failures do not retry indefinitely. Phase attempts default to one automated attempt unless explicitly configured otherwise.

---

## 28. Restart and Reconciliation

At startup, Mergeyard reconciles SQLite, GitHub, Git state, and tmux state before dispatching new work.

For every non-terminal run it verifies:

- issue state and claim labels;
- runner reachability;
- managed repository existence;
- worktree existence;
- branch/remote branch state;
- PR state;
- tmux phase session state;
- CI/merge state when applicable.

### Active session survived restart

If tmux shows the recorded session is still running, Mergeyard restores the run as active and continues monitoring it.

### Session completed while control plane was offline

Mergeyard reads the phase wrapper exit metadata/result artifact and continues the workflow.

### GitHub PR was manually merged while offline

Mergeyard marks the run completed and performs cleanup when safe.

### Orphaned GitHub claim

If an issue has `agent-running` but there is no reconcilable local run, Mergeyard does **not** dispatch it. It is shown as an orphaned claim requiring manual reconciliation.

The MVP does not promise full run reconstruction after SQLite deletion. GitHub labels prevent accidental duplicate dispatch, but local session history may be lost.

---

## 29. Runtime State and SQLite Data Model

SQLite is runtime/bookkeeping storage, not project truth.

Minimum logical tables:

### `runs`

```text
id
repository
issue_number
workflow
runner
state
current_phase
automation_mode
branch
worktree_path
pr_number
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
cycle
attempt
harness_profile
model
effort
status
session_name
harness_session_id
input_path
result_path
log_path
exit_code
started_at
ended_at
error
```

### `events`

```text
id
run_id
type
payload_json
created_at
```

### Optional cached state

Repository/runner health may be cached but must be refreshable from external reality.

---

## 30. Event and Logging Model

Core events include:

```text
scheduler.paused
scheduler.resumed
runner.health_changed
run.claimed
run.preparing
run.manual
run.automated
run.needs_attention
run.failed
run.stopped
run.completed
phase.started
phase.output
phase.completed
phase.failed
pr.created
pr.updated
review.completed
ci.updated
merge.completed
```

Consumers:

```text
core event
  ├── SQLite event record
  ├── structured application log
  └── SSE update to browser
```

### Phase logs

Full phase stdout/stderr is written to runner-local files:

```text
<workspace>/runs/<run-id>/logs/<phase>-<attempt>.log
```

The dashboard displays a bounded tail, not an unbounded transcript.

Mergeyard must not intentionally log environment-variable values or credentials. The MVP does not claim to provide a complete secret-redaction engine for arbitrary harness output.

---

## 31. Configuration Ownership and Precedence

### Source of truth

For the MVP, **YAML is the configuration source of truth**.

The browser Settings page is read-only/effective-configuration + diagnostics. Editing and persisting configuration through the browser is a future feature.

### Config search order

1. explicit `--config <path>`;
2. `./mergeyard.yaml`;
3. `~/.config/mergeyard/config.yaml` when present.

If no configuration exists, `mergeyard init` may generate an example file.

### Effective configuration precedence

```text
built-in safe defaults
  ↓
global config
  ↓
workflow config
  ↓
repository config
  ↓
resolved run config
```

Issue-level configuration overrides are not part of the MVP.

Configuration is validated on startup. Invalid required configuration prevents the scheduler from starting.

Configuration changes require a process restart in the MVP. New runs use the updated configuration; existing runs retain their persisted runner assignment.

Runner selection follows section 10 separately from phase configuration: `repository.runner` overrides top-level `default_runner`, which defaults to built-in `local`. Workflow, phase, and issue settings cannot override the runner. Effective diagnostics include the built-in local runner even when omitted from YAML, and show each repository's resolved runner and each existing run's persisted runner.

---

## 32. Configuration Examples

### Local-only routing with the implicit default

This routing fragment uses the common harness/workflow configuration in the complete example below. No `default_runner`, `runners`, or repository `runner` is required:

```yaml
repositories:
  - repo: company/api
    base_branch: main
    workflow: feature
    concurrency: 1
```

All runs use built-in `local` with workspace `~/.mergeyard` and `max_concurrency: 1`. Writing `default_runner: local` explicitly has the same routing behavior. A `runners.local` entry is needed only to customize its settings.

### Remote-only routing with an explicit global default

Use this fragment in place of the complete example's runner and repository settings, retaining its common harness/workflow configuration:

```yaml
default_runner: devbox

runners:
  devbox:
    type: ssh
    host: mergeyard-devbox
    workspace: ~/.mergeyard
    max_concurrency: 2
    connect_timeout: 10s

repositories:
  - repo: company/api
    base_branch: main
    workflow: feature
    concurrency: 2

  - repo: company/frontend
    base_branch: main
    workflow: feature
    concurrency: 1
```

Both repositories inherit `devbox`; no runs execute locally. Alternatively, each repository may explicitly set `runner: devbox` while omitting `default_runner`. Adding `runner: local` to one repository overrides the SSH default and produces hybrid execution.

### Complete hybrid configuration

The following example customizes built-in local capacity and adds an optional SSH runner. `default_runner` is omitted, so repositories without a runner setting use `local`.

```yaml
version: 1

server:
  host: 127.0.0.1
  port: 7331
  open_browser: true

state:
  directory: ~/.mergeyard-control

scheduler:
  interval: 30s
  global_concurrency: 4

labels:
  ready: ready-for-agent
  running: agent-running
  needs_attention: agent-needs-attention
  failed: agent-failed

ui:
  terminal: auto
  editor: auto

runners:
  local:
    type: local
    workspace: ~/.mergeyard
    max_concurrency: 2

  devbox:
    type: ssh
    host: mergeyard-devbox
    workspace: ~/.mergeyard
    max_concurrency: 4
    connect_timeout: 10s

harnesses:
  opencode-workhorse:
    type: opencode
    executable: opencode

  codex-review:
    type: codex
    executable: codex

  claude-planner:
    type: claude
    executable: claude

skillsets:
  implementation:
    - executing-plans

  review:
    - code-review

workflows:
  feature:
    implement:
      harness: opencode-workhorse
      model: minimax-m3
      effort: medium
      skillsets:
        - implementation
      max_attempts: 1

    review:
      enabled: true
      harness: codex-review
      model: gpt-5.6
      effort: high
      skillsets:
        - review
      max_cycles: 2
      publish_comment: false

    ci_fix:
      enabled: true
      harness: opencode-workhorse
      model: minimax-m3
      effort: medium
      max_attempts: 2

    merge:
      auto: true
      strategy: squash

repositories:
  - repo: company/api
    runner: devbox
    base_branch: main
    workflow: feature
    concurrency: 2

  - repo: company/frontend
    runner: devbox
    base_branch: main
    workflow: feature
    concurrency: 2
    merge:
      auto: false

  - repo: rafael/side-project
    # No runner specified: uses the implicit built-in local default.
    base_branch: main
    workflow: feature
    concurrency: 1
```

Exact model identifiers remain user configuration. Mergeyard does not maintain a canonical model catalog.

---

## 33. Web Application

Default address:

```text
http://127.0.0.1:7331
```

The dashboard is the primary interface.

### Dashboard

Shows:

- scheduler running/paused state;
- global concurrency usage;
- runner health and concurrency usage;
- repository counts;
- running runs;
- ready issues;
- blocked issues;
- needs-attention runs;
- recent completed/failed runs.

### Repository page

Shows:

- repository;
- runner;
- base branch;
- workflow;
- concurrency;
- ready/running/blocked counts;
- active runs;
- eligible queue;
- unresolved blockers;
- sync/health errors.

### Queue page

Sections:

```text
Running
Ready
Blocked
Needs attention
Ready to merge
```

### Run detail

Shows:

- issue link/title/body summary;
- run ID;
- runner;
- state;
- phase and attempt;
- automation/manual mode;
- worktree path;
- branch;
- harness profile;
- adapter type;
- model/effort;
- skills;
- tmux session;
- PR;
- review findings;
- CI state;
- timeline/events;
- bounded log tail;
- last error with error code.

Actions are state-dependent and may include:

```text
Attach
Return to automation
Stop
Retry
Open issue
Open PR
Copy worktree path
Copy attach command
```

There is no generic `Skip phase` action in the MVP because skipping arbitrary lifecycle gates can violate safety invariants.

### Settings/Diagnostics

Read-only in the MVP. Shows:

- effective config path;
- configured runners/harnesses/workflows;
- doctor results;
- binary versions when discoverable;
- state directory;
- polling/concurrency settings.

---

## 34. Web Interaction Model

Frontend:

- server-rendered Go templates;
- HTMX for actions/partial refreshes;
- SSE for runtime updates;
- Tailwind + daisyUI;
- Lucide SVG icons.

No SPA state store is required.

Suggested routes:

```text
GET  /
GET  /repositories
GET  /repositories/{owner}/{repo}
GET  /queue
GET  /runs/{id}
GET  /settings
GET  /events

POST /scheduler/pause
POST /scheduler/resume
POST /runs/{id}/manual
POST /runs/{id}/automated
POST /runs/{id}/stop
POST /runs/{id}/retry
```

State-changing HTTP requests require CSRF protection and loopback-origin checks even though the server binds only to localhost.

---

## 35. CLI

The CLI is an operational companion, not a TUI.

Initial commands:

```text
mergeyard
mergeyard start
mergeyard init
mergeyard status
mergeyard doctor
mergeyard open
mergeyard pause
mergeyard resume
mergeyard attach <run-id>
mergeyard stop <run-id>
mergeyard retry <run-id>
mergeyard reconcile
```

### `mergeyard` / `mergeyard start`

1. locate and validate config;
2. acquire the single-instance lock;
3. open/migrate SQLite;
4. validate minimum local dependencies;
5. reconcile existing runs;
6. start HTTP/SSE server;
7. start scheduler;
8. optionally open browser;
9. continue until interrupted.

### `mergeyard status`

Prints a concise non-interactive summary of scheduler, runners, and active runs.

### `mergeyard doctor`

Performs readiness diagnostics but does not mutate repositories beyond safe connectivity/access checks.

### `mergeyard reconcile`

Re-runs reconciliation for all active runs and reports orphaned claims/worktrees/sessions. It does not silently reset orphaned GitHub claims.

---

## 36. Doctor Requirements

`doctor` checks at least:

### Local control plane

- configuration parses;
- state directory writable;
- dashboard port available;
- `git` present;
- `gh` present and authenticated;
- `tmux` present if a repository resolves to local or an existing run retains local;
- `ssh` present if SSH runner configured.

### Per runner

Execution-readiness checks apply to runners assigned to repositories or retained by existing runs, including implicit `local`. An unused built-in local runner must not fail remote-only readiness because local execution tools are absent. An unused SSH runner's connectivity is diagnostic and must not block local-only operation; all declared runner definitions are still validated.

- runner reachable;
- workspace writable;
- `git` present;
- `tmux` present;
- configured harness executables present.

### Per repository/runner

- origin accessible;
- base branch exists;
- fetch works;
- push permission is available when required.

### Workflow validation

- referenced harness profile exists;
- requested adapter capabilities are supported;
- explicit runner references exist, built-in local defaults are applied, and the reserved local ID has a valid local definition;
- concurrency values are valid;
- bounded loop limits are positive and finite;
- auto-merge strategy is supported.

Doctor failures are grouped as `error`, `warning`, or `unverifiable`.

---

## 37. Startup, Shutdown, and Single-Instance Behavior

### Single-instance lock

Only one Mergeyard process may own a given state directory at a time.

A lock file/process lock prevents accidental duplicate local control planes.

This does not provide distributed locking across machines; managing the same repository from two independent Mergeyard installations remains unsupported.

### Graceful shutdown

On SIGINT/SIGTERM:

1. stop claiming new issues;
2. stop scheduler tick creation;
3. persist pending local state;
4. leave active tmux sessions running;
5. close HTTP/SSE and SQLite cleanly.

The next process startup performs reconciliation.

---

## 38. Security and Trust Model

### Local web boundary

The MVP binds only to loopback addresses.

Non-loopback binding and remote dashboard exposure are unsupported for the MVP.

### Issue trust boundary

Agent execution has the permissions of the configured runner and harness. Therefore an issue body is effectively executable intent once it receives the ready label.

The ready label must be treated as a trusted-maintainer authorization boundary.

Mergeyard does not sandbox arbitrary agent code or protect a runner from malicious instructions in a deliberately approved issue.

### SSH

- use the system SSH implementation;
- respect host-key verification;
- automation uses non-interactive mode;
- do not disable strict host checking automatically;
- do not store SSH private keys in Mergeyard config.

### Provider secrets

Mergeyard should rely on harness-native authentication and existing Git/GitHub authentication.

It should not store model-provider API keys unless a future adapter absolutely requires it.

### Shell safety

Untrusted issue text, PR text, review findings, and CI output must never be directly interpolated into shell commands.

Dynamic textual context is passed through files/stdin. Command arguments must be escaped/encoded by the runner implementation.

### Browser actions

State mutations require CSRF tokens and expected-origin validation to reduce attacks from unrelated local webpages targeting localhost.

---

## 39. Error Taxonomy

Errors should have stable codes for UI/CLI and tests.

Initial categories:

```text
config.*
github.*
runner.*
workspace.*
git.*
harness.*
phase.*
review.*
ci.*
merge.*
reconcile.*
internal.*
```

Examples:

```text
runner.unreachable
runner.missing_dependency
git.branch_conflict
git.push_rejected
phase.result_missing
phase.result_invalid
review.max_cycles_exceeded
ci.max_fix_attempts_exceeded
merge.not_mergeable
reconcile.orphaned_claim
```

Every `NEEDS_ATTENTION`/`FAILED` run must expose a human-readable message and stable error code.

---

## 40. Reliability Invariants

Mergeyard must enforce:

- no unbounded loops;
- no duplicate active run for one repository + issue;
- no runner migration mid-run;
- no implementation/review session reuse;
- no automated progression while run is `MANUAL`;
- no merge while checks are pending/failing;
- no bypass of GitHub merge protections;
- no dispatch for unresolved blockers;
- no force-push by default;
- no automatic deletion of a failed run's worktree;
- no automatic reset of unexpected user commits/branch divergence;
- no shell interpolation of issue/PR bodies;
- no new run on an unhealthy runner;
- no silent reset of orphaned claims.

When an invariant cannot be proven, prefer `NEEDS_ATTENTION`.

---

## 41. Technology Stack

### Backend

- Go;
- `net/http` / `http.ServeMux`;
- `html/template`;
- `os/exec`;
- `context` / `os/signal`;
- YAML configuration.

No Go web framework is required initially.

### Frontend

- server-rendered HTML;
- HTMX;
- Tailwind CSS;
- daisyUI;
- Lucide SVG icons;
- SSE.

### Storage

- SQLite through `database/sql`;
- no ORM initially;
- prefer a pure-Go SQLite driver to simplify distribution.

### External tools

- `gh` for GitHub operations;
- `git` for repository/worktree operations;
- system `ssh` for SSH runners;
- `tmux` for phase sessions.

### Distribution

Use `go:embed` for templates/static assets.

Target experience:

```text
brew install mergeyard
mergeyard
```

No Node runtime is required for end users. Node may be used at build time for Tailwind/daisyUI asset generation.

---

## 42. Suggested Project Structure

```text
mergeyard/
├── cmd/
│   └── mergeyard/
│       └── main.go
│
├── internal/
│   ├── app/              # startup/shutdown, single-instance ownership
│   ├── config/           # load, merge, validate effective config
│   ├── scheduler/        # discovery, eligibility, concurrency, claims
│   ├── workflow/         # fixed lifecycle + state transitions
│   ├── github/           # issues, blockers, labels, PR, CI, merge
│   ├── git/              # managed clone, branch, worktree, commits
│   ├── workspace/        # runner-relative managed paths/artifacts
│   ├── harness/
│   │   ├── harness.go
│   │   ├── claude.go
│   │   ├── codex.go
│   │   └── opencode.go
│   ├── runner/
│   │   ├── runner.go
│   │   ├── local.go
│   │   └── ssh.go
│   ├── sessions/
│   │   └── tmux.go
│   ├── skills/
│   ├── store/
│   ├── events/
│   ├── logs/
│   └── web/
│
├── web/
│   ├── templates/
│   └── static/
│
├── migrations/
├── mergeyard.example.yaml
├── go.mod
└── go.sum
```

---

## 43. MVP Delivery Plan

### MVP 0.1 — Local and optional SSH dispatch to PR

Must include:

- config loading/validation;
- single-instance lock;
- GitHub issue polling;
- ready-label eligibility;
- GitHub blockers;
- durable claim labels;
- built-in local runner with implicit default routing;
- optional SSH runner support;
- local-only, remote-only, and hybrid configuration/dispatch validation (SSH is required for remote-mode validation, not for local-only usage);
- managed repository checkout;
- isolated worktree;
- tmux session backend;
- one implementation harness adapter;
- phase input/result artifacts;
- commit/push;
- PR creation;
- SQLite runtime state;
- restart reconciliation;
- basic dashboard/run detail;
- `doctor`, `status`, `attach`, `stop`.

Stops after PR creation.

### MVP 0.2 — Independent review

Add:

- review harness profile;
- independent review session;
- structured review result;
- read-only review enforcement;
- bounded review cycles without fixes yet;
- dashboard review findings.

### MVP 0.3 — Fix loop + CI

Add:

- fix phase;
- bounded review/fix cycle;
- CI monitoring;
- CI-fix phase;
- bounded CI repairs;
- needs-attention flows.

### MVP 0.4 — Merge + cleanup

Add:

- ready-to-merge state;
- auto/manual merge modes;
- squash merge;
- post-merge issue reconciliation;
- successful cleanup.

### MVP 0.5 — Multi-repository hardening

Add/harden:

- multiple repositories;
- global/repository/runner concurrency;
- runner health dashboard;
- orphaned-claim detection;
- reconciliation edge cases.

### MVP 0.6 — Harness/configuration breadth

Add/harden:

- Claude Code adapter;
- Codex adapter;
- OpenCode adapter;
- model/effort capability validation;
- skillsets;
- repository overrides;
- effective-config diagnostics.

### MVP 0.7 — Manual handoff hardening

Add/harden:

- attach UX;
- manual/automated transitions;
- safe return-to-automation reconciliation;
- copy attach command/browser convenience actions.

---

## 44. Acceptance Criteria

The MVP is not accepted merely because the dashboard renders. The following behaviors must pass repeatably.

### Dispatch

Given an open issue with `ready-for-agent`, no blockers, and available capacity:

- exactly one run is created;
- the issue is claimed;
- one runner is resolved;
- one worktree is created from the correct remote base branch;
- the implementation harness starts in tmux;
- the control plane may restart without killing that session;
- successful code is committed/pushed;
- exactly one PR is created/discovered.

### Duplicate prevention

Repeated scheduler ticks and process restarts do not create a second active run, branch, worktree, or PR for the same claimed issue.

### Local-only and implicit default

With valid repository/harness/workflow configuration but no `default_runner`, no `runners` mapping, and no repository `runner`:

- startup resolves the repository to built-in `local`;
- its effective defaults are workspace `~/.mergeyard` and `max_concurrency: 1`;
- the worktree, tmux sessions, harnesses, tests, and cleanup execute locally;
- the configured lifecycle completes without an SSH runner, remote server, or cloud-provider configuration.

An explicit `default_runner: local` behaves identically. A partial `runners.local` customization retains defaults for omitted fields.

### Remote-only and optional SSH

With `default_runner: devbox` and every repository inheriting it, or every repository explicitly selecting a configured SSH runner:

- all run execution and worktrees remain on the selected SSH machines;
- no local harness, local tmux, or local execution workspace is required;
- the local control plane and localhost dashboard remain available;
- no cloud-provider-specific fields or APIs are required.

With the laptop/control plane disconnected after phase start, the phase remains running on the remote runner. After reconnection/restart, Mergeyard reconciles and continues. SSH runner support is tested separately from local-only usage.

### Hybrid and runner precedence

With one repository using implicit or explicit `local` and another explicitly selecting `devbox`, each dispatches to its resolved runner while enforcing global, repository, and runner limits. A repository's `runner: local` overrides `default_runner: devbox`. Declaring `devbox` without selecting it does not change the implicit local default.

An unknown explicit runner ID, an invalid runner definition, or redefining `local` as SSH fails configuration validation; none silently falls back to local.

### Runner lifecycle pinning

After a run is claimed, its persisted runner is used throughout all phases, CI wait, manual handoff, retries, restart/reconciliation, and cleanup. Changing the global default or repository routing affects newly claimed runs only. If the original runner becomes unreachable, the existing run reports the problem and waits for recovery on that runner. If the runner is removed from configuration, it requires intervention. Neither case migrates or falls back to another runner.

### Review independence

Review runs in a separate harness process/session from implementation and cannot silently modify accepted code.

### Bounded automation

A run exceeding configured review or CI-fix limits stops in `NEEDS_ATTENTION`; it never loops forever.

### Manual handoff

Attaching changes the run to `MANUAL`; Mergeyard does not progress that run until the user explicitly returns control.

### Merge safety

Mergeyard never auto-merges with failing/pending required checks or when GitHub reports the PR as unmergeable.

### Recovery

A restart during implementation, review, CI wait, or ready-to-merge reconstructs the correct state from SQLite + runner + GitHub without duplicating work.

---

## 45. Testing Strategy

### Unit tests

Cover:

- config precedence/validation, including implicit local defaults, repository/global runner precedence, optional local customization, and invalid runner references;
- eligibility rules;
- scheduler ordering;
- concurrency accounting;
- state-transition guards;
- review-cycle counting;
- CI-fix attempt counting;
- path/branch/session naming;
- error-code mapping;
- result-schema validation.

### Harness adapter contract tests

Use fake executables that simulate:

- successful result;
- missing result;
- invalid JSON;
- non-zero exit;
- long-running process;
- output streaming.

Normal CI should not require paid model calls.

### Local runner integration tests

Exercise real:

- Git repository;
- worktrees;
- tmux;
- generated wrapper scripts;
- process restart/reconciliation.

### SSH integration tests

Run against a disposable Linux SSH target in dedicated integration CI or developer testing. Verify command quoting, file transfer, tmux, disconnect/reconnect, and worktree behavior.

### GitHub integration tests

Use a dedicated test repository for end-to-end tests involving labels, issues, PRs, checks, and merges. Destructive/live tests must not run against arbitrary configured repositories.

---

## 46. Explicit Non-Goals for MVP

Do not build:

- a coding model;
- a model proxy/router;
- a new issue tracker;
- arbitrary workflow DAGs;
- automatic issue decomposition/planning;
- a desktop application;
- a full-screen TUI;
- an embedded terminal;
- an IDE/editor;
- a React/Vue/Svelte SPA;
- WebSockets unless SSE proves insufficient;
- a custom SSH protocol;
- a remote worker daemon;
- dynamic runner migration;
- automatic remote machine provisioning;
- automatic global-skill synchronization;
- Kubernetes;
- Redis;
- PostgreSQL;
- multi-user accounts/RBAC;
- billing;
- remote public dashboard exposure;
- GitLab/Linear abstraction before the GitHub loop is reliable;
- distributed locking across multiple Mergeyard control planes;
- automatic force-push/rebase of unexpected divergence.

---

## 47. Future Opportunities

After the core loop is reliable:

- GitHub Projects integration;
- webhook-driven updates alongside polling;
- planning/spec/ADR phases;
- issue creation/decomposition;
- configurable workflow DAGs;
- GitLab/Linear adapters;
- runner pools and capacity-aware routing;
- ephemeral cloud runner provisioning;
- runner bootstrap/provisioning;
- skill/config synchronization;
- team/multi-user mode;
- notifications;
- richer token/cost analytics;
- published review comments/check-runs;
- public API/plugin system;
- richer dependency/release visualization;
- browser-config editing;
- optional TUI client against the same core/API.

---

## 48. Final Architecture

The runner branches below are alternatives: local-only uses the local branch, remote-only uses SSH branches, and hybrid uses both. The local runner is built in; SSH branches are optional.

```text
                                  GitHub
                  issues / blockers / labels / PR / CI / merge
                                     │
                                     │ gh
                                     ▼
                    ┌─────────────────────────────────┐
                    │        Mergeyard Control        │
                    │        local Go process         │
                    │                                 │
                    │ Config + validation             │
                    │ Scheduler + claims              │
                    │ Fixed workflow state machine    │
                    │ GitHub adapter                  │
                    │ Harness adapters                │
                    │ Workspace/worktree manager      │
                    │ Runner abstraction              │
                    │ tmux session manager            │
                    │ SQLite + events                 │
                    │ HTTP + SSE                      │
                    └──────────────┬──────────────────┘
                                   │
                     ┌─────────────┴─────────────┐
                     │                           │
                     ▼                           ▼
              ┌──────────────┐            ┌──────────────┐
              │ Local runner │            │ SSH runner   │
              │ macOS/Linux  │            │ Linux host   │
              └──────┬───────┘            └──────┬───────┘
                     │                           │
              managed repo/worktree      managed repo/worktree
                     │                           │
                  tmux                        tmux
                     │                           │
          ┌──────────┼─────────┐      ┌─────────┼──────────┐
          ▼          ▼         ▼      ▼         ▼          ▼
       Claude      Codex   OpenCode  Claude    Codex    OpenCode


                 Browser dashboard: http://127.0.0.1:7331
                 Observe / control / attach / reconcile
```

Mergeyard remains intentionally small: it coordinates GitHub state, runner-local isolated work, harness invocations, structured phase results, reviews, CI, merging, and manual handoff. It does not replace the tools that perform the engineering work.

---

## 49. Implementation Decision Summary

The following decisions are locked for the MVP and should not be reinterpreted during implementation without changing this PRD:

1. GitHub is the project/work source of truth; SQLite is runtime state only.
2. The browser dashboard is the primary UI; the CLI is a companion; no TUI is required.
3. Mergeyard runs as one local control-plane process.
4. Local-only, remote-only, and hybrid execution are supported. The reserved built-in `local` POSIX runner exists automatically and is the implicit default; SSH runners are optional, explicitly configured, provider-agnostic remote Linux machines.
5. New-run routing uses `repository.runner`, then top-level `default_runner`, then built-in `local`. One run is pinned to its initially resolved runner for its entire lifecycle, including retries, manual handoff, restart/reconciliation, and cleanup; configuration changes never reroute it and runner failures never trigger automatic fallback.
6. Mergeyard manages its own repository checkout and worktrees on each runner.
7. tmux is the MVP session backend on all runners.
8. Harness adapters build/interpret invocations; runners execute them.
9. Every automated phase attempt starts a new harness process/session.
10. Implementation and review never share a harness session.
11. Agent-backed phases must return a normalized `result.json` artifact.
12. The MVP workflow is fixed; it is not a generic DAG engine.
13. YAML is the configuration source of truth; browser settings are read-only.
14. Issue-level model/harness overrides are not supported in the MVP.
15. Global skill synchronization to remote runners is not supported in the MVP.
16. Remote machine provisioning is not supported in the MVP.
17. Review/fix and CI-fix loops are always bounded.
18. Unexpected Git/PR/session state moves to `NEEDS_ATTENTION` instead of being guessed or overwritten.
19. Failed/stopped/manual worktrees are preserved by default.
20. Mergeyard never bypasses GitHub branch protection or required checks.
21. Multiple independent Mergeyard control planes managing the same repository are unsupported in the MVP.
22. The ready label is the authorization boundary for executing issue instructions with runner permissions.
