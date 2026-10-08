package scheduler_test

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
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func classifiedAdapter(cfgAgent string, claude, codex harness.HarnessAdapter) *classifiedHarness {
	adapter := claude
	if cfgAgent == "codex" {
		adapter = codex
	}
	return &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "phase-fixture"}}
}

func TestHarnessPairingsWaitBeforeReviewOrFixLaunch(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					_, runtime, api, remote, cfg, r := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
					now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
					deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }}
					s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
					if err != nil {
						t.Fatal(err)
					}
					run := finish(t, s, workflow.Active, phase)
					agent := reviewer
					if phase == workflow.Fix {
						agent = implementer
					}
					// A second repository discovers an account limit while this run is
					// between phases. Its scheduler has no authorization to advance repo.
					accountCfg := cfg
					accountCfg.Concurrency = 2
					accountRepo := cfg.Repositories[0]
					accountRepo.Repo, accountRepo.Implementer.Agent = "owner/account", agent
					accountCfg.Repositories = []config.Repository{accountRepo}
					api.issues[accountRepo.Repo] = []github.Issue{ready(8)}
					gitCommand(t, filepath.Dir(remote), "config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "--add", "url."+remote+".insteadOf", "https://github.com/owner/account.git")
					replacePhaseScript(t, accountCfg, workflow.Fix, `exit 1`)
					fake := classifiedAdapter(agent, harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
					accountDeps := deps
					accountDeps.Harnesses = map[string]harness.HarnessAdapter{agent: fake}
					account, err := scheduler.New(accountCfg, schedulerResources(runtime), accountDeps)
					if err != nil {
						t.Fatal(err)
					}
					waitFor(t, func() bool {
						if err := account.Tick(context.Background()); err != nil {
							t.Fatal(err)
						}
						runs, err := account.Runs(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						for _, v := range runs {
							if v.Repository == accountRepo.Repo && v.State == workflow.WaitingForHarness {
								return true
							}
						}
						return false
					})
					// Reinstate usable fake executables; the durable account restriction
					// alone must prevent even constructing the next phase invocation.
					replacePhaseScript(t, cfg, phase, loopScript(`printf fixed > feature.txt; `+fixedReport))
					for range 2 {
						if err := s.Tick(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					waiting, err := runtime.Workflow.Get(context.Background(), run.ID)
					if err != nil || waiting.State != workflow.WaitingForHarness || waiting.HarnessWait.AttemptID != "" || waiting.HarnessWait.Phase != phase || waiting.HarnessWait.Harness != agent || waiting.ReviewRound != 1 {
						t.Fatalf("next phase bypassed account gate: %+v %v", waiting, err)
					}
					artifacts, err := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", string(phase)+"-*"))
					if err != nil || len(artifacts) != 0 {
						t.Fatalf("pre-launch wait constructed an execution: %v %v", artifacts, err)
					}
					root := runtime.Workspace.Root
					if err := runtime.Close(); err != nil {
						t.Fatal(err)
					}
					runtime, err = app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					reopened := runtime
					t.Cleanup(func() { reopened.Close() })
					s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
					if err != nil {
						t.Fatal(err)
					}
					now = waiting.HarnessWait.ResetAt
					var done workflow.Run
					waitForWithin(t, 15*time.Second, func() bool {
						if err := s.Tick(context.Background()); err != nil {
							t.Fatal(err)
						}
						done, err = runtime.Workflow.Get(context.Background(), run.ID)
						if err != nil {
							t.Fatal(err)
						}
						return done.State == workflow.WaitingForCI
					})
					if done.Review.Attempt != 1 || (phase == workflow.Fix && (done.Fix.Attempt != 1 || done.Fix.SessionID != run.Implementer.SessionID)) || done.Review.SessionID == done.Implementer.SessionID || len(done.HarnessWaitHistory) != 1 || len(done.Retries) != 0 {
						t.Fatalf("pre-launch wait spent budgets or resumed wrong role: %+v", done)
					}
				})
			}
		}
	}
}

func TestReviewAndFixWaitAllowancesAreIndependent(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				_, runtime, api, remote, cfg, r := pairingFlow(t, implementer, reviewer, disputedReport)
				limited := `case "$*" in
 *review-1-1*|*review-1-2*|*review-1-3*) printf contaminated > feature.txt; printf reviewer > reviewer.txt; git add feature.txt; git -c user.name=Reviewer -c user.email=reviewer@example.invalid commit -m 'Reviewer edit' >&2; exit 1;;
 *fix-1-1*|*fix-1-2*|*fix-1-3*) printf partial >> partial.txt; exit 1;;
 esac
` + loopScript(`test "$(cat partial.txt)" = partialpartialpartial || exit 9; printf fixed > feature.txt; `+fixedReport)
				replacePhaseScript(t, cfg, workflow.Review, limited)
				replacePhaseScript(t, cfg, workflow.Fix, limited)
				cfg.UsageLimits.MaxWaits = 2
				now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
				claude := classifiedAdapter("claude", harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
				codex := classifiedAdapter("codex", harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
				deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": claude, "codex": codex}}
				s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				var waiting workflow.Run
				for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
					waiting = finish(t, s, workflow.WaitingForHarness, phase)
					if waiting.ReviewRound != 1 || waiting.HarnessWait.Consecutive != 1 || waiting.HarnessWait.Allowance != 2 || waiting.HarnessWait.Phase != phase {
						t.Fatalf("phase inherited another wait budget: %+v", waiting.HarnessWait)
					}
					if phase == workflow.Review && (!waiting.Review.Restored || !waiting.Review.Contaminated || waiting.Review.Accepted || waiting.Review.Report != nil || waiting.ApprovedSHA != "") {
						t.Fatalf("interrupted verdict accepted: %+v", waiting.Review)
					}
					session := waiting.Review.SessionID
					agent := reviewer
					if phase == workflow.Fix {
						session, agent = waiting.Implementer.SessionID, implementer
					}
					if waiting.HarnessWait.Harness != agent {
						t.Fatalf("waiting on wrong role: %+v", waiting.HarnessWait)
					}
					for _, allowance := range []int{3, 4} {
						now = waiting.HarnessWait.ResetAt
						attention := finish(t, s, workflow.NeedsAttention, phase)
						if attention.LastErrorCode != "harness.usage_limit_waits_exhausted" || attention.HarnessWait.Consecutive != allowance-1 || attention.HarnessWait.Allowance != allowance-1 || attention.ReviewRound != 1 {
							t.Fatalf("wait bound lost: %+v", attention)
						}
						waiting, err = s.Retry(context.Background(), attention.ID)
						if err != nil {
							t.Fatal(err)
						}
						last := waiting.Retries[len(waiting.Retries)-1]
						if waiting.State != workflow.WaitingForHarness || last.NextPhase != phase || last.Round != 1 || last.GrantedWait != allowance {
							t.Fatalf("incorrect wait grant: %+v", last)
						}
						if allowance == 3 {
							server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
							if err != nil {
								t.Fatal(err)
							}
							role := "reviewer"
							if phase == workflow.Fix {
								role = "implementer"
							}
							for _, path := range []string{"/runs/" + waiting.ID, "/settings"} {
								response := httptest.NewRecorder()
								server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
								for _, value := range []string{string(phase) + " / round 1 · " + role, "temporary_limit", "Exactly one additional usage-limit wait granted; allowance 3"} {
									if response.Code != 200 || !strings.Contains(response.Body.String(), value) {
										t.Errorf("%s missing %q", path, value)
									}
								}
							}
						}
						for range 2 {
							again, err := s.Retry(context.Background(), waiting.ID)
							if err != nil || len(again.Retries) != len(waiting.Retries) {
								t.Fatalf("duplicated wait grant: %+v %v", again, err)
							}
						}
						root := runtime.Workspace.Root
						if err := runtime.Close(); err != nil {
							t.Fatal(err)
						}
						runtime, err = app.Open(context.Background(), root)
						if err != nil {
							t.Fatal(err)
						}
						reopened := runtime
						t.Cleanup(func() { reopened.Close() })
						s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
						if err != nil {
							t.Fatal(err)
						}
						for range 2 {
							if err := s.Tick(context.Background()); err != nil {
								t.Fatal(err)
							}
						}
						saved, err := runtime.Workflow.Get(context.Background(), waiting.ID)
						if err != nil || saved.State != workflow.WaitingForHarness || len(saved.Retries) != len(waiting.Retries) {
							t.Fatalf("restart bypassed reset or lost grant: %+v %v", saved, err)
						}
					}
					now = waiting.HarnessWait.ResetAt
					nextState, nextPhase := workflow.Active, workflow.Fix
					if phase == workflow.Fix {
						nextState, nextPhase = workflow.WaitingForCI, workflow.Review
					}
					done := finish(t, s, nextState, nextPhase)
					contexts := claude.phases
					if agent == "codex" {
						contexts = codex.phases
					}
					resumes := 0
					for _, p := range contexts {
						if p.Phase != phase || !strings.Contains(p.PhaseDir, string(phase)+"-1-") {
							continue
						}
						if strings.HasSuffix(p.PhaseDir, "-1") {
							continue
						}
						if !p.Resume || p.SessionID != session || !strings.Contains(p.Interruption, "usage limit") {
							t.Fatalf("resumed wrong role/context: %+v", p)
						}
						resumes++
					}
					if resumes != 3 || done.Implementer.SessionID == done.Review.SessionID {
						t.Fatalf("lost role continuity: resumes=%d run=%+v", resumes, done)
					}
				}
				done, err := runtime.Workflow.Get(context.Background(), waiting.ID)
				if err != nil || len(done.HarnessWaitHistory) != 6 || len(done.Retries) != 4 || done.ReviewRound != 2 || done.Fix.Attempt != 4 || done.Review.Attempt != 1 || api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
					t.Fatalf("limits spent engineering budgets or duplicated publication: %+v %v", done, err)
				}
			})
		}
	}
}

func TestUsageLimitedReviewRetainsPinnedHead(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				_, runtime, api, _, cfg, r := pairingFlow(t, implementer, reviewer, disputedReport)
				replacePhaseScript(t, cfg, workflow.Review, reviewScript(`case "$*" in *review-1-1*) exit 1;; esac
`+approvedReview))
				fake := classifiedAdapter(reviewer, harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
				now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
				s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{reviewer: fake}})
				if err != nil {
					t.Fatal(err)
				}
				waiting := finish(t, s, workflow.WaitingForHarness, workflow.Review)
				paths, err := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("worktrees: %v %v", paths, err)
				}
				if err := os.WriteFile(filepath.Join(paths[0], "feature.txt"), []byte("changed while waiting"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, paths[0], "add", "feature.txt")
				gitCommand(t, paths[0], "commit", "-m", "Change during wait")
				gitCommand(t, paths[0], "push", "origin", "HEAD")
				now = waiting.HarnessWait.ResetAt
				var done workflow.Run
				waitFor(t, func() bool {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
					done, err = runtime.Workflow.Get(context.Background(), waiting.ID)
					if err != nil {
						t.Fatal(err)
					}
					return done.State == workflow.NeedsAttention || done.State == workflow.WaitingForCI
				})
				if done.State != workflow.NeedsAttention || done.LastErrorCode != "review.head_changed" || done.ApprovedSHA != "" || done.Review.Accepted || done.Review.Attempt != 1 || done.Review.TargetSHA != waiting.Review.TargetSHA {
					t.Fatalf("resumed review silently changed its target: %+v review=%+v", done, done.Review)
				}
				// Explicit Retry reconciles and authorizes the new published target.
				if _, err := s.Retry(context.Background(), done.ID); err != nil {
					t.Fatal(err)
				}
				done = finish(t, s, workflow.WaitingForCI, workflow.Review)
				if done.Review.Attempt != 2 || done.Review.SessionID != waiting.Review.SessionID || done.ApprovedSHA != gitCommand(t, paths[0], "rev-parse", "HEAD") {
					t.Fatalf("explicit new-target review lost continuity: %+v", done)
				}
				input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", done.ID, "phases", "review-1-2", "input.md"))
				if err != nil || strings.Contains(string(input), "## Interruption context") {
					t.Fatalf("new-target review inherited stale interruption context: %s %v", input, err)
				}
			})
		}
	}
}

func TestReviewAndFixLimitsLeaveOrdinaryAttemptsAvailable(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					_, runtime, api, _, cfg, r := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
					limited := `case "$*" in *` + string(phase) + `-1-1*) exit 1;; *` + string(phase) + `-1-2*) echo 'Ordinary fixture failure' >&2; exit 1;; esac
`
					script := reviewScript(limited + approvedReview)
					agent := reviewer
					cfg.Repositories[0].Reviewer.MaxAttempts = 2
					if phase == workflow.Fix {
						agent = implementer
						cfg.Repositories[0].Implementer.MaxAttempts = 2
						script = loopScript(limited + `printf fixed > feature.txt; ` + fixedReport)
					}
					replacePhaseScript(t, cfg, phase, script)
					fake := classifiedAdapter(agent, harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
					fake.outcomes = []harness.FailureClassification{fake.outcome, {Kind: harness.Ordinary}}
					now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
					s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
					if err != nil {
						t.Fatal(err)
					}
					waiting := finish(t, s, workflow.WaitingForHarness, phase)
					now = waiting.HarnessWait.ResetAt
					done := finish(t, s, workflow.WaitingForCI, workflow.Review)
					if phase == workflow.Review && (done.Review.Attempt != 3 || done.ReviewRound != 1 || done.Review.SessionID != waiting.Review.SessionID) {
						t.Fatalf("review limit spent an ordinary attempt: %+v", done)
					}
					if phase == workflow.Fix && (done.Fix.Attempt != 3 || done.ReviewRound != 2 || done.Fix.SessionID != waiting.Implementer.SessionID) {
						t.Fatalf("fix limit spent an ordinary attempt: %+v", done)
					}
					if len(done.HarnessWaitHistory) != 1 || len(done.Retries) != 0 || len(done.SessionRecoveries) != 0 || fake.calls != 2 {
						t.Fatalf("ordinary failure was hidden or required an extra grant: %+v calls=%d", done, fake.calls)
					}
				})
			}
		}
	}
}
