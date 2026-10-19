# Managed Git operations

`internal/git` implements PRD sections 10 and 16. Use one `git.Manager` per
locked `workspace.Workspace`; the manager serializes Git operations. It invokes
the installed Git executable directly, inherits the machine's Git identity and
credentials, honors transport URL rewrites, and disables terminal prompts.

## Deadlines and non-interactive transport

Every managed operation carries a deadline. Quick local reads of refs, the
index, or single paths get `Timeouts.Metadata` (`2m`). Operations that may run
repository hooks, stage or restore the worktree, or create and remove one get
`Timeouts.Worktree` (`10m`). Operations that contact the configured remote get
`Timeouts.Remote` (`10m`). `New` uses those defaults; `NewWithTimeouts` selects
shorter or longer ones. A caller-supplied deadline always wins, including while
waiting for another managed operation to finish, so the scheduler's
five-second publication and cleanup budgets stay authoritative.

An operation that commits and publishes keeps a budget per phase: `Worktree`
for the commit and its hooks, `Remote` for the push and its remote reads, and
an outer bound that covers both, so a slow hook cannot consume the push's
deadline and a stalled push cannot run longer than `Remote`. `CommitAndPush`,
`CommitFix`, and `CommitHandback` behave this way; the checks path calls
`CommitImplement` and `PushImplement`, which each bound one phase.

A command that reaches its deadline is killed with its owned process group, and
the call returns once its output pipes are drained, so a repository hook or
transport helper that outlives Git cannot extend the operation; a command that
exited successfully still reports its own exit status. Deadlines are coded
failures: `git.timeout` names the stalled operation (for example
`push timed out`) or the serialized wait, with the deadline as its cause. An
explicitly cancelled context still reports `git.canceled`. Both flow through
the scheduler's ordinary retry and attention paths, and the next tick proceeds.

Git never prompts from an unattended control plane. `GIT_TERMINAL_PROMPT=0` is
always set, `LC_ALL=C` keeps diagnostics stable, and SSH defaults to
`ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30 -o ServerAliveCountMax=4`,
so a missing credential or a dead connection fails instead of hanging. A
`GIT_SSH` or `GIT_SSH_COMMAND` already present in the environment, or a
`core.sshCommand` the machine's configuration resolves for the command's
repository, is the machine's own transport choice and is preserved. That
includes repository-scoped conditional includes such as `gitdir:` and
`hasconfig:remote.*.url:`, because the probe runs in the directory the command
runs in. Results are cached per directory and configuration source, and an edit
to a configuration file is picked up on restart. A first clone probes the
workspace: its repository does not exist yet, exactly as it does not for any
other `git clone`.

## Preparing a run

`Manager.Prepare` accepts a repository (`owner/repo`), issue number, and run ID.
The default remote is `https://github.com/<owner>/<repo>.git`; `RemoteURL` can
select a local mirror. The configured base branch is optional. When absent, Git
queries the remote's current default branch on each preparation.

Preparation clones once into `repos/<owner>-<repo>/base/`, verifies origin,
refuses a dirty base, fetches and prunes, and checks out the fetched base in
detached HEAD. Each run gets `worktrees/<owner>-<repo>/<run-id>` on
`mergeyard/issue-<number>`. Refreshing the base leaves other run worktrees alone.

Persist the returned `Run`, including `BaseSHA`, with the runtime run record.
Pass recorded runs through `PrepareRequest.KnownRuns` on reconciliation. An
existing remote branch, local branch, or worktree is reusable only when its
repository, issue, run ID, paths, branch, and remote match a recorded run.
Otherwise preparation returns `git.branch_conflict`. Resuming keeps local
commits and partial changes, restores a missing worktree from an owned local or
remote branch, and retains the original base SHA for comparison.

Before creating the branch and worktree, preparation writes
`worktrees/<owner>-<repo>/<run-id>.intent` and removes it once creation
succeeds. If `worktree add` is interrupted, the next attempt for that run
recognizes the deterministic branch and path from the matching intent, removes
the partial worktree and local branch, and creates them again from the fetched
base. The leftover branch is discarded only while it still points at the
recorded start, and an existing checkout must still be on the run branch and
attached to the managed base; anything else is preserved and refused with
`git.branch_conflict`. Cleanup removes an interrupted initialization lock so
Git no longer treats the branch as checked out. A matching intent never adopts
a remote branch, and any branch, worktree, or remote branch without a recorded
run or intent is still refused with `git.branch_conflict`.

## Finishing a phase

`Manager.CommitAndPush` accepts the saved run and a `Phase` containing the issue
title and `RequireChanges`. Set `RequireChanges` for implementation. Leave it
false for a fix that may dispute every finding without changing files.

The manager stages tracked edits/deletions and non-ignored new files. If the
index has changes, it commits `mergeyard: #<issue> <title>` using the machine's
identity. Existing agent commits remain in history. Implementation with no tree
diff from the pinned base returns `git.no_changes` before pushing, including
when an agent made only empty commits or left only excluded untracked files.

The saved `Run.Exclude` carries per-repository `exclude` patterns. Staging uses
`git add --all -- . ':(exclude,glob)<pattern>'`, then includes matching tracked
edits/deletions. A trailing `/` expands to `/**` so wildcard directories exclude
their whole subtree. New excluded files already in the index are unstaged before
committing. During a pending merge, tracked files from `HEAD` and every merge
parent are retained, including base additions matching an exclude pattern; existing agent commits remain intact. Patterns use Git glob pathspec
semantics, with `*` limited to a path component and `**` for recursive matching.
No Git configuration, shared `info/exclude`, or repository files are rewritten.

`CommitResult.Diagnostics` lists excluded untracked leftovers under
`git.excluded_untracked` and warnings for matching tracked changes under
`git.exclude_tracked`. The scheduler saves implement/fix diagnostics in each
attempt's `staging.json` and a `git.staging_diagnostics` run event, even on
`git.no_changes`. Excluded leftovers remain available during checks and review.
Pinned-tree inspections and retry ignore only excluded untracked files; tracked
or staged changes and other non-ignored files still require attention.

Push uses an explicit refspec for the run branch and the verified origin URL,
with force, mirrored updates, automatic tags, and recursive submodule pushes
disabled. An unrelated `remote.origin.pushurl` is ignored. A rejected update
returns `git.push_rejected`; the local commit and remote collaborator's work
remain intact. Other transport errors return `git.push`.

## Cleanup

Call `Manager.Cleanup` only after the run is completed. It removes the worktree,
prunes stale metadata, and deletes the local branch, including after a squash
merge. Remote branches remain. Cleanup can be retried after interruption and
refuses dirty worktrees or worktrees on a different branch. Stopped, manual,
and needs-attention runs must keep their artifacts. Completed cleanup permits
excluded untracked leftovers and removes them with the worktree after verifying
ownership, publication and absence of other dirty work.

## Errors and verification

Errors use `fault.Error`, retaining a stable `git.*` code, filesystem path, and
wrapped cause. Key codes include `git.invalid_input`, `git.base_checkout`,
`git.origin_mismatch`, `git.dirty_base`, `git.base_branch`, `git.branch_conflict`,
`git.worktree_mismatch`, `git.no_changes`, `git.push_rejected`, `git.timeout`,
and `git.canceled`.
Operation failures retain codes such as `git.clone`, `git.fetch`, `git.worktree`,
`git.stage`, `git.commit`, and `git.cleanup`.

`go test ./internal/git` runs integration tests against temporary local bare
repositories. Tests isolate user Git configuration and use a test identity;
they do not access GitHub or mutate a developer checkout.

## Review protection

SnapshotReview captures HEAD, index tree, branch/worktree HEAD reflogs and exact tracked and
non-ignored files, including symlinks and modes. ReviewChanged also detects
commits followed by resets. RestoreReview checks persisted ownership, restores
HEAD/index/files, removes newly created non-ignored files and leaves ignored
output alone. Snapshots inventory pre-existing ignored paths so force-adding an
artifact or changing ignore rules cannot make cleanup delete it. Older review
snapshots without that inventory require attention when restoration is needed.
The scheduler must persist contamination before calling restore.
Restoration is replayable after interruption; unsafe paths, switched branches,
submodules and unsupported file shapes require attention rather than cleanup.
