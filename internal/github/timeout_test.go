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

func TestRequestDeadlineEndsAHungGHCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec /bin/sleep 600\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	client := github.NewWithOptions(nil, github.Options{Timeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := client.IssueState(context.Background(), "owner/repo", 7)
	requireCode(t, err, "github.timeout")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost the deadline cause: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hung gh held the client for %s", elapsed)
	}
}

func TestRequestDeadlineEndsAHungReadyMark(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec /bin/sleep 600\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	client := github.NewWithOptions(nil, github.Options{Timeout: 200 * time.Millisecond})
	start := time.Now()
	err := client.MarkReady(context.Background(), "owner/repo", 12)
	requireCode(t, err, "github.timeout")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hung gh held the ready mark for %s", elapsed)
	}
}
