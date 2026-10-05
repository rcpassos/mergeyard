package scripts_test

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstaller(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch, platform, tag, failure, wantError string
	}{
		{name: "linux amd64 latest", os: "Linux", arch: "x86_64", platform: "linux_amd64"},
		{name: "linux arm64 pinned", os: "Linux", arch: "aarch64", platform: "linux_arm64", tag: "v0.1.0"},
		{name: "mac arm64 prerelease", os: "Darwin", arch: "arm64", platform: "darwin_arm64", tag: "v0.2.0-rc.1"},
		{name: "mac amd64", os: "Darwin", arch: "x86_64", platform: "darwin_amd64"},
		{name: "checksum mismatch", os: "Linux", arch: "x86_64", platform: "linux_amd64", failure: "checksum", wantError: "checksum mismatch"},
		{name: "missing checksum", os: "Linux", arch: "x86_64", platform: "linux_amd64", failure: "missing-checksum", wantError: "missing or invalid archive checksum"},
		{name: "download failure", os: "Linux", arch: "x86_64", platform: "linux_amd64", failure: "download", wantError: "download failed"},
		{name: "unsupported os", os: "FreeBSD", arch: "x86_64", wantError: "supported operating systems"},
		{name: "unsupported arch", os: "Linux", arch: "riscv64", wantError: "supported architectures"},
		{name: "invalid tag", os: "Linux", arch: "x86_64", tag: "v1/../../bad", wantError: "invalid release tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "tools")
			fixtures := filepath.Join(root, "releases")
			installDir := filepath.Join(root, "install with spaces")
			tmp := filepath.Join(root, "tmp")
			for _, dir := range []string{bin, fixtures, installDir, tmp} {
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			oldBinary := "previous installation\n"
			newBinary := "#!/bin/sh\nprintf 'mergeyard test-version\\n'\n"
			write(filepath.Join(installDir, "mergeyard"), oldBinary, 0755)
			archive := "mergeyard_" + tc.platform + ".tar.gz"
			var data strings.Builder
			gz := gzip.NewWriter(&data)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "mergeyard", Mode: 0755, Size: int64(len(newBinary))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(newBinary)); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(fixtures, archive), data.String(), 0644)
			sum := fmt.Sprintf("%x", sha256.Sum256([]byte(data.String())))
			if tc.failure == "checksum" {
				sum = strings.Repeat("0", 64)
			}
			manifest := sum + "  " + archive + "\n"
			if tc.failure == "missing-checksum" {
				manifest = sum + "  another.tar.gz\n"
			}
			write(filepath.Join(fixtures, "checksums.txt"), manifest, 0644)
			write(filepath.Join(bin, "uname"), "#!/bin/sh\ncase $1 in -s) echo \"$TEST_OS\" ;; -m) echo \"$TEST_ARCH\" ;; esac\n", 0755)
			write(filepath.Join(bin, "curl"), `#!/bin/sh
set -eu
[ "$TEST_FAILURE" != download ] || exit 22
url=
out=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --proto|--tls-max) shift ;;
        -o) shift; out=$1 ;;
        https://*) url=$1 ;;
    esac
    shift
done
printf '%s\n' "$url" >> "$TEST_URL_LOG"
cp "$TEST_FIXTURES/${url##*/}" "$out"
`, 0755)
			args := []string{"install.sh"}
			if tc.tag != "" {
				args = append(args, tc.tag)
			}
			cmd := exec.Command("sh", args...)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "MERGEYARD_INSTALL_DIR="+installDir,
				"TMPDIR="+tmp, "TEST_OS="+tc.os, "TEST_ARCH="+tc.arch, "TEST_FAILURE="+tc.failure,
				"TEST_FIXTURES="+fixtures, "TEST_URL_LOG="+filepath.Join(root, "urls"))
			output, err := cmd.CombinedOutput()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(string(output), tc.wantError) {
					t.Fatalf("error = %v, output = %s; want %s", err, output, tc.wantError)
				}
			} else if err != nil {
				t.Fatalf("install: %v\n%s", err, output)
			}
			installed, err := os.ReadFile(filepath.Join(installDir, "mergeyard"))
			if err != nil {
				t.Fatal(err)
			}
			want := newBinary
			if tc.wantError != "" {
				want = oldBinary
			}
			if string(installed) != want {
				t.Fatalf("installed = %q, want %q", installed, want)
			}
			if tc.wantError == "" {
				output, err := exec.Command(filepath.Join(installDir, "mergeyard"), "--version").CombinedOutput()
				if err != nil || string(output) != "mergeyard test-version\n" {
					t.Fatalf("installed executable: %v %s", err, output)
				}
				prefix := "https://github.com/rcpassos/mergeyard/releases/latest/download/"
				if tc.tag != "" {
					prefix = "https://github.com/rcpassos/mergeyard/releases/download/" + tc.tag + "/"
				}
				urls, err := os.ReadFile(filepath.Join(root, "urls"))
				if err != nil || string(urls) != prefix+archive+"\n"+prefix+"checksums.txt\n" {
					t.Fatalf("release URLs: %v %s", err, urls)
				}
			}
			for _, dir := range []string{tmp, installDir} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if dir == tmp || entry.Name() != "mergeyard" {
						t.Errorf("temporary file not cleaned up: %s", filepath.Join(dir, entry.Name()))
					}
				}
			}
		})
	}
}
