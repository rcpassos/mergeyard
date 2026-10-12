# Configuration package

`internal/config.Load(path)` loads an explicit path. With an empty path it searches
`./mergeyard.yaml`, then `~/.config/mergeyard/config.yaml`. Only a missing file
falls through; unreadable or invalid files return an error. Files are selected,
not combined. "Global config" in the PRD means top-level fields in the selected
file. `mergeyard.example.yaml` lists all defaults and demonstrates overrides.

`Parse(data)` and `Load(path)` return a resolved `Config` and the source
`Document`. Top-level fields override built-in defaults; each repository inherits
the global roles, labels, and concurrency, then overrides explicitly supplied
fields. `enabled` defaults to true. An empty `BaseBranch` means discovery is
deferred to runtime. Workspace paths retain their configured spelling; the
workspace package is responsible for expanding `~` when using them.

Model and effort strings pass through to the selected harness without a catalog.
`null` clears a model or effort override. An explicit empty `skills` list clears
inherited skills. CI timeouts, repository command timeouts, poll intervals and
usage-limit cooldowns must be positive durations. Resolved labels must be nonempty
and distinct, and repositories may only be listed once; both comparisons ignore
case.

Unknown keys are tolerated and retained. `Document.Warnings` returns diagnostics
with code `config.unknown_field`, a field path, and a message so callers such as
start/doctor can surface likely typos, including misspelled permission settings.
Warnings describe the last Parse/Load; reload after edits to refresh them.
Known configuration
sections must be mappings with unique string keys; YAML merge keys (`<<`) are
rejected rather than silently ignored. Ordinary anchors and aliases can be read.

## Advisory escalation rules

`escalation` flags ready-to-merge runs that need careful human review. Rules are
deterministic and use no LLM. A flag never blocks `READY_TO_MERGE` or starts
another agent. The default rules are:

```yaml
escalation:
  max_changed_lines: 400
  paths:
    ".github/**": ci/deploy
    "**/Dockerfile*": ci/deploy
    "**/*.tf": ci/deploy
    "**/migrations/**": migration
  new_dependencies: true
```

The size rule triggers when additions plus deletions exceed the threshold across
the entire diff. `0` disables it. Binary files count as zero lines, but still
match path rules. Paths are relative to the repository root: `*`, `?` and character
classes match within one path segment; `**` as a complete segment matches zero or
more directories. Thus `**/migrations/**` includes root and nested migrations.
Both old and new paths of renames are evaluated. Overlapping rules with the same
reason are grouped, with unique files and reasons sorted for deterministic output.

`new_dependencies` flags changes to `go.mod`, `package.json`, `pyproject.toml`,
`Cargo.toml` and `Gemfile`, including nested manifests. Any manifest edit counts;
this rule does not parse whether an individual dependency was added. Lockfile-only
changes do not count. There are no built-in auth or payment path rules; add the
paths appropriate to your repository.

Each repository inherits global settings and overrides supplied fields. A supplied
`paths` mapping **replaces** the inherited mapping, so include default paths you
want to retain. `{}` clears paths; `new_dependencies: false` disables manifest
flags. To disable all rules, set all three fields:

```yaml
repositories:
  - repo: company/api
    escalation:
      max_changed_lines: 0
      paths: {}
      new_dependencies: false
```

Mergeyard evaluates `git diff <saved-run-base>...<approved-head>` as CI readiness
completes. The run stores the base, approved head and `{reason, files}` list in
SQLite. Restarts retain that result; a different approved head causes evaluation
again with the current effective repository settings. Configuration changes alone
do not change flags already saved for an approved head. Advancing the PR head
without reviewer approval retains the existing changed-after-approval warning and
does not relabel the new commit as approved.

Flagged runs show a **Needs careful review** badge with reasons and files on the
dashboard and in `mergeyard status`. The PR body gets an owned escalation section
with the same reasons, files and approved commit. It preserves the generated
summary and user-written content, removes stale flags on a subsequent unflagged
approval, and is independent of `pr_comments`. Unflagged runs show no badge.
Diff evaluation or PR body publication failures produce advisory warnings and
retry on subsequent readiness observations, including after restart. They do not
prevent readiness. Incomplete or ambiguous PR ownership markers are left intact
for human repair.

## Per-issue harness overrides

An issue can select a role's harness with `mergeyard:implementer=claude`,
`mergeyard:implementer=codex`, `mergeyard:reviewer=claude`, or
`mergeyard:reviewer=codex`. Label names and values are matched case-insensitively;
the format is fixed. Each role falls back to its repository configuration when
there is no override.

Mergeyard resolves both roles when it claims the issue. The configured model and
effort apply only when the selected harness equals the configured role's agent;
otherwise both are unset and the selected harness uses its defaults. Skills
always come from the repository's role configuration. For example, a Claude role
with a configured model and skills, overridden to Codex, keeps those skills and
uses Codex's default model and effort.

The run stores each role's agent, model, effort, skills and harness source
(`label` or `config`). All later phases, CI repair, restarts, usage-limit resumes,
recovery probes, takeover, handback and Retry use that selection. Changing labels
or role configuration affects only subsequent claims. Runs created before this
feature have no stored selection and continue reading configuration. Harness
permissions, executables and execution policy remain configuration settings.
Run detail and `mergeyard status` show the stored selection.

Before claiming, Mergeyard validates both resolved roles against their selected
harnesses, including supported capabilities and skill syntax. For example,
Claude's namespaced skills such as `plugin:check` cannot be used by Codex. An
incompatible selection remains unclaimed, receives needs-attention and an
explanation, and can be corrected through role configuration or override labels.
Skills are preserved rather than silently discarded. Remove needs-attention
after correcting the selection to allow another claim.

Unknown agents or labels selecting different harnesses for the same role prevent
the claim. Mergeyard applies the repository's `needs_attention` label and posts
one comment naming the invalid labels and valid values. Fix the override labels
and remove the needs-attention label to make the issue eligible again. Repeated
labels selecting the same harness are allowed.

The claim gate checks each issue's resolved implementer harness for a usage limit
or exhausted-credit block. A restricted issue is skipped while later issues in
the repository are still considered, preserving their normal order. A restricted
reviewer permits the claim and waits at the review phase gate.

## Repository setup commands

`repositories[].setup` is a list of shell commands, defaulting to `[]`. Omit it or
leave it empty to keep the existing workspace and agent flow. It is a repository
setting; commands are never read from issue or PR text.

```yaml
repositories:
  - repo: company/api
    setup: ["npm ci"]
    command_timeout: 20m
```

After preparing a fresh worktree, Mergeyard runs the setup list before starting
the implementer. Setup also runs whenever Mergeyard recreates that worktree, so
commands must be idempotent. The list runs in order as one `bash --login` script
with `set -e` and `set -o pipefail`, using only the explicitly configured
environment. Commands share one subshell which inherits login startup variables
and functions; a failing command or pipeline stops the script. Logout hooks cannot
replace the configured commands' exit status.

`repositories[].command_timeout` is a positive Go duration, defaulting to `20m`.
It bounds an entire setup or checks script, rather than each command in the list.
When the foreground script finishes, Mergeyard stops any background processes
still in its owned process group. Setup and checks must finish the work they need
before returning; launching a background service does not keep it running.
The phase wrapper owns the timeout watchdog, which continues running if the control
plane restarts. Timeout and Stop end the whole owned process group. Stop preserves
the worktree.

Setup is a system step within `ACTIVE/implement`, recorded in phase metadata as
`step: setup`. Its tmux session is named
`mergeyard-<short-run-id>-setup-<round>-<n>` and can be watched with
`mergeyard watch <run-id>`. A failure or timeout moves the run to `NEEDS_ATTENTION`
with `workspace.setup_failed` and an output excerpt; the implementer is never
started. Explicit Retry reruns setup before the agent phase.

After a control-plane restart, Mergeyard waits on a live setup session and reads
exit metadata for a finished session. If the outcome is unknown, it stops any
remaining owned processes and reruns the idempotent setup list.
Removing the configured list does not bypass an already-started setup execution.
Mergeyard observes its result and, when the outcome is unknown, reruns its saved
commands with the originally captured timeout before starting the agent. New
executions for later phases or newly prepared worktrees use current configuration.

## Repository checks

`repositories[].checks` is a list of shell commands, defaulting to `[]`. Omit it
or leave it empty to keep the existing flow. Like setup, checks come only from
repository configuration, never from issue or PR text. Commands must be nonempty
and contain no NUL characters.

```yaml
repositories:
  - repo: company/api
    setup: ["npm ci"]
    checks: ["npm test", "npm run lint"]
    check_retries: 1
    command_timeout: 20m
```

After every successful `implement` or `fix`, including a CI-repair fix, Mergeyard
commits the changes, runs checks, then pushes and starts review. Every configured
check must have a durable passing result, and the worktree must match the clean
pinned commit before and after checks. Early `exit` or `exec`
cannot authorize skipped checks. Results obtained against a changed worktree
are saved as failures and cannot be reused after restoring files or restarting.
A failing commit is not pushed. Checks reuse setup's `bash --login` script, shared
shell,
`set -e`, `set -o pipefail`, configured environment, and process-group cleanup.
`command_timeout` (default `20m`) bounds the entire check list on each execution.

Checks are a system step inside the current `ACTIVE/implement` or `ACTIVE/fix`
phase, recorded as `step: checks`. The tmux session is named
`mergeyard-<short-run-id>-checks-<round>-<n>`, with executions counted separately
from agent attempts. Watch attaches to the live check session. Stop ends its
whole owned process group and preserves the worktree. On restart, Mergeyard waits
on a live session or reads its saved results. An unknown outcome causes remaining
owned processes to be stopped and checks to be rerun before publication; the
wrapper's timeout watchdog keeps running while the control plane is offline.
Clearing `checks` while a step is pending still requires that execution to finish
and its result to be handled before publication. Unknown outcomes replay the saved
configured commands with their captured timeout. Completed results remain in the
review input after the list is removed; later phases use current configuration.

`repositories[].check_retries` is a nonnegative integer, defaulting to `1`.
`0` disables automatic fixes after failed checks. Within the remaining allowance,
each failure resumes the implementer's session in the same phase with the failing command and an output
excerpt in its input file. These extra phase attempts consume only the check
retry allowance, not `max_attempts` or review rounds. Usage-limit waits retain
their usual handling and bounds. Restart preserves the check retry allowance and
resumes an in-progress check-retry attempt without granting another retry.

A failure or timeout with no retries remaining moves the run to
`NEEDS_ATTENTION` with `checks.failed`. Explicit Retry grants exactly one more
check/fix attempt, records the grant in run history, and reruns checks first so
manual repairs can pass without another agent attempt. It preserves the previous
check results and retry history rather than resetting the configured allowance.

Handback commits manual changes and runs checks before pushing or reviewing.
A failure requires attention with `checks.failed` immediately; Mergeyard does
not automatically fix the user's changes. The latest check results (command,
pass/fail, and output excerpt) are included in the review input. PR fix comments
also report check retries.

Current-base verification is tracked separately in
[#96](https://github.com/rcpassos/mergeyard/issues/96). Its planned
`verify_commands` setting defaults to `checks` when omitted; an explicit `[]`
disables that verification. `verify_commands` is not yet a supported setting.

## Editing configuration

Start with the `Document` returned by loading the user's file; for a new config,
use `Parse([]byte("{}"))`. Adding fields to this empty document produces readable
block YAML. Existing nonempty flow-style documents retain their style.
Use `Document.Set` to change individual fields:

```go
_, doc, err := config.Load("")
if err != nil { return err }
err = doc.Set([]string{"implementer", "agent"}, "codex")
if err != nil { return err }
err = doc.Set([]string{"repositories", "-"}, map[string]string{
    "repo": "owner/new-repo",
})
if err != nil { return err }
return doc.Write(doc.Path)
```

Path segments are mapping keys or zero-based sequence indices. `-` appends to an
sequence, creating it when its parent field is missing; missing mapping parents
are created. Append must be the final path segment. Replacing an entire
mapping or sequence replaces its children, so edit leaves or append repositories
to keep existing comments and unknown fields. Edits through aliases are rejected;
edit the anchor's original field instead. Resolved `Config` values do not change
when the document is edited; reload after saving to get the new effective config.

For setup, `Document.SetRoleAgents(implementer, reviewer)` updates the global
agents independently. Unchanged roles keep their aliases. When an agent changes,
aliases sharing its role mapping or agent scalar are copied before editing, so
the other role and explicit repository overrides retain their values. Comments,
unknown fields, and unrelated anchors and aliases are preserved.

`Write(path)` revalidates the edited document, writes a temporary file in the same
directory, and renames it over the destination. For an existing symlinked config,
the target is resolved and updated while the symlink remains intact. Dangling
symlinks return a write error without replacement. Invalid edits leave the original
file intact. Parent directories are created with mode `0700`; new files use
`0600`, and existing file permission bits are retained. Concurrent external edits
are not merged; reload before editing if another process may have changed it.

The writer preserves unknown fields, comments attached to retained nodes, key
order, and styles of unchanged nodes. It does not promise byte-for-byte formatting:
the YAML encoder normalizes indentation and may reposition comments or blank lines.

## Stable errors

Use `errors.As(err, &configError)` with `var configError *config.Error`. `Code`
is stable, `Path` identifies the file or configuration field, and `Unwrap`
retains underlying I/O errors. Validation applies to global and repository role
settings, including repositories with `enabled: false`.

| Code | Meaning |
| --- | --- |
| `config.not_found` | Selected file is missing, or neither search location exists |
| `config.read_failed` | File or home-directory lookup failed |
| `config.invalid_yaml` | Syntax, document shape, duplicate keys, or field type is invalid |
| `config.invalid_repo` | Repository must be an `owner/repo` slug |
| `config.duplicate_repo` | Repository is already listed, ignoring case |
| `config.invalid_labels` | Resolved labels must be nonempty and distinct, ignoring case |
| `config.invalid_agent` | Agent must be `claude` or `codex` |
| `config.invalid_concurrency` | Concurrency must be a positive finite integer |
| `config.invalid_max_rounds` | Round limit must be a positive finite integer |
| `config.invalid_max_attempts` | Role attempt limit must be a positive finite integer |
| `config.invalid_checks` | Check commands must be nonempty and contain no NUL |
| `config.invalid_check_retries` | Check retry limit must be a nonnegative finite integer |
| `config.invalid_escalation` | Size threshold must be nonnegative; path globs and single-line reasons must be valid |
| `config.invalid_max_waits` | Usage-limit wait bound must be a positive finite integer |
| `config.invalid_duration` | CI timeout, repository command timeout, poll interval or cooldown must parse as a positive Go duration |
| `config.invalid_permission_mode` | Claude permission mode must be `auto`, `acceptEdits`, or `bypassPermissions` |
| `config.invalid_sandbox` | Codex sandbox must be `workspace-write` or `danger-full-access` |
| `config.invalid_path` | Document edit path cannot be traversed |
| `config.write_failed` | YAML encoding or atomic file write failed |

Tool versions and harness capability checks belong to runtime/adapter validation;
this package performs no external commands or GitHub calls.

`ci_timeout` is a top-level positive duration, default `60m`. A run saves its
first wait start and deadline with reviewer approval. Pending/unknown changes,
restarts, and later configuration changes do not renew that deadline.

`usage_limits.cooldown` (default `30m`) and `usage_limits.max_waits` (default `3`)
apply only when a harness adapter classifies a temporary limit. Both production
adapters currently have this detection disabled, so these settings do not turn
native usage-limit failures into waits. Those failures consume the role's normal
`max_attempts` budget. Credit detection is also disabled; Check availability and
Retry-as-probe require its evidence-backed capability. See
[M3 acceptance](m3-acceptance.md) for the gates and current evidence.

`mergeyard status` and Settings report the configuration loaded by the running
control plane, its selected source path, resolved workspace, and effective
repository role overrides. Edit the selected file and restart to apply changes.

## Trusted labelers

Authorization is enforced before claim. `trusted_labelers` is an optional list of
GitHub logins, globally or per repository. A nonempty list is an allowlist that
replaces permission-based trust. Login comparisons ignore case. A repository
inherits the global list; an explicit repository list replaces it. `[]` clears an
inherited list and returns to the default policy; it does not disable enforcement.

```yaml
trusted_labelers: [alice, release-bot]
repositories:
  - repo: company/api
  - repo: company/frontend
    trusted_labelers: [bob]
  - repo: company/internal
    trusted_labelers: [] # trust write/maintain/admin permission
```

With no nonempty list, the actor must have `write`, `maintain`, or `admin`
repository permission. `triage` and `read` do not authorize execution. The GitHub
account running Mergeyard must be able to read collaborator permissions, issue
label events and GraphQL content edit history. If those reads fail or omit required
information, Mergeyard refuses the issue.

Mergeyard checks the most recent ready-label event, plus each present
`mergeyard:implementer=<agent>` and `mergeyard:reviewer=<agent>` label event. It
also checks every body edit at or after the ready-label event. An edit by an
untrusted actor prevents claim, even if a later edit has a trusted editor. Edits
before approval are accepted: a trusted maintainer reviews that content when
applying the ready label. Equal timestamps are treated conservatively as after
approval because GitHub timestamps do not establish ordering within a second.

A refusal creates no run. Mergeyard persists the reason, posts one comment with
`<!-- mergeyard:trust:refused -->`, and removes the ready label. Pending comment
and label operations survive restarts. The dashboard lists the issue under
**Needs attention**, including its refusal reason after the ready label is gone.
A trusted maintainer should review the body and override labels, remove or
reapply any untrusted overrides, then reapply the ready label. The historical
comment is retained, and a newly trusted decision clears the dashboard refusal.

The issue title, body and URL saved at claim remain the input for implementation,
review, fix and resumed attempts. Later issue edits do not replace this authorized
snapshot. Lifecycle polling still reads current issue state and labels. Refusal
comments must be authored by Mergeyard's authenticated GitHub account and contain
its explanation; copied markers from other accounts are ignored.

Authorization decisions are cached for the observed issue, label-event identity
and body-edit state during the client lifetime. The issue-list `updated_at`,
content and labels invalidate the snapshot, so a quiet poll adds no trust API
requests. Changes to the configured allowlist also invalidate it. A fresh issue read and uncached authorization check run immediately before a
claim. This catches permission revocation and rapid label changes even when an
unchanged issue snapshot was cached during blocked polls. A restart also rechecks
trust.
`doctor` warns when a public repository has no explicit allowlist.
