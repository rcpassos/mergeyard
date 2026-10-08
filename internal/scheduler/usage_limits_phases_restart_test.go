package scheduler_test

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReviewAndFixUsageLimitsSurviveControlPlaneKill(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				boundaries := []string{"interruption", "waiting", "before-relaunch", "resuming"}
				if phase == workflow.Review {
					boundaries = append(boundaries, "before-restoration", "restoration")
				}
				for _, boundary := range boundaries {
					t.Run(implementer+"/"+reviewer+"/"+string(phase)+"/"+boundary, func(t *testing.T) {
						s, initial, api, remote, cfg, _ := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
						if phase == workflow.Review {
							replacePhaseScript(t, cfg, phase, reviewScript(`case "$*" in *review-1-1*) printf contaminated > feature.txt; printf reviewer > reviewer.txt; git add feature.txt; git -c user.name=Reviewer -c user.email=reviewer@example.invalid commit -m 'Reviewer edit' >&2; exit 1;; esac
`+approvedReview))
						} else {
							replacePhaseScript(t, cfg, phase, loopScript(`case "$*" in *fix-1-1*) printf partial > partial.txt; exit 1;; esac
test "$(cat partial.txt)" = partial || exit 9
printf fixed > feature.txt
`+fixedReport))
						}
						run := finish(t, s, workflow.Active, phase)
						root := initial.Workspace.Root
						if err := initial.Close(); err != nil {
							t.Fatal(err)
						}
						socket := fmt.Sprintf("mergeyard-phase-limit-%d", time.Now().UnixNano())
						t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
						now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
						agent := reviewer
						if phase == workflow.Fix {
							agent = implementer
						}
						killUsageControlPlane(t, usageProcessInput{Config: cfg, Workspace: root, Socket: socket, Agent: agent, Boundary: boundary, Now: now, Phase: phase, PR: api.prs["mergeyard/issue-7"], Remote: remote})
						runtime, err := app.Open(context.Background(), root)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { runtime.Close() })
						fake := classifiedAdapter(agent, harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
						restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: runner.NewLocal(runner.Options{SocketName: socket}), Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
						if err != nil {
							t.Fatal(err)
						}
						if boundary != "resuming" && boundary != "before-relaunch" {
							waiting := finish(t, restarted, workflow.WaitingForHarness, phase)
							if len(waiting.HarnessWaitHistory) != 1 || waiting.ReviewRound != 1 || (phase == workflow.Review && (!waiting.Review.Restored || !waiting.Review.Contaminated || waiting.Review.Accepted || waiting.Review.Report != nil)) {
								t.Fatalf("interruption/restoration lost: %+v", waiting)
							}
							for range 2 {
								if _, err := restarted.Reconcile(context.Background()); err != nil {
									t.Fatal(err)
								}
							}
							resumed, err := runtime.Workflow.Get(context.Background(), run.ID)
							if err != nil || resumed.State != workflow.WaitingForHarness || len(resumed.HarnessWaitHistory) != 1 {
								t.Fatalf("waiting advanced early: %+v %v", resumed, err)
							}
							now = waiting.HarnessWait.ResetAt
						} else {
							saved, err := runtime.Workflow.Get(context.Background(), run.ID)
							if err != nil || saved.HarnessWait == nil {
								t.Fatalf("relaunch lost its wait: %+v %v", saved, err)
							}
							now = saved.HarnessWait.ResetAt
						}
						state := workflow.WaitingForCI
						if boundary == "before-relaunch" {
							state = workflow.NeedsAttention
						}
						finishPhase := workflow.Review
						if state == workflow.NeedsAttention {
							finishPhase = phase
						}
						done := finish(t, restarted, state, finishPhase)
						if len(done.HarnessWaitHistory) != 1 || api.creations != 1 || len(done.SessionRecoveries) != 0 {
							t.Fatalf("duplicated execution/publication: %+v", done)
						}
						if phase == workflow.Review && (len(done.ReviewHistory) != 2 || done.Review.Attempt != 2 || done.Review.SessionID != done.ReviewHistory[0].SessionID || !done.Review.Restored) {
							t.Fatalf("review continuity lost: %+v", done.ReviewHistory)
						}
						if phase == workflow.Fix && (len(done.FixHistory) != 2 || done.Fix.Attempt != 2 || done.Fix.SessionID != run.Implementer.SessionID || done.Implementer.SessionID != run.Implementer.SessionID) {
							t.Fatalf("fix continuity lost: %+v", done.FixHistory)
						}
						if boundary == "before-relaunch" {
							if done.LastErrorCode != "phase.session_missing" || done.ApprovedSHA != "" || done.ReviewRound != 1 {
								t.Fatalf("ambiguous launch replayed: %+v", done)
							}
						} else if done.ApprovedSHA != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || (phase == workflow.Fix && done.ReviewRound != 2) {
							t.Fatalf("approved wrong target or consumed a round: %+v", done)
						}
						for range 2 {
							if _, err := restarted.Reconcile(context.Background()); err != nil {
								t.Fatal(err)
							}
						}
						if err := restarted.Stop(context.Background(), done.ID); err != nil {
							t.Fatal(err)
						}
					})
				}
			}
		}
	}
}
