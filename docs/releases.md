# Release management

Mergeyard uses [GoReleaser v2](https://goreleaser.com/) and GitHub Actions.
The release configuration is [`.goreleaser.yaml`](../.goreleaser.yaml); the
workflow is [`.github/workflows/release.yml`](../.github/workflows/release.yml).
No package registry or separate release credentials are needed: the workflow
uses its repository-scoped `GITHUB_TOKEN` with `contents: write`.

## Publish a release

Merge the intended changes into `main` and wait for CI to pass. From a clean,
up-to-date checkout of `main`, create and push a semantic version tag:

```sh
git switch main
git pull --ff-only
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

Replace `v0.1.0` with the next version. Tags are the version source; there is
no version file to update. GoReleaser embeds the version in the binary, available
through `mergeyard version` and `mergeyard --version`. Ordinary local builds show
`dev`.

The workflow runs lint and tests (with tmux installed), rebuilds the dashboard,
and verifies that the generated assets match the committed files before
publishing. Release notes are generated from commits since the previous tag.
All release assets are uploaded to a draft before GoReleaser publishes it.

Each release contains:

- `mergeyard_darwin_arm64.tar.gz` and `mergeyard_darwin_amd64.tar.gz`;
- `mergeyard_linux_arm64.tar.gz` and `mergeyard_linux_amd64.tar.gz`;
- `checksums.txt` with SHA-256 hashes;
- `install.sh`, the same installer as `scripts/install.sh` in that tag.

Archives include the binary, README, example configuration, documentation, and
embedded asset license notices. Builds disable CGO, so Linux binaries do not
require a particular system libc. Native Windows is not a release target.

The README's latest-release install command becomes available after the first
non-prerelease is published. Before then, use the source build instructions.

## Prereleases

Use a tag such as `v0.2.0-rc.1`. GoReleaser marks it as a prerelease; install it
explicitly so ordinary installs continue to use the latest stable release:

```sh
curl -fsSL https://github.com/rcpassos/mergeyard/releases/download/v0.2.0-rc.1/install.sh \
  | sh -s -- v0.2.0-rc.1
```

## Validate locally

Install GoReleaser v2, Go from `go.mod`, Node.js 24, and tmux, then run:

```sh
make lint
make test
make assets
git diff --exit-code -- web/static
goreleaser check
goreleaser release --snapshot --clean
```

Snapshot mode builds archives and checksums in the ignored `dist/` directory
without publishing or requiring a release tag. It uses the committed dashboard
assets, so regenerate them first when changing the UI. CI also builds snapshots
on pull requests to catch packaging problems before tagging.

Installer tests run as part of `go test ./...`. They use local release fixtures
and a fake downloader to exercise platform selection, versioned URLs, upgrades,
and failures without contacting GitHub or modifying a real installation.

## Upgrade and recovery

Stop the control plane before replacing its binary and start it afterward so
the new version opens the runtime. Re-running the installer upgrades in place.
It verifies the archive checksum before extracting and replaces the executable
only after staging the download successfully. Config and runtime data are kept.

The same explicit-tag install command can select an older binary, but database
migrations may make runtime data incompatible with an older version. Back up
the workspace while the control plane is stopped before upgrades that change
the storage schema; consult the release notes before downgrading.

If a workflow fails before publishing, fix the cause and rerun it if the tag's
source does not need changes. If code must change, publish a new version rather
than moving a published tag. A checksum failure leaves the existing executable
untouched; retry using an explicit tag and inspect the release assets if it
persists.
