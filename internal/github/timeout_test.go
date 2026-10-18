package github_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
)

func TestSuccessfulGHCommandWithLingeringHelperStillSucceeds(t *testing.T) {
	dir := t.TempDir()
	// A credential helper outlives a successful gh while holding its pipes; the
	// request itself completed, so it must not be classified as a failure.
	script := `#!/bin/sh
/bin/sleep 2 &
printf '%s' '{"number":2,"state":"closed","created_at":"2026-10-03T21:25:53Z"}'
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	start := time.Now()
	state, err := github.New(nil).IssueState(context.Background(), "owner/repo", 2)
	if err != nil || state != github.Closed {
		t.Fatalf("state = %q, err = %v", state, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("lingering helper held the client for %s", elapsed)
	}
}

func TestDeadlineEndsAHungGHCommand(t *testing.T) {
	cases := []struct {
		name string
		call func(*github.Client) error
	}{
		{"api request", func(c *github.Client) error {
			_, err := c.IssueState(context.Background(), "owner/repo", 7)
			return err
		}},
		{"ready mark", func(c *github.Client) error {
			return c.MarkReady(context.Background(), "owner/repo", 12)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec /bin/sleep 600\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			client := github.NewWithOptions(nil, github.Options{Timeout: 200 * time.Millisecond})
			start := time.Now()
			err := tc.call(client)
			requireCode(t, err, "github.timeout")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost the deadline cause: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("hung gh held the client for %s", elapsed)
			}
		})
	}
}
