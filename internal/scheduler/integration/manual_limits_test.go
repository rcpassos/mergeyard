package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHandbackIntoBlockedImplementWait(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
			t.Run(agent+"/"+string(kind), func(t *testing.T) {

				_, rt, api, remote, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `printf partial > partial.txt; exit 1`)
				now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
				h := testkit.ManualLimitHarness(cfg, agent, kind)
				deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
				if err != nil {
					t.Fatal(err)
				}
				initial := workflow.WaitingForHarness
				reason := "temporary_limit"
				if kind == harness.CreditsExhausted {
					initial, reason = workflow.NeedsAttention, "credits_exhausted"
				}
				before := testkit.Finish(t, s, initial, workflow.Implement)
				ctx := context.Background()
				prepared, err := s.Takeover(ctx, before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(testkit.RunWorktree(t, rt.Workspace.Root, prepared), "manual.txt"), []byte("manual work"), 0600); err != nil {
					t.Fatal(err)
				}
				selected, err := s.Handback(ctx, before.ID)
				if err != nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Implement || selected.HarnessWait == nil || selected.HarnessWait.Reason != reason || selected.Handbacks[0].NextState != workflow.WaitingForHarness {
					t.Fatalf("handback should accept published work into waiting: %+v %v", selected, err)
				}
				if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:manual.txt") != "manual work" {
					t.Fatal("manual work not published")
				}
				if _, err := s.Handback(ctx, before.ID); err != nil {
					t.Fatal(err)
				}
				root := rt.Workspace.Root
				if err := rt.Close(); err != nil {
					t.Fatal(err)
				}
				rt, err = app.Open(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				defer rt.Close()
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := s.Tick(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if len(h.Phases) != 1 {
					t.Fatalf("launched before recovery: %d", len(h.Phases))
				}
				testkit.ReplacePhaseScript(t, cfg, workflow.Fix, testkit.SuccessfulScript)
				if kind == harness.TemporaryLimit {
					now = before.HarnessWait.ResetAt
				} else {
					now = now.Add(365 * 24 * time.Hour)
					if err := s.Tick(ctx); err != nil {
						t.Fatal(err)
					}
					if len(h.Phases) != 1 {
						t.Fatal("manual work or time implicitly cleared credits")
					}
					if _, err := s.Retry(ctx, before.ID); err != nil {
						t.Fatal(err)
					}
				}
				after := testkit.Finish(t, s, workflow.Active, workflow.Review)
				if after.Implementer.Attempt != 2 || after.Implementer.SessionID != before.Implementer.SessionID || len(h.Phases) != 2 || len(after.Handbacks) != 1 || len(after.HarnessWaitHistory) != 2 {
					t.Fatalf("lost continuation: %+v launches=%d", after, len(h.Phases))
				}
				if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:manual.txt") != "manual work" {
					t.Fatal("resumption overwrote manual work")
				}
			})
		}
	}
}

func TestHandbackBlockedReviewerPreservesGrantAndMixedRoles(t *testing.T) {

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
				t.Run(implementer+"/"+reviewer+"/"+string(kind), func(t *testing.T) {

					_, rt, api, remote, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
					cfg.MaxRounds, cfg.Concurrency, cfg.Repositories[0].Concurrency = 1, 2, 2
					now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
					h := testkit.ManualLimitHarness(cfg, reviewer, kind)
					deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{reviewer: h}}
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
					if before.LastErrorCode != "review.max_rounds_exceeded" {
						t.Fatalf("fixture: %+v", before)
					}
					ctx := context.Background()
					prepared, err := s.Takeover(ctx, before.ID)
					if err != nil {
						t.Fatal(err)
					}
					api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
					testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(`printf tamper > feature.txt; printf reviewer > reviewer-only.txt; exit 1`))
					var blocker workflow.Run
					testkit.WaitForWithin(t, 30*time.Second, func() bool {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
						runs, err := s.Runs(ctx)
						if err != nil {
							t.Fatal(err)
						}
						for _, run := range runs {
							if run.IssueNumber == 8 {
								blocker = run
							}
						}
						return blocker.HarnessWait != nil
					})
					if blocker.Review == nil || !blocker.Review.Restored {
						t.Fatalf("reviewer was not restored: %+v", blocker)
					}
					if err := s.Stop(ctx, blocker.ID); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(testkit.RunWorktree(t, rt.Workspace.Root, prepared), "feature.txt"), []byte("manual repair"), 0600); err != nil {
						t.Fatal(err)
					}
					// Interrupt the final selection transaction after publication. The
					// rollback must retain one pending intent and no spent round grant.
					if _, err := rt.DB.Exec(`CREATE TRIGGER fail_blocked_handback BEFORE INSERT ON events WHEN NEW.type='run.handed_back' BEGIN SELECT RAISE(ABORT,'interrupted blocked handback'); END`); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Handback(ctx, before.ID); err == nil {
						t.Fatal("selection fault was not reached")
					}
					pending, err := rt.Workflow.Get(ctx, before.ID)
					if err != nil || pending.State != workflow.Manual || pending.ReviewRound != 1 || pending.PendingHandback() == nil || pending.PendingHandback().GrantedRound != 2 {
						t.Fatalf("lost pending round grant: %+v %v", pending, err)
					}
					if _, err := rt.DB.Exec("DROP TRIGGER fail_blocked_handback"); err != nil {
						t.Fatal(err)
					}
					rootBeforeSelection := rt.Workspace.Root
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
					rt, err = app.Open(ctx, rootBeforeSelection)
					if err != nil {
						t.Fatal(err)
					}
					defer rt.Close()
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.Reconcile(ctx); err != nil {
						t.Fatal(err)
					}
					selected, err := s.Handback(ctx, before.ID)
					if err != nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Review || selected.ReviewRound != 2 || selected.ApprovedSHA != "" || selected.HarnessWait.Harness != reviewer || selected.HarnessWait.Round != 2 || selected.Handbacks[0].GrantedRound != 2 {
						t.Fatalf("blocked review handback: %+v %v", selected, err)
					}
					server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
					if err != nil {
						t.Fatal(err)
					}
					guidance := "Automatic resumption at reset"
					if kind == harness.CreditsExhausted {
						guidance = "Explicit recovery is required"
					}
					page := httptest.NewRecorder()
					server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+before.ID, nil))
					if page.Code != 200 || !strings.Contains(page.Body.String(), guidance) || !strings.Contains(page.Body.String(), "Manual conversation success does not clear the harness block") || !strings.Contains(page.Body.String(), "Selected Waiting for harness · Review, round") || !strings.Contains(page.Body.String(), "Granted additional review round: 2") || strings.Contains(page.Body.String(), ">Retry run<") != (kind == harness.CreditsExhausted) {
						t.Fatalf("blocked handback actions: %d %s", page.Code, page.Body.String())
					}
					for range 2 {
						if _, err := s.Handback(ctx, before.ID); err != nil {
							t.Fatal(err)
						}
					}
					root := rt.Workspace.Root
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
					rt, err = app.Open(ctx, root)
					if err != nil {
						t.Fatal(err)
					}
					defer rt.Close()
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					launches := len(h.Phases)
					for range 2 {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
					}
					if len(h.Phases) != launches {
						t.Fatal("blocked handback launched on restart")
					}
					testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(testkit.ApprovedAfterFixReview))
					if kind == harness.TemporaryLimit {
						now = selected.HarnessWait.ResetAt
					} else {
						now = now.Add(365 * 24 * time.Hour)
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
						if len(h.Phases) != launches {
							t.Fatal("credit wait resumed automatically")
						}
						if _, err := s.Retry(ctx, before.ID); err != nil {
							t.Fatal(err)
						}
					}
					var done workflow.Run
					testkit.WaitForWithin(t, 30*time.Second, func() bool {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
						done, err = rt.Workflow.Get(ctx, before.ID)
						if err != nil {
							t.Fatal(err)
						}
						return done.State == workflow.WaitingForCI
					})
					if done.ReviewRound != 2 || done.Review.Attempt != 1 || done.Review.SessionID != before.Review.SessionID || done.Review.SessionID == done.Implementer.SessionID || len(done.Handbacks) != 1 || done.Implementer.SessionID != before.Implementer.SessionID || done.ApprovedSHA != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") {
						t.Fatalf("lost independent review/grant: %+v", done)
					}
					if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "2" {
						t.Fatalf("duplicate manual commit: %s", got)
					}
					history, err := rt.Events.History(ctx, 0, 200)
					if err != nil {
						t.Fatal(err)
					}
					handed := 0
					for _, event := range history {
						if event.RunID == before.ID && event.Type == "run.handed_back" {
							handed++
						}
					}
					if handed != 1 {
						t.Fatalf("duplicate handback/grant events: %d", handed)
					}
				})
			}
		}
	}
}

func TestTakeoverActiveCreditProbeRetainsBlockAndRestoresReviewer(t *testing.T) {

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {

					_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
					agent := implementer
					script := testkit.LoopScript(`printf partial > partial.txt; exit 1`)
					replace := workflow.Fix
					if phase == workflow.Implement {
						script = `printf partial > partial.txt; exit 1`
					}
					if phase == workflow.Review {
						agent, replace = reviewer, workflow.Review
						script = testkit.ReviewScript(`printf tamper > feature.txt; printf reviewer > reviewer-only.txt; exit 1`)
					}
					testkit.ReplacePhaseScript(t, cfg, replace, script)
					h := testkit.ManualLimitHarness(cfg, agent, harness.CreditsExhausted)
					deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					before := testkit.Finish(t, s, workflow.NeedsAttention, phase)
					ctx := context.Background()
					if _, err := s.Retry(ctx, before.ID); err != nil {
						t.Fatal(err)
					}
					script = `printf probe > probe-partial.txt; sleep 60`
					if phase == workflow.Review {
						script = testkit.ReviewScript(`printf tamper > feature.txt; printf reviewer > reviewer-only.txt; sleep 60`)
					}
					testkit.ReplacePhaseScript(t, cfg, replace, script)
					path := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID)
					marker := "probe-partial.txt"
					if phase == workflow.Review {
						marker = "reviewer-only.txt"
					}
					testkit.WaitForWithin(t, 30*time.Second, func() bool {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
						_, err := os.Stat(filepath.Join(path, marker))
						return err == nil
					})
					ref, err := s.Watch(ctx, before.ID)
					if err != nil {
						t.Fatal(err)
					}
					prepared, err := s.Takeover(ctx, before.ID)
					if err != nil {
						t.Fatal(err)
					}
					manual, err := rt.Workflow.Get(ctx, before.ID)
					command := testkit.ManualCommand(t, s, before.ID)
					if err != nil || manual.State != workflow.Manual || manual.Implementer.SessionID != before.Implementer.SessionID || len(manual.CreditProbes) != 1 || manual.CreditProbes[0].Status != "released" || prepared.State != workflow.Manual || !strings.Contains(strings.Join(command.Args, " "), before.Implementer.SessionID) {
						t.Fatalf("probe takeover: %+v %v", manual, err)
					}
					status, err := r.SessionStatus(ctx, ref)
					if err != nil || status.State != runner.SessionExited {
						t.Fatalf("probe survived takeover: %+v %v", status, err)
					}
					if phase == workflow.Review {
						if !manual.Review.Restored || manual.Review.Accepted || testkit.GitCommand(t, path, "show", "HEAD:feature.txt") != "implemented" {
							t.Fatalf("review restoration: %+v", manual.Review)
						}
						data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
						if err != nil || string(data) != "implemented\n" {
							t.Fatalf("review overwrote implementer: %q %v", data, err)
						}
						if _, err := os.Stat(filepath.Join(path, marker)); !os.IsNotExist(err) {
							t.Fatal("reviewer edit survived takeover")
						}
					} else if data, err := os.ReadFile(filepath.Join(path, marker)); err != nil || string(data) != "probe" {
						t.Fatalf("lost partial implementer work: %q %v", data, err)
					}
					states, err := s.HarnessAvailability(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, state := range states {
						if state.Harness == agent && (state.Available || state.ProbeID != "" || state.Reason != "credits_exhausted") {
							t.Fatalf("takeover cleared credit block: %+v", state)
						}
					}
					// A different eligible run can own the next explicit recovery request.
					other, err := rt.Workflow.Transition(ctx, "other-run", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 8})
					if err != nil {
						t.Fatal(err)
					}
					api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
					if _, err := rt.Workflow.Transition(ctx, other.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "test.interrupted_claim", Message: "Interrupted before worktree preparation"}}); err != nil {
						t.Fatal(err)
					}
					// Reconciliation selects implementation for this run. When the reviewer
					// alone is blocked, that selection correctly requires no credit probe.
					next, err := s.Retry(ctx, other.ID)
					if err != nil {
						t.Fatal(err)
					}
					if agent == implementer {
						testkit.WaitForWithin(t, 30*time.Second, func() bool {
							if err := s.Tick(ctx); err != nil {
								t.Fatal(err)
							}
							waiting, err := rt.Workflow.Get(ctx, other.ID)
							if err != nil {
								t.Fatal(err)
							}
							return waiting.State == workflow.WaitingForHarness
						})
						next, err = s.Retry(ctx, other.ID)
						if err != nil || len(next.CreditProbes) != 1 || next.CreditProbes[0].Status != "reserved" {
							t.Fatalf("could not select another run: %+v %v", next, err)
						}
					} else if len(next.CreditProbes) != 0 {
						t.Fatalf("other harness reserved probe: %+v", next.CreditProbes)
					}
					if err := s.Stop(ctx, other.ID); err != nil {
						t.Fatal(err)
					}
					root := rt.Workspace.Root
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
					rt, err = app.Open(ctx, root)
					if err != nil {
						t.Fatal(err)
					}
					defer rt.Close()
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					launches := len(h.Phases)
					for range 2 {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
					}
					saved, err := rt.Workflow.Get(ctx, before.ID)
					if err != nil || saved.State != workflow.Manual || len(h.Phases) != launches || saved.CreditProbes[0].Status != "released" {
						t.Fatalf("manual restart duplicated probe: %+v %v", saved, err)
					}
				})
			}
		}
	}
}

func TestCreditProbeStopTakeoverRetryRace(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		for _, launched := range []bool{false, true} {
			name := agent + "/reserved"
			if launched {
				name = agent + "/running"
			}
			t.Run(name, func(t *testing.T) {

				_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `exit 1`)
				h := testkit.ManualLimitHarness(cfg, agent, harness.CreditsExhausted)
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
				ctx := context.Background()
				if _, err := s.Retry(ctx, before.ID); err != nil {
					t.Fatal(err)
				}
				launches := 1
				if launched {
					launches = 2
					testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `printf probe > probe.txt; sleep 60`)
					testkit.WaitForWithin(t, 30*time.Second, func() bool {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
						_, err := os.Stat(filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID, "probe.txt"))
						return err == nil
					})
				}
				start := make(chan struct{})
				stopResult := make(chan error, 1)
				others := make(chan struct{}, 2)
				go func() { <-start; stopResult <- s.Stop(ctx, before.ID) }()
				go func() { <-start; s.Takeover(ctx, before.ID); others <- struct{}{} }()
				go func() { <-start; s.Retry(ctx, before.ID); others <- struct{}{} }()
				close(start)
				if err := <-stopResult; err != nil {
					t.Fatal(err)
				}
				<-others
				<-others
				for range 2 {
					if err := s.Tick(ctx); err != nil {
						t.Fatal(err)
					}
				}
				saved, err := rt.Workflow.Get(ctx, before.ID)
				if err != nil || saved.State != workflow.Stopped || len(saved.CreditProbes) != 1 || saved.CreditProbes[0].Status != "released" || len(h.Phases) != launches {
					t.Fatalf("race duplicated/revived work: %+v %v", saved, err)
				}
				states, err := s.HarnessAvailability(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, state := range states {
					if state.Harness == agent && (state.Available || state.ProbeID != "") {
						t.Fatalf("race changed block: %+v", state)
					}
				}
			})
		}
	}
}
