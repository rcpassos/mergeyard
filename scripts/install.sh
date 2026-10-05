#!/bin/sh
set -eu

# Keep the body in a function so a truncated curl | sh download cannot run it.
install_mergeyard() {
    fail() { printf 'mergeyard: %s\n' "$*" >&2; exit 1; }

    [ "$#" -le 1 ] || fail 'usage: install.sh [vX.Y.Z]'
    version=${1:-latest}
    case "$version" in
        latest) ;;
        v[0-9]*)
            case "$version" in *[!a-zA-Z0-9.+-]*) fail 'invalid release tag' ;; esac
            ;;
        *) fail 'expected a release tag such as v0.1.0' ;;
    esac

    case "$(uname -s)" in
        Darwin) os=darwin ;;
        Linux) os=linux ;;
        *) fail 'supported operating systems: macOS and Linux' ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) fail 'supported architectures: amd64 and arm64' ;;
    esac
    for tool in curl tar mktemp awk; do
        command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool"
    done
    if command -v sha256sum >/dev/null 2>&1; then
        hasher=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        hasher=shasum
    else
        fail 'install sha256sum or shasum to verify downloads'
    fi

    install_dir=${MERGEYARD_INSTALL_DIR:-"$HOME/.local/bin"}
    # Absolute paths also prevent a leading dash from becoming a tool option.
    case "$install_dir" in /*) ;; *) fail 'MERGEYARD_INSTALL_DIR must be an absolute path' ;; esac
    [ ! -d "$install_dir/mergeyard" ] || fail "$install_dir/mergeyard is a directory"
    base=https://github.com/rcpassos/mergeyard/releases
    if [ "$version" = latest ]; then
        base=$base/latest/download
    else
        base=$base/download/$version
    fi
    archive=mergeyard_${os}_${arch}.tar.gz
    tmp=$(mktemp -d)
    staged=
    trap 'rm -rf "$tmp"; if [ -n "$staged" ]; then rm -f "$staged"; fi' 0
    trap 'exit 1' HUP INT TERM

    printf 'Downloading Mergeyard (%s, %s/%s)...\n' "$version" "$os" "$arch"
    curl --proto '=https' --tlsv1.2 -fsSL "$base/$archive" -o "$tmp/$archive" ||
        fail 'download failed; check the release tag and published assets'
    curl --proto '=https' --tlsv1.2 -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" ||
        fail 'could not download release checksums'
    expected=$(awk -v name="$archive" '$2 == name {print $1}' "$tmp/checksums.txt")
    [ "${#expected}" -eq 64 ] || fail 'missing or invalid archive checksum'
    case "$expected" in *[!a-fA-F0-9]*) fail 'invalid archive checksum' ;; esac
    if [ "$hasher" = sha256sum ]; then
        digest=$(sha256sum "$tmp/$archive")
    else
        digest=$(shasum -a 256 "$tmp/$archive")
    fi
    actual=${digest%% *}
    [ "$actual" = "$expected" ] || fail 'checksum mismatch; existing installation was not changed'

    tar -xzf "$tmp/$archive" -C "$tmp" mergeyard
    [ -f "$tmp/mergeyard" ] && [ ! -L "$tmp/mergeyard" ] || fail 'archive does not contain a regular mergeyard binary'
    mkdir -p "$install_dir"
    staged=$(mktemp "$install_dir/.mergeyard.XXXXXX")
    cp "$tmp/mergeyard" "$staged"
    chmod 755 "$staged"
    mv -f "$staged" "$install_dir/mergeyard"
    staged=
    printf 'Installed %s/mergeyard\n' "$install_dir"
    case ":${PATH:-}:" in
        *":$install_dir:"*) ;;
        *) printf 'Add %s to your PATH, then open a new terminal.\n' "$install_dir" ;;
    esac
    printf 'Run mergeyard --version, then mergeyard init to get started.\n'
}

install_mergeyard "$@"
