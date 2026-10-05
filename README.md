# Mergeyard

Mergeyard turns ready GitHub issues into draft pull requests using a local coding
agent. It manages isolated Git worktrees and tmux sessions, tracks work in SQLite,
and gives you a local dashboard to monitor the queue and stop runs.

**Current stage: first independent review.** The scheduler runs Claude Code or Codex as the
implementer, followed by an independent Claude reviewer. Fix execution, CI
monitoring, merge detection, and Codex review are planned.

## Install

Prebuilt binaries target macOS and Linux, on Apple Silicon / ARM64 and Intel / AMD64.
They include the dashboard; Go and Node.js are not required to run them.

Once the first release is published:

```sh
curl -fsSL https://github.com/rcpassos/mergeyard/releases/latest/download/install.sh | sh
```

The installer needs `curl`, `tar`, and either `sha256sum` (Linux) or `shasum`
(macOS). It downloads the matching binary, verifies its SHA-256 checksum, and
installs to `~/.local/bin` without sudo. If that directory is not on your PATH,
add this line to your shell configuration (`~/.zshrc` or `~/.bashrc`) and restart
your terminal:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Check the installation:

```sh
mergeyard --version
```

Re-run the installer to upgrade. Stop the running Mergeyard process first and
restart it afterward. To install a specific release or choose a directory:

```sh
# Replace v0.1.0 with a published tag; this also supports prerelease tags.
curl -fsSL https://github.com/rcpassos/mergeyard/releases/download/v0.1.0/install.sh \
  | MERGEYARD_INSTALL_DIR="$HOME/.local/bin" sh -s -- v0.1.0
```

You can also download an archive and `checksums.txt` from
[GitHub Releases](https://github.com/rcpassos/mergeyard/releases), verify it with
`sha256sum` or `shasum -a 256`, and put the extracted `mergeyard` on your PATH.
The installer source is [scripts/install.sh](scripts/install.sh).

### Build from source

Before the first release, or for development, use Go 1.24 or newer:

```sh
git clone https://github.com/rcpassos/mergeyard.git
cd mergeyard
go build -o bin/mergeyard ./cmd/mergeyard
./bin/mergeyard --help
```

The committed dashboard assets are embedded by this build. Run commands below
with `./bin/mergeyard` if you have not added the binary to your PATH.

## Get started

Install `git`, [GitHub CLI](https://cli.github.com/), `tmux`, and the configured agents.
The doctor requires Claude Code 2.1.277 or newer and, when selected, Codex CLI
0.156.1 or newer. Authenticate GitHub CLI and the configured agents, and ensure Git can push to the repositories you want to manage.

```sh
gh auth login
mergeyard init
```

Choose Claude or Codex as the implementer and Claude as the reviewer, enter an `owner/repo`, and let setup create
the queue labels when prompted. Init writes the configuration and runs dependency,
authentication, and repository checks. Resolve any reported errors, then start:

```sh
mergeyard doctor
mergeyard start
```

Open [the local dashboard](http://127.0.0.1:7331). Add the `ready-for-agent` label
to an issue you want implemented. Mergeyard claims eligible issues, runs the configured implementer in
an isolated worktree, and creates a draft PR. An independent Claude reviewer then
reviews the pinned PR head. Approval waits for CI; blocking findings prepare the
fix phase. Both retain the draft PR and consume a concurrency slot. Stop preserves
the implemented branch, worktree, and draft PR.

Mergeyard runs agents with your local permissions. The default Claude configuration
uses `bypassPermissions` for unattended execution. Use it with repositories and
issues you trust; see [the example configuration](mergeyard.example.yaml) for
permission settings.

## Everyday commands

| Command | Purpose |
| --- | --- |
| `mergeyard init` | Set up configuration and check prerequisites |
| `mergeyard repo add owner/repo` | Add another repository |
| `mergeyard start` | Start the scheduler and dashboard; also the default command |
| `mergeyard status` | Show current runs and scheduler state |
| `mergeyard pause` / `mergeyard resume` | Disable or enable new claims |
| `mergeyard watch <run-id>` | Attach read-only to the live tmux phase |
| `mergeyard stop <run-id>` | Stop a run and keep its worktree, branch, and PR |
| `mergeyard doctor` | Check dependencies, authentication, and configuration |
| `mergeyard reconcile` | Inspect persisted work with the control plane stopped |
| `mergeyard --version` | Show the installed version |

Status, pause, resume, watch, and stop connect to the running process. Use the
same configuration for both. Ctrl-C shuts down the control plane; running tmux
phases survive and are reconciled on the next startup. `takeover`, `handback`,
`retry`, and `open` are reserved commands and are not implemented yet.

## Configuration

Configuration is selected in this order: `--config <path>`, `./mergeyard.yaml`,
then `~/.config/mergeyard/config.yaml`. Files are not merged. First-time init
creates the home configuration when no existing file or explicit path is present.
Runtime data defaults to `~/.mergeyard`.

See [mergeyard.example.yaml](mergeyard.example.yaml) for defaults and repository
overrides, [setup](docs/setup.md) for adding repositories, and
[configuration](docs/configuration.md) for the complete loading rules.

## Development and releases

```sh
make test   # Go tests, including installer tests; install tmux for integration tests
make lint   # gofmt and go vet
make build  # rebuild dashboard assets with Node.js 24, then compile Go
```

Commit generated `web/static/` files with dashboard source changes.

Pushing a version tag such as `v0.1.0` triggers tests, builds all four platform
archives, and publishes them with checksums, the installer, and release notes.
See [release management](docs/releases.md) for publishing and local release checks.

Further documentation: [CLI](docs/cli.md), [dashboard](docs/web.md),
[doctor](docs/doctor.md), [scheduler](docs/scheduler.md),
[workflow](docs/workflow.md), and [product roadmap](docs/PRD.md).
