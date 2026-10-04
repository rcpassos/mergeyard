package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
)

func TestImplementInputWriterKeepsIssueTextOutOfInvocation(t *testing.T) {
	ctx := phase(t)
	r := runner.NewLocal(runner.Options{})
	input := harness.ImplementInput{Repository: "owner/repo", IssueNumber: 10,
		IssueTitle: "Handle quotes '$() ;", IssueBody: "Literal body: $(touch injected)\n`echo unsafe`\n--continue",
		IssueURL: "https://github.com/owner/repo/issues/10", BaseBranch: "main", Constraints: []string{"Preserve the public interface"}}
	path, err := harness.WriteImplementInput(context.Background(), r, ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(ctx.PhaseDir, "input.md") {
		t.Fatalf("input path = %q", path)
	}
	data, err := r.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{input.Repository, "10", input.IssueTitle, input.IssueBody, input.IssueURL, "main", ctx.WorktreePath,
		"Preserve the public interface", "Do not commit.", string(harness.ImplementSchema())} {
		if !strings.Contains(string(data), value) {
			t.Fatalf("input does not contain %q: %s", value, data)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("input mode = %v; error = %v", info, err)
	}
	invocation, err := harness.NewClaude(config.Claude{}).BuildInvocation(ctx, config.Role{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(invocation.Args, "\n"), input.IssueBody) || strings.Contains(strings.Join(invocation.Args, "\n"), input.IssueTitle) {
		t.Fatalf("issue text was included in command arguments: %q", invocation.Args)
	}
	ctx.Resume = true
	input.IssueBody = "Full updated context for a resumed attempt"
	if _, err := harness.WriteImplementInput(context.Background(), r, ctx, input); err != nil {
		t.Fatal(err)
	}
	data, err = r.ReadFile(context.Background(), path)
	if err != nil || !strings.Contains(string(data), input.IssueBody) || strings.Contains(string(data), "touch injected") {
		t.Fatalf("resumed input = %s; error = %v", data, err)
	}
}

func TestImplementInputWriterReportsFileFailures(t *testing.T) {
	ctx := phase(t)
	r := runner.NewLocal(runner.Options{})
	ctx.PhaseDir = filepath.Join(ctx.PhaseDir, "missing")
	_, err := harness.WriteImplementInput(context.Background(), r, ctx, harness.ImplementInput{})
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "internal.file_write_failed" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file write error = %v", err)
	}
	ctx = phase(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = harness.WriteImplementInput(cancelled, r, ctx, harness.ImplementInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write cancellation = %v", err)
	}
	ctx.PhaseDir = ctx.WorktreePath
	_, err = harness.WriteImplementInput(context.Background(), r, ctx, harness.ImplementInput{})
	assertCode(t, err, "phase.invalid_request")
	if _, err := os.Stat(filepath.Join(ctx.WorktreePath, "input.md")); !os.IsNotExist(err) {
		t.Fatalf("invalid input directory was written: %v", err)
	}
}
