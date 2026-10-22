# Doctor

Run `mergeyard doctor` before starting the control plane. Use
`mergeyard doctor --config <path>` to select a configuration explicitly; without
it, doctor uses the configuration package's normal search order.

Doctor writes findings to stdout, grouped as `error`, `warning`, and
`unverifiable`. Each finding includes a stable code and its tool, repository,
or role. Empty groups show `none`. Exit status is `1` if any error exists and
`0` when only warnings or unverifiable findings exist. CLI usage errors still
exit with `2`.

The report also records the checks that passed, each with a stable pass code
and scope. The CLI output omits them; the dashboard's Settings page shows every
recorded check as a pass, warn or fail row.

For example, a repository using Codex with only `CLAUDE.md` and a missing
running label reports:

```text
error:
  [github.label_missing] octo/repo: required label "agent-running" is missing; create it or update this repository's label configuration
warning:
  [harness.instructions_missing] octo/repo: Codex is assigned but the base branch has CLAUDE.md without AGENTS.md; add AGENTS.md for repository instructions
unverifiable:
  none
```

## Machine checks

- Configuration loads and validates; unknown configuration fields are warnings.
- The workspace or its nearest existing parent allows a temporary directory
  and file to be created and removed. Doctor does not initialize a workspace,
  open SQLite, or acquire the control plane's lock.
- The dashboard port is in range and can bind to `127.0.0.1`. A running
  dashboard already occupying the port is reported as an error.
- Git, tmux, and GitHub CLI can run, and `gh auth status --hostname github.com`
  succeeds.
- Harnesses assigned to effective repository roles can run using their configured
  executables. With no repositories, doctor checks the global roles.
- Claude Code is at least `2.1.277`; Codex CLI is at least `0.156.1`. An
  unparseable version is unverifiable. A prerelease of the minimum version does
  not satisfy that minimum.
- Login succeeds through `claude auth status` or `codex login status`.
- Requested model and effort selection flags appear in harness help. Doctor
  checks capabilities without validating model names or effort values against
  a catalog. Both supported harness versions select skills through prompts.
- Claude's `bypassPermissions` cannot be combined with effective UID `0`.

## Repository checks

Doctor checks every configured repository, including disabled repositories,
using its resolved roles, labels, and base branch.

- Git can access the configured GitHub origin.
- The configured base branch, or origin's default branch, resolves.
- A shallow fetch succeeds into a disposable bare repository.
- The authenticated GitHub account has `permissions.push`, and Git push
  transport works with a dry run. Missing permission information is
  unverifiable; an explicit denial is an error. The dry run does not prove that
  branch protection, receive hooks, or CI will accept a later push.
- All three configured labels exist; pagination and case-insensitive names
  are supported.
- Role skills have a `SKILL.md` at a supported repository or personal/system
  location that resolves to a regular file. Repository checks use Git's native
  symlink resolution, including symlinked directories and chains. Dangling
  links, cycles, and directory targets do not establish availability; targets
  outside the fetched repository are unverifiable for the future worktree.
  Claude uses `.claude/skills` in the repository and its personal
  configuration directory (`CLAUDE_CONFIG_DIR`, default `~/.claude`). Codex
  uses repository `.agents/skills`, `~/.agents/skills`, `/etc/codex/skills`, and
  bundled `CODEX_HOME/skills/.system` skills. Its personal `.agents` directory
  is independent of `CODEX_HOME`.
- Claude namespaced plugin skills are unverifiable until the harness reports
  loaded skills in its runtime `system/init` event.
- Codex with `CLAUDE.md` but no `AGENTS.md` produces a warning. Claude below
  `2.1.277` with `AGENTS.md` but no `CLAUDE.md` also produces a warning, alongside
  the minimum-version error. An unparseable Claude version makes that
  instruction check unverifiable.

Instruction and repository skill checks inspect the fetched base branch, not
uncommitted local files. If fetching fails, dependent checks are reported as
unverifiable; personal skills can still be checked. Instruction files also
must resolve to regular files before counting as available.

When GitHub itself cannot be reached (a transport failure, a rate limit, a
server error, or a command that outlasts its deadline), every check that needs
it reports unverifiable instead of an error, and checks that depend on the
unreachable data follow. The GitHub login check is included: it probes the API
once, because gh reports a connection failure as an invalid token. Start serves
and reconciles while the service recovers. Failures GitHub answered stay errors:
an invalid token, missing labels, denied push permission, and unresolvable base
branches are setup problems. A deleted, renamed, forbidden, or otherwise
unreadable repository is reported but does not stop `mergeyard start`: its
access findings are printed and reconciliation scopes the failure to it, so
other repositories keep working.

Doctor never changes labels, creates remote branches, logs in, invokes a model,
or alters a managed checkout. Temporary probes are removed. Git hooks are
disabled, shell repository/index overrides are cleared, existing credential
helpers and transport URL rewrites remain available, and Git never prompts:
SSH probes use batch mode with keepalive options so a missing credential or a
dead connection fails instead of hanging, unless the machine chooses its own
transport through `GIT_SSH`, `GIT_SSH_COMMAND`, or a `core.sshCommand` the
machine resolves for the probe. Probe repositories are disposable and have no
remote of their own, so repository-scoped conditional includes cannot apply to
them; a multi-account setup that relies on those should set `GIT_SSH_COMMAND`
for the daemon. Every external command has a 30-second timeout;
SIGINT/SIGTERM cancels the checks.

## Verification

`go test ./internal/doctor` tests diagnostics using faked command outputs and
real temporary filesystem/port probes. It also exercises actual Git fetch and
dry-run push against a local bare repository, verifies unchanged remote refs,
and checks that configured Git hooks never run. No test requires paid harness
calls or live GitHub writes. Real-Git skill fixtures also cover valid and
dangling symlinks, chains, symlinked directories, cycles, and non-file targets.
`go test ./cmd/mergeyard -run TestDoctor` checks
CLI output grouping and exit statuses with fake tool executables.
