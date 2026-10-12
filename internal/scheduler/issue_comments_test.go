package scheduler_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestEveryPhaseAttemptFetchesIssueComments(t *testing.T) {
	s, rt, api, _, _, _ := pairingFlow(t, "claude", "codex", `printf fixed > feature.txt; `+fixedReport)
	fetches := 0
	api.comments = func(repo string, n int, trusted []string) (github.IssueComments, error) {
		if repo != "owner/repo" || n != 7 {
			t.Fatalf("wrong issue: %s #%d", repo, n)
		}
		fetches++
		return github.IssueComments{Comments: []github.Comment{{ID: int64(fetches), User: &github.Actor{Login: "maintainer"}, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Body: fmt.Sprintf("Clarification for attempt %d", fetches)}}, OmittedUntrusted: 2}, nil
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	for i, phase := range []string{"implement-0-1", "review-1-1", "fix-1-1", "review-2-1"} {
		path := filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", phase, "input.md")
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), fmt.Sprintf("Clarification for attempt %d", i+1)) || !strings.Contains(string(data), "2 untrusted comments omitted") {
			t.Fatalf("%s did not receive fresh comments: %s %v", phase, data, err)
		}
		if i > 1 && !strings.Contains(string(data), "New trusted issue comments since the previous input: 1") {
			t.Fatalf("%s did not notify resumed role of newly fetched comments", phase)
		}
	}
	if fetches != 4 {
		t.Fatalf("comment fetches = %d, want one per attempt", fetches)
	}
}

func TestRetryAfterBlockedRefreshesCommentsAcrossPhasesAndHarnesses(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, p := range []workflow.Phase{workflow.Implement, workflow.Fix, workflow.Review} {
			t.Run(agent+"/"+string(p), func(t *testing.T) {
				fix := `printf fixed > feature.txt; ` + fixedReport
				_, rt, api, _, cfg, r := pairingFlow(t, agent, agent, fix)
				script := loopScript(fix)
				switch p {
				case workflow.Implement:
					script = strings.Replace(script, successfulScript, `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":2,"status":"blocked","summary":"Need an answer",`+blockedAction+`}}'`, 1)
				case workflow.Review:
					script = strings.Replace(script, changesReview, `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"status":"blocked","summary":"Need an answer",`+blockedAction+`,"findings":[],"prior_findings":[]}}'`, 1)
				case workflow.Fix:
					script = loopScript(`printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":2,"status":"blocked","summary":"Need an answer",` + blockedAction + `,"responses":[]}}'`)
				}
				replacePhaseScript(t, cfg, p, script)
				cfg.Repositories[0].TrustedLabelers = []string{"maintainer"}
				comments := []github.Comment{{ID: 1, User: &github.Actor{Login: "maintainer"}, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Body: "Original clarification"}}
				api.comments = func(_ string, _ int, trusted []string) (github.IssueComments, error) {
					if len(trusted) != 1 || trusted[0] != "maintainer" {
						t.Fatalf("comment policy differs from configured labelers: %v", trusted)
					}
					return github.IssueComments{Comments: comments}, nil
				}
				deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r}
				s, err := scheduler.New(cfg, schedulerResources(rt), deps)
				if err != nil {
					t.Fatal(err)
				}
				before := finish(t, s, workflow.NeedsAttention, p)
				if before.LastErrorCode != "phase.blocked" {
					t.Fatalf("fixture did not block: %+v", before)
				}
				answer := "Answer posted after block: reject duplicates; $(touch injected)\n```sh\n`id`\n```"
				comments = append(comments, github.Comment{ID: 2, User: &github.Actor{Login: "maintainer"}, CreatedAt: comments[0].CreatedAt, Body: answer})
				replacePhaseScript(t, cfg, p, loopScript(fix))
				if _, err := s.Retry(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				// Restart between Retry and execution to exercise durable sessions.
				s, err = scheduler.New(cfg, schedulerResources(rt), deps)
				if err != nil {
					t.Fatal(err)
				}
				finish(t, s, workflow.WaitingForCI, workflow.Review)
				var path string
				var resumed bool
				if err := rt.DB.QueryRow("SELECT input_path,resumed_session FROM phase_attempts WHERE run_id=? AND phase=? AND attempt=2", before.ID, p).Scan(&path, &resumed); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil || !resumed || !strings.Contains(string(data), answer) || !strings.Contains(string(data), "## Interruption context") || !strings.Contains(string(data), "New trusted issue comments since the previous input: 1") {
					t.Fatalf("blocked retry did not resume with the answer: resumed=%v %s %v", resumed, data, err)
				}
			})
		}
	}
}
