package runtimeauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// secretName is the workspace file shared by the runtime and the CLI. Its
// owner-only permissions are the boundary between the workspace owner and other
// local users.
const secretName = "runtime.secret"

const secretBytes = 32

// SecretPath returns the workspace secret location inside one workspace root.
func SecretPath(root string) string { return filepath.Join(root, secretName) }

// EnsureSecret returns the workspace secret, creating it with owner-only
// permissions when it is absent. The runtime owns creation; the CLI only reads
// the file.
func EnsureSecret(root string) ([]byte, error) {
	err := createSecret(SecretPath(root))
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return LoadSecret(root)
}

// RotateSecret replaces the workspace secret with a fresh value. The runtime
// rotates at startup so a secret read before a restart can no longer
// authenticate a client. The replacement is atomic.
func RotateSecret(root string) error {
	path := SecretPath(root)
	fail := func(action string, err error) error {
		return secretFailure("runtime.secret_unavailable", path, "could not "+action+" the workspace secret; check workspace permissions and restart Mergeyard", err)
	}
	secret, err := newSecret()
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(root, ".runtime.secret.*")
	if err != nil {
		return fail("write", err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.WriteString(secret + "\n"); err != nil {
		temp.Close()
		return fail("write", err)
	}
	if err := temp.Close(); err != nil {
		return fail("write", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fail("replace", err)
	}
	return nil
}

// LoadSecret reads and validates the workspace secret written by the owning
// runtime. It refuses files that other local users could read.
func LoadSecret(root string) ([]byte, error) {
	path := SecretPath(root)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, secretFailure("runtime.secret_missing", path, "stop any running Mergeyard for this workspace, start the current version with this configuration to create the workspace secret, then retry", err)
	}
	if err != nil {
		return nil, secretFailure("runtime.secret_unreadable", path, "fix read access for your user, then retry", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, secretFailure("runtime.secret_unreadable", path, "could not inspect the workspace secret", err)
	}
	if !info.Mode().IsRegular() {
		return nil, secretFailure("runtime.secret_insecure", path, "the workspace secret is not a regular file; remove it and restart Mergeyard", nil)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		return nil, secretFailure("runtime.secret_insecure", path, fmt.Sprintf("the workspace secret has permissions %#o and is readable by other users; run chmod 600 %s and restart Mergeyard", perm, path), nil)
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); ok && int(owner.Uid) != os.Getuid() {
		return nil, secretFailure("runtime.secret_insecure", path, "the workspace secret is owned by another user; remove it and restart Mergeyard", nil)
	}
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return nil, secretFailure("runtime.secret_unreadable", path, "could not read the workspace secret; restart Mergeyard to recreate it", err)
	}
	secret := strings.TrimSpace(string(data))
	if !validSecret(secret) {
		return nil, secretFailure("runtime.secret_insecure", path, "the workspace secret is malformed; remove it and restart Mergeyard", nil)
	}
	return []byte(secret), nil
}

// createSecret creates the secret file exclusively. It returns fs.ErrExist when
// another process already created it.
func createSecret(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	secret, err := newSecret()
	if err == nil {
		_, err = file.WriteString(secret + "\n")
	}
	if err != nil {
		file.Close()
		os.Remove(path)
		return secretFailure("runtime.secret_unavailable", path, "could not create the workspace secret; check workspace permissions and restart Mergeyard", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return secretFailure("runtime.secret_unavailable", path, "could not create the workspace secret; check workspace permissions and restart Mergeyard", err)
	}
	return nil
}

func newSecret() (string, error) {
	secret := make([]byte, secretBytes)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return "", secretFailure("runtime.secret_unavailable", "", "could not generate the workspace secret; retry", err)
	}
	return hex.EncodeToString(secret), nil
}

// validSecret accepts exactly the hex form written by the runtime.
func validSecret(secret string) bool {
	if len(secret) != hex.EncodedLen(secretBytes) {
		return false
	}
	_, err := hex.DecodeString(secret)
	return err == nil
}

func secretFailure(code, path, message string, err error) *fault.Error {
	return &fault.Error{Code: code, Path: path, Message: message, Err: err}
}
