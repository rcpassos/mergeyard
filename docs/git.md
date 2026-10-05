# Managed Git operations

`internal/git` implements PRD sections 10 and 16. Use one `git.Manager` per
locked `workspace.Workspace`; the manager serializes Git operations. It invokes
the installed Git executable directly, inherits the machine's Git identity and
credentials, honors transport URL rewrites, and disables terminal prompts.

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

## Finishing a phase

`Manager.CommitAndPush` accepts the saved run and a `Phase` containing the issue
title and `RequireChanges`. Set `RequireChanges` for implementation. Leave it
false for a fix that may dispute every finding without changing files.

The manager stages tracked edits/deletions and non-ignored new files. If the
index has changes, it commits `mergeyard: #<issue> <title>` using the machine's
identity. Existing agent commits remain in history. Implementation with no tree
diff from the pinned base returns `git.no_changes` before pushing, including
when an agent made only empty commits.

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
and needs-attention runs must keep their artifacts.

## Errors and verification

Errors use `fault.Error`, retaining a stable `git.*` code, filesystem path, and
wrapped cause. Key codes include `git.invalid_input`, `git.base_checkout`,
`git.origin_mismatch`, `git.dirty_base`, `git.base_branch`, `git.branch_conflict`,
`git.worktree_mismatch`, `git.no_changes`, `git.push_rejected`, and `git.canceled`.
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
output alone. The scheduler must persist contamination before calling restore.
Restoration is replayable after interruption; unsafe paths, switched branches,
submodules and unsupported file shapes require attention rather than cleanup.
