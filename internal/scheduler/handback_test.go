package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/app"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHandbackPublishesManualWorkWithoutPRAndResumesImplementer(t *testing.T) {
	s, runtime, _, remote, _, _ := localFlow(t, `if [ ! -f started.txt ]; then printf started > started.txt; sleep 60; else
`+successfulScript+`
fi`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	before := runs[0]
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "started.txt")); return err == nil })
	if _, err := s.Takeover(ctx, before.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("manual change"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Handback(ctx, before.ID)
	if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Implement || selected.Implementer.SessionID != before.Implementer.SessionID {
		t.Fatalf("handback: %+v %v", selected, err)
	}
	head := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	if head != gitCommand(t, path, "rev-parse", "HEAD") || gitCommand(t, remote, "show", head+":manual.txt") != "manual change" {
		t.Fatal("manual work not published")
	}
	if _, err := s.Handback(ctx, before.ID); err != nil {
		t.Fatal(err)
	}
	if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
		t.Fatalf("duplicate commit: %s", got)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.Implementer.Attempt != 2 || after.Implementer.SessionID != before.Implementer.SessionID {
		t.Fatalf("implementer continuity: %+v", after)
	}
}

func TestHandbackExistingPRAlwaysGetsIndependentReviewAndOneDurableRound(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, edited := range []bool{false, true} {
				name := implementer + "/" + reviewer + "/" + fmt.Sprint(edited)
				t.Run(name, func(t *testing.T) {
					_, runtime, api, remote, cfg, r := pairingFlow(t, implementer, reviewer, disputedReport)
					cfg.MaxRounds = 1
					s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					before := finish(t, s, workflow.NeedsAttention, workflow.Review)
					if before.LastErrorCode != "review.max_rounds_exceeded" {
						t.Fatalf("fixture: %+v", before)
					}
					if _, err := s.Takeover(context.Background(), before.ID); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
					if edited {
						if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("manual repair"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					selected, err := s.Handback(context.Background(), before.ID)
					if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Review || selected.ReviewRound != 2 || selected.ApprovedSHA != "" || selected.Implementer.SessionID != before.Implementer.SessionID || len(selected.Handbacks) != 1 || selected.Handbacks[0].GrantedRound != 2 {
						t.Fatalf("selected: %+v %v", selected, err)
					}
					if _, err := s.Handback(context.Background(), before.ID); err != nil {
						t.Fatal(err)
					}
					root := runtime.Workspace.Root
					if err := runtime.Close(); err != nil {
						t.Fatal(err)
					}
					restarted, err := app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					defer restarted.Close()
					s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					after := finish(t, s, workflow.WaitingForCI, workflow.Review)
					if after.ReviewRound != 2 || after.Review.SessionID == after.Implementer.SessionID || after.ApprovedSHA != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || len(after.Handbacks) != 1 {
						t.Fatalf("independent review: %+v", after)
					}
					want := "1"
					if edited {
						want = "2"
					}
					if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != want {
						t.Fatalf("publication commits: %s want %s", got, want)
					}
				})
			}
		}
	}
}

func TestHandbackInvalidatesApprovalAndPreservesManualCommits(t *testing.T) {
	_, runtime, api, remote, cfg, r := localFlow(t, successfulScript)
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("manually committed"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-m", "human commit")
	manual := gitCommand(t, path, "rev-parse", "HEAD")
	selected, err := s.Handback(context.Background(), before.ID)
	if err != nil || selected.ApprovedSHA != "" || selected.ReviewRound != 2 || selected.Handbacks[0].GrantedRound != 0 {
		t.Fatalf("selection: %+v %v", selected, err)
	}
	if gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != manual {
		t.Fatal("manual commit replaced")
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ApprovedSHA != manual || !after.Review.Accepted || after.Implementer.SessionID != before.Implementer.SessionID {
		t.Fatalf("fresh approval: %+v", after)
	}
}

func TestHandbackPreservesUnsafeExternalStateForAttention(t *testing.T) {
	for _, change := range []string{"closed PR", "closed issue", "wrong branch", "remote ahead", "rewritten local head", "wrong PR owner"} {
		t.Run(change, func(t *testing.T) {
			s, runtime, api, remote, _, _ := localFlow(t, successfulScript)
			before := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			pr := api.prs["mergeyard/issue-7"]
			switch change {
			case "closed PR":
				pr.State = github.Closed
			case "closed issue":
				api.issues["owner/repo"][0].State = github.Closed
			case "wrong branch":
				gitCommand(t, path, "checkout", "-b", "foreign")
			case "rewritten local head":
				gitCommand(t, path, "reset", "--hard", "HEAD~1")
			case "wrong PR owner":
				pr.Head.Repo.FullName = "someone/else"
			case "remote ahead":
				external := filepath.Join(t.TempDir(), "external")
				gitCommand(t, t.TempDir(), "clone", remote, external)
				gitCommand(t, external, "checkout", "mergeyard/issue-7")
				if err := os.WriteFile(filepath.Join(external, "external.txt"), []byte("external work"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, external, "add", ".")
				gitCommand(t, external, "commit", "-m", "external")
				gitCommand(t, external, "push", "origin", "mergeyard/issue-7")
				pr.Head.SHA = gitCommand(t, external, "rev-parse", "HEAD")
			}
			if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			remoteHead := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			selected, err := s.Handback(context.Background(), before.ID)
			if err == nil || selected.State != workflow.NeedsAttention || len(selected.Handbacks) != 1 || selected.Handbacks[0].GrantedRound != 0 || selected.Handbacks[0].Pending {
				t.Fatalf("unsafe handback: %+v %v", selected, err)
			}
			data, err := os.ReadFile(filepath.Join(path, "manual.txt"))
			if err != nil || string(data) != "preserved" || remoteHead != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") {
				t.Fatal("unexpected state overwritten")
			}
		})
	}
}

func TestHandbackRequiresInteractiveAndOwnedProcessesExited(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprint(interactive), func(t *testing.T) {
			s, runtime, api, remote, cfg, r := localFlow(t, successfulScript)
			before := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			if interactive {
				lock, err := runner.AcquireInteractive(runtime.Workspace.Root, before.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			} else {
				var err error
				s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: ambiguousTakeoverRunner{r}})
				if err != nil {
					t.Fatal(err)
				}
			}
			head := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			_, err := s.Handback(context.Background(), before.ID)
			if err == nil {
				t.Fatal("live or ambiguous process accepted")
			}
			after, err := runtime.Workflow.Get(context.Background(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if interactive && (after.State != workflow.Manual || len(after.Handbacks) != 0) {
				t.Fatalf("interactive exit bypassed: %+v", after)
			}
			if !interactive && after.State != workflow.NeedsAttention {
				t.Fatalf("unverified phase exit: %+v", after)
			}
			if head != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || gitCommand(t, path, "status", "--porcelain") == "" {
				t.Fatal("published despite process ownership")
			}
		})
	}
}

type interruptedHandbackGit struct {
	*managedgit.Manager
	boundary string
	cancel   context.CancelFunc
}

func (g interruptedHandbackGit) CommitHandback(ctx context.Context, run managedgit.Run, previous string) (managedgit.CommitResult, error) {
	result, err := g.Manager.CommitHandback(ctx, run, previous)
	if err == nil && g.boundary == "commit" {
		g.cancel()
		return result, ctx.Err()
	}
	return result, err
}
func (g interruptedHandbackGit) PushHandback(ctx context.Context, run managedgit.Run, previous, target string) error {
	err := g.Manager.PushHandback(ctx, run, previous, target)
	if err == nil && g.boundary == "push" {
		g.cancel()
		return ctx.Err()
	}
	return err
}

func TestHandbackPublicationAndGrantRecoverAfterRestart(t *testing.T) {
	for _, boundary := range []string{"commit", "push", "grant"} {
		t.Run(boundary, func(t *testing.T) {
			_, runtime, api, remote, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
			cfg.MaxRounds = 1
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.NeedsAttention, workflow.Review)
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("manual repair"), 0600); err != nil {
				t.Fatal(err)
			}
			requestCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Git: interruptedHandbackGit{managedgit.New(runtime.Workspace), boundary, cancel}})
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "grant" {
				// Fault injection at the durable event write rolls back the entire selection.
				if _, err := runtime.DB.Exec(`CREATE TRIGGER fail_handback_grant BEFORE INSERT ON events WHEN NEW.type='run.handed_back' BEGIN SELECT RAISE(ABORT,'interrupted grant'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Handback(requestCtx, before.ID); err == nil {
				t.Fatal("fixture did not interrupt")
			}
			pending, err := runtime.Workflow.Get(context.Background(), before.ID)
			if err != nil || pending.State != workflow.Manual || pending.ReviewRound != 1 || pending.PendingHandback() == nil {
				t.Fatalf("lost journal: %+v %v", pending, err)
			}
			if boundary == "grant" {
				if _, err := runtime.DB.Exec("DROP TRIGGER fail_handback_grant"); err != nil {
					t.Fatal(err)
				}
			}
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			selected, err := restarted.Workflow.Get(context.Background(), before.ID)
			if err != nil || selected.Phase != workflow.Review || selected.State != workflow.Active || selected.ReviewRound != 2 || len(selected.Handbacks) != 1 || selected.Handbacks[0].GrantedRound != 2 || selected.Handbacks[0].Pending {
				t.Fatalf("recovered: %+v %v", selected, err)
			}
			if _, err := s.Handback(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "2" {
				t.Fatalf("duplicate publication: %s", got)
			}
			history, err := restarted.Events.History(context.Background(), 0, 200)
			if err != nil {
				t.Fatal(err)
			}
			selectedEvents := 0
			for _, event := range history {
				if event.Type == "run.handed_back" && strings.Contains(string(event.Payload), `"trigger":"hand_back"`) {
					selectedEvents++
				}
			}
			if selectedEvents != 1 {
				t.Fatalf("grant selections: %d", selectedEvents)
			}
		})
	}
}

func TestDashboardHandbackUsesProtectedSharedOperation(t *testing.T) {
	_, runtime, api, _, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
	cfg.MaxRounds = 1
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.NeedsAttention, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg, ConfigPath: "/tmp/test-config.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"/runs/", "/api/runs/"} {
		for _, attack := range []string{"missing token", "foreign origin"} {
			request := httptest.NewRequest("POST", "http://127.0.0.1:7331"+action+before.ID+"/handback", nil)
			request.Header.Set("Origin", "http://127.0.0.1:7331")
			if attack == "foreign origin" {
				request.Header.Set("X-CSRF-Token", status.Token)
				request.Header.Set("Origin", "https://attacker.example")
			}
			response = httptest.NewRecorder()
			server.ServeHTTP(response, request)
			after, err := runtime.Workflow.Get(context.Background(), before.ID)
			if err != nil || response.Code != http.StatusForbidden || after.State != workflow.Manual || len(after.Handbacks) != 0 {
				t.Fatalf("unprotected handback: %d %+v %v", response.Code, after, err)
			}
		}
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
	for _, text := range []string{"Hand back run", "Exit the interactive harness", "exactly one additional round", "original implementer session"} {
		if !strings.Contains(response.Body.String(), text) {
			t.Fatalf("missing disclosure %q", text)
		}
	}
	request := httptest.NewRequest("POST", "http://127.0.0.1:7331/runs/"+before.ID+"/handback", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Selected ACTIVE / review") || !strings.Contains(response.Body.String(), "Granted additional review round: 2") || strings.Contains(response.Body.String(), ">Hand back run</button>") {
		t.Fatalf("handback page: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+before.ID+"/handback", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var selected workflow.Run
	if err := json.Unmarshal(response.Body.Bytes(), &selected); err != nil || response.Code != 200 || len(selected.Handbacks) != 1 || selected.Handbacks[0].GrantedRound != 2 {
		t.Fatalf("API duplicate grant: %d %+v %v", response.Code, selected, err)
	}
}

func TestHandbackReusesUnfinishedReviewRound(t *testing.T) {
	s, runtime, api, _, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
	before := finish(t, s, workflow.Active, workflow.Review)
	replacePhaseScript(t, cfg, workflow.Review, reviewScript(`printf reviewer > review-only.txt; sleep 60`))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("manual repair"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Handback(context.Background(), before.ID)
	if err != nil || selected.ReviewRound != 1 || selected.Handbacks[0].GrantedRound != 0 || selected.Handbacks[0].AttemptFrom != 2 {
		t.Fatalf("unfinished review selection: %+v %v", selected, err)
	}
	replacePhaseScript(t, cfg, workflow.Review, reviewScript(approvedReview))
	// Continue against GitHub's freshly observed published head.
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ReviewRound != 1 || after.Review.Attempt != 2 || after.Review.SessionID == after.Implementer.SessionID {
		t.Fatalf("review was skipped: %+v", after)
	}
}

func TestHandbackPendingPublicationIsVisibleAndStopCancelsIt(t *testing.T) {
	s, runtime, api, remote, cfg, r := localFlow(t, successfulScript)
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	// A rejecting receive hook establishes a real publication failure.
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Handback(context.Background(), before.ID)
	if err == nil || selected.State != workflow.Manual || selected.PendingHandback() == nil || selected.PendingHandback().Error == "" || selected.ApprovedSHA != "" {
		t.Fatalf("pending publication: %+v %v", selected, err)
	}
	if _, err := s.Takeover(context.Background(), before.ID); err == nil {
		t.Fatal("interactive resume raced pending publication")
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
	for _, text := range []string{"Selected ACTIVE / review", "Publication or reconciliation is incomplete", "Handback publication is pending"} {
		if !strings.Contains(response.Body.String(), text) {
			t.Fatalf("missing recovery detail %q", text)
		}
	}
	if err := s.Stop(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || after.State != workflow.Stopped || after.PendingHandback() != nil || after.Handbacks[0].Error == "" {
		t.Fatalf("stop did not cancel: %+v %v", after, err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "manual.txt")); err != nil || string(data) != "preserved" {
		t.Fatal("manual work lost")
	}
}

func TestHandbackNoPRPublishesUnchangedOrEditedWorkForEitherHarness(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, edited := range []bool{false, true} {
			t.Run(agent+"/"+fmt.Sprint(edited), func(t *testing.T) {
				flow, script := localFlow, `exit 1`
				if agent == "codex" {
					flow, script = codexFlow, codexIdentity+"\nexit 1"
				}
				s, runtime, _, remote, _, _ := flow(t, script)
				before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
				if _, err := s.Takeover(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
				if edited {
					if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("manual"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				selected, err := s.Handback(context.Background(), before.ID)
				if err != nil || selected.Phase != workflow.Implement || selected.State != workflow.Active || selected.Implementer.SessionID != before.Implementer.SessionID || selected.Handbacks[0].GrantedRound != 0 {
					t.Fatalf("selection: %+v %v", selected, err)
				}
				want := "0"
				if edited {
					want = "1"
				}
				if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != want {
					t.Fatalf("manual publication: %s want %s", got, want)
				}
			})
		}
	}
}

func TestHandbackAfterRetryReviewsNewestManualCommitWithFractionalTimestamp(t *testing.T) {
	_, runtime, api, remote, cfg, r := localFlow(t, `case "$*" in *review-*) exit 1;; *) `+successfulScript+`;; esac`)
	now := time.Date(2026, 10, 7, 12, 0, 0, 123450000, time.UTC)
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.NeedsAttention, workflow.Review)
	if _, err := s.Retry(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("new manual work"), 0600); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 7, 12, 0, 0, 123456000, time.UTC)
	selected, err := s.Handback(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ReviewRound != 1 {
		t.Fatalf("fixture did not retain round: %+v", selected)
	}
	replacePhaseScript(t, cfg, workflow.Review, reviewScript(approvedReview))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	head := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	if err != nil || after.State != workflow.Active || after.Phase != workflow.Review || after.Review.Attempt != 2 || after.Review.TargetSHA != head {
		t.Fatalf("newer handback target lost: %+v %v", after, err)
	}
}

func TestHandbackReconcilesLabelsAfterPublicationSelection(t *testing.T) {
	_, runtime, api, _, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
	cfg.MaxRounds = 1
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.NeedsAttention, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	api.mutate = func(action, repo string, n int, label string) error {
		if action == "remove" && label == "agent-needs-attention" {
			return fmt.Errorf("label response lost")
		}
		return nil
	}
	selected, err := s.Handback(context.Background(), before.ID)
	if err == nil || selected.State != workflow.Active || len(selected.Handbacks) != 1 {
		t.Fatalf("fixture did not select before label failure: %+v %v", selected, err)
	}
	api.mutate = nil
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if has(api.issues["owner/repo"][0], "agent-needs-attention") || !has(api.issues["owner/repo"][0], "agent-running") {
		t.Fatal("handback label bookkeeping was not recovered")
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || len(after.Handbacks) != 1 || after.Handbacks[0].GrantedRound != 2 {
		t.Fatalf("label recovery duplicated handback: %+v %v", after, err)
	}
}
