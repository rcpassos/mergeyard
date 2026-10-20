# Managed Git operations

`internal/git` implements PRD sections 10 and 16. Use one `git.Manager` per
locked `workspace.Workspace`; the manager serializes Git operations. It invokes
the installed Git executable directly, inherits the machine's Git identity and
credentials, honors transport URL rewrites, and disables terminal prompts.

## Deadlines and non-interactive transport

Every managed operation carries a deadline. Quick local reads of refs, the
index, or single paths get `Timeouts.Metadata` (`2m`). Operations that may run
repository hooks, stage, restore or remove the worktree, create one, or scan
its full status get `Timeouts.Worktree` (`10m`). The first transfer into a
managed base gets `Timeouts.Clone` (`1h`). Routine fetches and remote reads get
`Timeouts.Fetch` (`10m`). Pushes, including the repository's pre-push hook, get
`Timeouts.Push` (`30m`). `New` uses those defaults; `NewWithTimeouts` selects
shorter or longer ones. A caller-supplied deadline always wins, including while
waiting for another managed operation to finish, so the scheduler's
five-second publication budget stays authoritative. Worktree cleanup keeps the
manager's own deadlines instead: removing a large tree can outlast optional
network maintenance, and a killed removal must remain resumable.

An operation that commits and publishes keeps a budget per phase: `Worktree`
for the commit and its hooks, `Fetch` for remote reads, and `Push` for the push
and its pre-push hook, inside an outer bound that covers every phase. A hook
that outlasts the routine fetch budget but fits the push budget therefore
publishes, while a stalled push cannot run longer than `Push`. `CommitAndPush`,
`CommitFix`, and `CommitHandback` behave this way. The checks path calls
`CommitImplement` for the worktree phase; `PushImplement`, `PushFix`, and
`PushHandback` run their local preflight, including the full status scan, under
the worktree budget, then bind remote reads to `Fetch` and the push to `Push`.

A command that reaches its deadline is killed with its owned process group, and
the call returns once its output pipes are drained, so a repository hook or
transport helper that outlives Git cannot extend the operation; a command that
exited successfully still reports its own exit status. Deadlines are coded
failures: `git.timeout` names the stalled operation, how long it ran
and, for a phase deadline, the class budget that bounded it (for example
`push timed out after 30m0s; its push budget is 30m0s`). An earlier caller or
outer operation deadline is reported as `<operation> timed out after
<elapsed>, before its <class> budget of <budget> expired`, and a serialized
manager wait is reported as `wait timed out`. The dashboard and the saved
attention message show that explanation. An explicitly cancelled context still reports
`git.canceled`. Both flow through the scheduler's ordinary retry and attention
paths, and the next tick proceeds.

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
detached HEAD. The first clone runs under the clone budget in a staging
directory beside the base and is renamed into place only after Git succeeds, so
a killed or timed-out transfer never looks like a completed base. The next
attempt therefore runs a fresh clone under the clone budget rather than
restarting the transfer as a routine fetch. Each run gets
`worktrees/<owner>-<repo>/<run-id>` on `mergeyard/issue-<number>`. Refreshing
the base leaves other run worktrees alone.

Persist the returned `Run`, including `BaseSHA`, with the runtime run record.
Pass recorded runs through `PrepareRequest.KnownRuns` on reconciliation. An
existing remote branch, local branch, or worktree is reusable only when its
repository, issue, run ID, paths, branch, and remote match a recorded run.
Otherwise preparation returns `git.branch_conflict`. Resuming keeps local
commits and partial changes, restores a missing worktree from an owned local or
remote branch, and retains the original base SHA for comparison.

Before creating the branch and worktree, preparation writes
`worktrees/<owner>-<repo>/<run-id>.intent` and records there that the
creation finished. The intent outlives creation: the caller still has to
persist the run's ownership, and an interruption in that gap must leave the
evidence that this run owns the branch and worktree. Call `ConfirmOwnership`
once the run record holds the ownership; it releases the intent. A cancelled
caller keeps the intent, because the ownership it was about to save may not
have reached the database. A caller that resumes a run with persisted
ownership repeats the confirmation before the run continues, so a release that
failed once does not leave a stale claim behind. Preparation drops an intent
it finds for a run it already knows.

The next attempt for that run recognizes the deterministic branch and path
from the matching intent. A creation that finished is recovered without
discarding anything: the intent adopts the worktree when the checkout is on
the run branch, attached to the managed base, clean, and still at the recorded
start, and that commit stays the run's comparison point. A worktree that
differs in any other way holds work that arrived after the creation, so it is
preserved and refused with `git.branch_conflict`.

If `worktree add` was interrupted, the creation never finished, so the next
attempt removes the partial worktree and local branch and creates them again
from the fetched base. The leftover branch is discarded only while it still
points at the recorded start, and an existing checkout must still be on the run
branch and attached to the managed base. Git writes the worktree link,
reciprocal registration, and common directory before HEAD, so a deadline in
that window leaves no readable checkout; that state is recognizable only when
those links point back to this run's managed base, Git still holds the
initialization lock, and the pending HEAD names the run branch. Anything else,
including a non-empty path whose Git metadata is missing or unreadable, is
preserved because ownership cannot be established, and is refused with
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

Removal writes `<run-id>.cleanup` after ownership, publication, and
cleanliness checks pass and before Git starts deleting. If Git refuses without
deleting anything, for example because the worktree is locked, that record is
cleared so a later attempt still requires a clean worktree. If Git is killed,
the record survives and a later call finishes the removal, whether it deleted
tracked files, the worktree link, or both. On such a retry an intact tree is
rechecked: a lock, or any change other than the tracked deletions a killed
removal leaves, preserves it for human attention.

`Run.RemovalIntent` carries the scheduler's durable authorization recorded
before removal. It additionally finishes a missing-link state left by a removal
that predates the local record. Without either record, a missing link is still a
verification failure and the tree is preserved. After pruning, a registration
that remains (for example because it is locked) is reported instead of being
recorded as removed.

Leftover deletion checks the operation context between entries, so an expired
deadline or cancellation stops it before the manager releases its gate.

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
