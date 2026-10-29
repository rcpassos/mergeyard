package git_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

func TestManagersKeepConcurrentCommandEnvironmentsIsolated(t *testing.T) {
	f := newFixture(t)
	type result struct {
		index int
		run   managedgit.Run
		err   error
	}
	results := make(chan result, 2)
	managers := make([]*managedgit.Manager, 2)
	remotes := make([]string, 2)
	for i := range 2 {
		root := t.TempDir()
		remote := filepath.Join(root, "remote.git")
		git(t, root, "clone", "--bare", f.remote, remote)
		remotes[i] = remote
		configPath := filepath.Join(root, "gitconfig")
		git(t, root, "config", "--file", configPath, "url."+remote+".insteadOf", "https://github.com/owner/repo.git")
		w, err := workspace.Open(filepath.Join(root, "workspace"))
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"GIT_CONFIG_GLOBAL": configPath, "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": fmt.Sprintf("Fixture %d", i)}
		managers[i] = managedgit.NewWithOptions(w, managedgit.Options{Environment: env})
		env["GIT_CONFIG_GLOBAL"] = "/does/not/exist" // The manager owns its copy.
	}
	for i, m := range managers {
		go func() {
			run, err := m.Prepare(context.Background(), managedgit.PrepareRequest{Repository: "owner/repo", RunID: "run-1", IssueNumber: 7})
			results <- result{i, run, err}
		}()
	}
	runs := make([]managedgit.Run, 2)
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		runs[got.index] = got.run
	}
	for i, run := range runs {
		if got := git(t, run.BasePath, "remote", "get-url", "origin"); got != "https://github.com/owner/repo.git" {
			t.Fatalf("origin=%s", got)
		}
		write(t, run.Path, "feature.txt", fmt.Sprint(i))
	}
	for i, m := range managers {
		go func() {
			_, err := m.CommitAndPush(context.Background(), runs[i], managedgit.Phase{Title: "isolated environment", RequireChanges: true})
			results <- result{index: i, err: err}
		}()
	}
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
	}
	for i, remote := range remotes {
		if got := git(t, remote, "show", "-s", "--format=%an", "mergeyard/issue-7"); got != fmt.Sprintf("Fixture %d", i) {
			t.Fatalf("manager %d inherited another identity: %q", i, got)
		}
	}
}
