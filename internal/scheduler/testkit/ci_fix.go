package testkit

import (
	"context"
	"os"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
)

const CiFixedReport = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Repaired failing CI","responses":[]}}'`

func CiFixScript(fix string) string {
	return `case "$*" in
 *review-*) ` + ApprovedReview + `;;
 *fix-*) ` + fix + `;;
 *) ` + SuccessfulScript + `;;
 esac`
}

func UseCIRepairScript(t *testing.T, cfg config.Config, fix string) {
	t.Helper()
	script := CiFixScript(fix)
	for _, executable := range []struct{ Path, Script string }{
		{cfg.Agents.Claude.Executable, script},
		{cfg.Agents.Codex.Executable, NativeCodexScript(script)},
	} {
		if err := os.WriteFile(executable.Path, []byte("#!/bin/sh\n"+executable.Script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

type CiDeadlineGitHub struct {
	*CiGitHub
	BeforeRead func()
}

func (f CiDeadlineGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	f.BeforeRead()
	return f.FakeGitHub.GetPullRequest(ctx, repo, n)
}
