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
