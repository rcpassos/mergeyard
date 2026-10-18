package runtimeauth_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
)

func TestSecretIsCreatedOwnerOnlyAndReused(t *testing.T) {
	root := t.TempDir()
	secret, err := runtimeauth.EnsureSecret(root)
	if err != nil || len(secret) == 0 {
		t.Fatalf("ensure: %q %v", secret, err)
	}
	path := runtimeauth.SecretPath(root)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions = %v %v", info.Mode(), err)
	}
	again, err := runtimeauth.EnsureSecret(root)
	if err != nil || string(again) != string(secret) {
		t.Fatalf("ensure replaced the secret: %q %v", again, err)
	}
	loaded, err := runtimeauth.LoadSecret(root)
	if err != nil || string(loaded) != string(secret) {
		t.Fatalf("load: %q %v", loaded, err)
	}
}

func TestRotateReplacesTheSecretAtomically(t *testing.T) {
	root := t.TempDir()
	before, err := runtimeauth.EnsureSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeauth.RotateSecret(root); err != nil {
		t.Fatal(err)
	}
	after, err := runtimeauth.LoadSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("rotation kept the previous secret")
	}
	info, err := os.Stat(runtimeauth.SecretPath(root))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("rotated permissions = %v %v", info.Mode(), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".runtime.secret.") {
			t.Fatalf("rotation left a temporary file: %s", entry.Name())
		}
	}
}

func TestLoadSecretRefusesFilesOtherUsersCanRead(t *testing.T) {
	root := t.TempDir()
	path := runtimeauth.SecretPath(root)
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runtimeauth.LoadSecret(root)
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "runtime.secret_insecure" {
		t.Fatalf("shared secret accepted: %v", err)
	}
	for _, want := range []string{"chmod 600", path} {
		if !strings.Contains(failure.Error(), want) {
			t.Fatalf("guidance %q missing from %v", want, failure)
		}
	}
}

func TestLoadSecretMissingFileTellsTheUserHowToCreateIt(t *testing.T) {
	root := t.TempDir()
	_, err := runtimeauth.LoadSecret(root)
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "runtime.secret_missing" {
		t.Fatalf("missing secret accepted: %v", err)
	}
	if !strings.Contains(failure.Message, "stop any running Mergeyard") || !strings.Contains(failure.Message, "create the workspace secret") {
		t.Fatalf("missing guidance: %v", failure)
	}
}

func TestLoadSecretRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(strings.Repeat("b", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, runtimeauth.SecretPath(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeauth.LoadSecret(root); err == nil {
		t.Fatal("symlinked secret accepted")
	}
}
