# Installation

Prebuilt binaries target macOS and Linux, on Apple Silicon / ARM64 and Intel / AMD64.
They include the dashboard; Go and Node.js are not required to run them.

Install the latest release:

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
The installer source is [scripts/install.sh](../scripts/install.sh).

## Build from source

For development, use Go 1.26.10 or newer
(the module selects the Go 1.26.10 toolchain):

```sh
git clone https://github.com/rcpassos/mergeyard.git
cd mergeyard
go build -o bin/mergeyard ./cmd/mergeyard
./bin/mergeyard --help
```

The committed dashboard assets are embedded by this build. Use `./bin/mergeyard`
in place of `mergeyard` in the [quickstart](../README.md#quickstart), or add
`bin/` to your PATH.

## Runtime prerequisites

Install Git, [GitHub CLI](https://cli.github.com/), tmux, and your configured
agents. Claude Code must be at least 2.1.277 and Codex CLI at least 0.156.1
when selected. Authenticate GitHub CLI and the agents, and ensure Git can push
to the repositories you manage. `mergeyard init` checks these requirements;
run `mergeyard doctor` to check them again. See [doctor](doctor.md) for details.
