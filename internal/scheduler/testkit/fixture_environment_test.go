package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitConfigRequiresRegisteredFixture(t *testing.T) {
	global := filepath.Join(t.TempDir(), "developer.gitconfig")
	original := []byte("[user]\n name = preserved\n")
	if err := os.WriteFile(global, original, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestMissingGitConfigProcess$")
	cmd.Env = append(os.Environ(), "MERGEYARD_TEST_MISSING_GIT_CONFIG=1", "GIT_CONFIG_GLOBAL="+global)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "no isolated Git configuration registered") {
		t.Fatalf("unregistered config was accepted: %v\n%s", err, output)
	}
	content, err := os.ReadFile(global)
	if err != nil || string(content) != string(original) {
		t.Fatalf("developer configuration changed: %q, %v", content, err)
	}
	RegisterGitConfig(t, filepath.Join(t.TempDir(), "owned.gitconfig"))
	t.Run("child inherits only the registered fixture", func(t *testing.T) {
		if got := TestGitConfig(t); got == global || !strings.HasSuffix(got, "owned.gitconfig") {
			t.Fatalf("wrong fixture config: %s", got)
		}
	})
}

func TestMissingGitConfigProcess(t *testing.T) {
	if os.Getenv("MERGEYARD_TEST_MISSING_GIT_CONFIG") != "1" {
		return
	}
	_ = TestGitConfig(t)
}
