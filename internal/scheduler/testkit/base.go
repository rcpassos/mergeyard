package testkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/ci"
)

func AdvanceBase(t *testing.T, remote, name, content string) string {
	t.Helper()
	seed := filepath.Join(filepath.Dir(remote), "seed")
	if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	GitCommand(t, seed, "add", ".")
	GitCommand(t, seed, "commit", "-m", "advance base")
	GitCommand(t, seed, "push", "origin", "main")
	return GitCommand(t, seed, "rev-parse", "HEAD")
}

func PassingBaseAPI(base *FakeGitHub) *CiGitHub {
	return &CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
}
