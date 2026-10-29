package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHandbackPublishesManualWorkWithoutPRAndResumesImplementer(t *testing.T) {

	s, runtime, _, remote, _, _ := testkit.TmuxFlow(t, `if [ ! -f started.txt ]; then printf started > started.txt; sleep 60; else
`+testkit.SuccessfulScript+`
fi`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	before := runs[0]
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "started.txt")); return err == nil })
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
	head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	if head != testkit.GitCommand(t, path, "rev-parse", "HEAD") || testkit.GitCommand(t, remote, "show", head+":manual.txt") != "manual change" {
		t.Fatal("manual work not published")
	}
	if _, err := s.Handback(ctx, before.ID); err != nil {
		t.Fatal(err)
	}
	if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
		t.Fatalf("duplicate commit: %s", got)
	}
	after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
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

					_, runtime, api, remote, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
					cfg.MaxRounds = 1
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
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
					server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
					if err != nil {
						t.Fatal(err)
					}
					response := httptest.NewRecorder()
					server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+before.ID, nil))
					if response.Code != 200 || !strings.Contains(response.Body.String(), "Granted additional review round: 2") {
						t.Fatalf("run detail lost the handback grant: %d %s", response.Code, response.Body.String())
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
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
					if after.ReviewRound != 2 || after.Review.SessionID == after.Implementer.SessionID || after.ApprovedSHA != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || len(after.Handbacks) != 1 {
						t.Fatalf("independent review: %+v", after)
					}
					want := "1"
					if edited {
						want = "2"
					}
					if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != want {
						t.Fatalf("publication commits: %s want %s", got, want)
					}
				})
			}
		}
	}
}

func TestHandbackInvalidatesApprovalAndPreservesManualCommits(t *testing.T) {

	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("manually committed"), 0600); err != nil {
		t.Fatal(err)
	}
	testkit.GitCommand(t, path, "add", ".")
	testkit.GitCommand(t, path, "commit", "-m", "human commit")
	manual := testkit.GitCommand(t, path, "rev-parse", "HEAD")
	selected, err := s.Handback(context.Background(), before.ID)
	if err != nil || selected.ApprovedSHA != "" || selected.ReviewRound != 2 || selected.Handbacks[0].GrantedRound != 0 {
		t.Fatalf("selection: %+v %v", selected, err)
	}
	if testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != manual {
		t.Fatal("manual commit replaced")
	}
	after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ApprovedSHA != manual || !after.Review.Accepted || after.Implementer.SessionID != before.Implementer.SessionID {
		t.Fatalf("fresh approval: %+v", after)
	}
}

func TestHandbackPreservesUnsafeExternalStateForAttention(t *testing.T) {

	for _, change := range []string{"closed PR", "closed issue", "wrong branch", "remote ahead", "rewritten local head", "wrong PR owner"} {
		t.Run(change, func(t *testing.T) {

			s, runtime, api, remote, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			pr := api.Prs["mergeyard/issue-7"]
			switch change {
			case "closed PR":
				pr.State = github.Closed
			case "closed issue":
				api.Issues["owner/repo"][0].State = github.Closed
			case "wrong branch":
				testkit.GitCommand(t, path, "checkout", "-b", "foreign")
			case "rewritten local head":
				testkit.GitCommand(t, path, "reset", "--hard", "HEAD~1")
			case "wrong PR owner":
				pr.Head.Repo.FullName = "someone/else"
			case "remote ahead":
				external := filepath.Join(t.TempDir(), "external")
				testkit.GitCommand(t, t.TempDir(), "clone", remote, external)
				testkit.GitCommand(t, external, "checkout", "mergeyard/issue-7")
				if err := os.WriteFile(filepath.Join(external, "external.txt"), []byte("external work"), 0600); err != nil {
					t.Fatal(err)
				}
				testkit.GitCommand(t, external, "add", ".")
				testkit.GitCommand(t, external, "commit", "-m", "external")
				testkit.GitCommand(t, external, "push", "origin", "mergeyard/issue-7")
				pr.Head.SHA = testkit.GitCommand(t, external, "rev-parse", "HEAD")
			}
			if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			remoteHead := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			selected, err := s.Handback(context.Background(), before.ID)
			if err == nil || selected.State != workflow.NeedsAttention || len(selected.Handbacks) != 1 || selected.Handbacks[0].GrantedRound != 0 || selected.Handbacks[0].Pending {
				t.Fatalf("unsafe handback: %+v %v", selected, err)
			}
			data, err := os.ReadFile(filepath.Join(path, "manual.txt"))
			if err != nil || string(data) != "preserved" || remoteHead != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") {
				t.Fatal("unexpected state overwritten")
			}
		})
	}
}

func TestHandbackRequiresInteractiveAndOwnedProcessesExited(t *testing.T) {

	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprint(interactive), func(t *testing.T) {

			s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
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
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: testkit.AmbiguousTakeoverRunner{Runner: r}})
				if err != nil {
					t.Fatal(err)
				}
			}
			head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
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
			if head != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || testkit.GitCommand(t, path, "status", "--porcelain") == "" {
				t.Fatal("published despite process ownership")
			}
		})
	}
}

func TestHandbackPublicationAndGrantRecoverAfterRestart(t *testing.T) {

	for _, boundary := range []string{"commit", "push", "grant"} {
		t.Run(boundary, func(t *testing.T) {

			_, runtime, api, remote, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", testkit.DisputedReport)
			cfg.MaxRounds = 1
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("manual repair"), 0600); err != nil {
				t.Fatal(err)
			}
			requestCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Git: testkit.InterruptedHandbackGit{Manager: testkit.TestGitManager(runtime.Workspace), Boundary: boundary, Cancel: cancel}})
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
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
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
			if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "2" {
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

	_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", testkit.DisputedReport)
	cfg.MaxRounds = 1
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg, ConfigPath: "/tmp/test-config.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.ApiRequest(t, runtime.Workspace.Root, "GET", "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"/runs/", "/api/runs/"} {
		for _, attack := range []string{"missing token", "foreign origin"} {
			request := testkit.DashboardRequest(t, server, "POST", "http://127.0.0.1:7331"+action+before.ID+"/handback", nil)
			request.Header.Set("Origin", "http://127.0.0.1:7331")
			testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
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
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, "GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
	for _, text := range []string{"Hand back run", "Exit the interactive harness", "exactly one additional round", "original implementer session"} {
		if !strings.Contains(response.Body.String(), text) {
			t.Fatalf("missing disclosure %q", text)
		}
	}
	request := testkit.DashboardRequest(t, server, "POST", "http://127.0.0.1:7331/runs/"+before.ID+"/handback", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Selected Reviewing · round") || !strings.Contains(response.Body.String(), "Granted additional review round: 2") || strings.Contains(response.Body.String(), ">Hand back run</button>") {
		t.Fatalf("handback page: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+before.ID+"/handback", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var selected workflow.Run
	if err := json.Unmarshal(response.Body.Bytes(), &selected); err != nil || response.Code != 200 || len(selected.Handbacks) != 1 || selected.Handbacks[0].GrantedRound != 2 {
		t.Fatalf("API duplicate grant: %d %+v %v", response.Code, selected, err)
	}
}

func TestHandbackReusesUnfinishedReviewRound(t *testing.T) {

	s, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", testkit.DisputedReport)
	before := testkit.Finish(t, s, workflow.Active, workflow.Review)
	testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(`printf reviewer > review-only.txt; sleep 60`))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
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
	testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(testkit.ApprovedReview))
	// Continue against GitHub's freshly observed published head.
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ReviewRound != 1 || after.Review.Attempt != 2 || after.Review.SessionID == after.Implementer.SessionID {
		t.Fatalf("review was skipped: %+v", after)
	}
}

func TestHandbackPendingPublicationIsVisibleAndStopCancelsIt(t *testing.T) {

	s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
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
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, "GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
	for _, text := range []string{"Selected Reviewing · round", "Publication or reconciliation is incomplete", "Handback publication is pending"} {
		if !strings.Contains(response.Body.String(), text) {
			t.Fatalf("missing recovery detail %q", text)
		}
	}
	if err := s.Stop(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
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
				flow, script := testkit.TmuxFlow, `exit 1`
				if agent == "codex" {
					flow, script = testkit.TmuxCodexFlow, testkit.CodexIdentity+"\nexit 1"
				}
				s, runtime, _, remote, _, _ := flow(t, script)
				before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
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
				if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != want {
					t.Fatalf("manual publication: %s want %s", got, want)
				}
			})
		}
	}
}

func TestHandbackAfterRetryReviewsNewestManualCommitWithFractionalTimestamp(t *testing.T) {

	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, `case "$*" in *review-*) exit 1;; *) `+testkit.SuccessfulScript+`;; esac`)
	now := time.Date(2026, 10, 7, 12, 0, 0, 123450000, time.UTC)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
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
	testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(testkit.ApprovedReview))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	if err != nil || after.State != workflow.Active || after.Phase != workflow.Review || after.Review.Attempt != 2 || after.Review.TargetSHA != head {
		t.Fatalf("newer handback target lost: %+v %v", after, err)
	}
}

func TestHandbackReconcilesLabelsAfterPublicationSelection(t *testing.T) {

	_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", testkit.DisputedReport)
	cfg.MaxRounds = 1
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	api.Mutate = func(action, repo string, n int, label string) error {
		if action == "remove" && label == "agent-needs-attention" {
			return fmt.Errorf("label response lost")
		}
		return nil
	}
	selected, err := s.Handback(context.Background(), before.ID)
	if err == nil || selected.State != workflow.Active || len(selected.Handbacks) != 1 {
		t.Fatalf("fixture did not select before label failure: %+v %v", selected, err)
	}
	api.Mutate = nil
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") || !testkit.Has(api.Issues["owner/repo"][0], "agent-running") {
		t.Fatal("handback label bookkeeping was not recovered")
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || len(after.Handbacks) != 1 || after.Handbacks[0].GrantedRound != 2 {
		t.Fatalf("label recovery duplicated handback: %+v %v", after, err)
	}
}

func TestHandbackOpensConfiguredImplementAttemptWindow(t *testing.T) {

	script := `attempt_count_file="$0.attempt-count"
 attempt_count=0
 if [ -f "$attempt_count_file" ]; then attempt_count=$(cat "$attempt_count_file"); fi
 attempt_count=$((attempt_count+1))
 printf '%s' "$attempt_count" > "$attempt_count_file"
 if [ "$attempt_count" -lt 3 ]; then exit 1; fi
 ` + testkit.SuccessfulScript
	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
	before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	var err error
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Handback(context.Background(), before.ID)
	if err != nil || selected.Handbacks[0].AttemptFrom != 2 {
		t.Fatalf("fresh window: %+v %v", selected, err)
	}
	// A physical attempt after handback gets its own configured two-attempt
	// window, including after reconstructing the scheduler from saved state.
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		run, err := runtime.Workflow.Get(context.Background(), before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == workflow.NeedsAttention {
			t.Fatalf("handback's first failure exhausted max_attempts=2: physical attempt %d, %s", run.Implementer.Attempt, run.LastErrorCode)
		}
		return run.Implementer.Attempt == 3
	})
	after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.Implementer.SessionID != before.Implementer.SessionID || after.Implementer.Attempt != 3 || len(after.Handbacks) != 1 {
		t.Fatalf("attempt window lost identity/history: %+v", after)
	}
}

func TestHandbackPreservesUnsavedClosedAndMergedPRs(t *testing.T) {

	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprint(merged), func(t *testing.T) {

			s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, `exit 1`)
			before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			var err error
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.HandbackDiscoveryGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			if err := os.WriteFile(filepath.Join(path, "published.txt"), []byte("manual PR work"), 0600); err != nil {
				t.Fatal(err)
			}
			testkit.GitCommand(t, path, "add", ".")
			testkit.GitCommand(t, path, "commit", "-m", "manual PR work")
			testkit.GitCommand(t, path, "push", "origin", "HEAD:refs/heads/mergeyard/issue-7")
			head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			pr := &github.PullRequest{Number: 88, State: github.Closed, Merged: merged, URL: "https://github.com/owner/repo/pull/88"}
			pr.Head.Ref = "mergeyard/issue-7"
			pr.Head.Repo.FullName = "owner/repo"
			pr.Head.SHA = head
			api.Prs = map[string]*github.PullRequest{"mergeyard/issue-7": pr}
			if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("preserve this edit"), 0600); err != nil {
				t.Fatal(err)
			}
			selected, err := s.Handback(context.Background(), before.ID)
			if err == nil || selected.State != workflow.NeedsAttention || selected.LastErrorCode != "handback.pr_closed" {
				t.Fatalf("unsaved closed/merged PR treated as absent: %s/%s %v", selected.State, selected.Phase, err)
			}
			data, err := os.ReadFile(filepath.Join(path, "manual.txt"))
			if err != nil || string(data) != "preserve this edit" || testkit.GitCommand(t, path, "rev-parse", "HEAD") != head || testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != head {
				t.Fatal("closed PR work was published or overwritten")
			}
		})
	}
}

func TestHandbackPreservesAmbiguousUnsavedPRHistory(t *testing.T) {

	s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, `exit 1`)
	before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	var err error
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.AmbiguousHandbackGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Takeover(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Handback(context.Background(), before.ID)
	if err == nil || selected.State != workflow.NeedsAttention || selected.LastErrorCode != "handback.pr_ambiguous" || selected.PendingHandback() != nil {
		t.Fatalf("ambiguous history accepted: %+v %v", selected, err)
	}
	if testkit.GitCommand(t, path, "status", "--porcelain") == "" || testkit.GitCommand(t, remote, "ls-remote", "--heads", remote, "refs/heads/mergeyard/issue-7") != "" {
		t.Fatal("ambiguous PR history caused publication")
	}
}

func TestHandbackOpensBoundedReviewerAttemptWindow(t *testing.T) {

	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {

			reviewResult := `exit 1`
			if success {
				reviewResult = testkit.ApprovedReview
			}
			script := `case "$*" in *review-*)
 attempt_count_file="$0.review-count"
 attempt_count=0
 if [ -f "$attempt_count_file" ]; then attempt_count=$(cat "$attempt_count_file"); fi
 attempt_count=$((attempt_count+1))
 printf '%s' "$attempt_count" > "$attempt_count_file"
 if [ "$attempt_count" -lt 3 ]; then exit 1; fi
 ` + reviewResult + `;; *) ` + testkit.SuccessfulScript + `;; esac`
			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Takeover(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			selected, err := s.Handback(context.Background(), before.ID)
			if err != nil || selected.ReviewRound != 1 || selected.Handbacks[0].AttemptFrom != 2 {
				t.Fatalf("unfinished review window: %+v %v", selected, err)
			}
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				run, err := runtime.Workflow.Get(context.Background(), before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.State == workflow.NeedsAttention && run.Review.Attempt < 3 {
					t.Fatalf("handback prematurely exhausted reviewer budget at attempt %d", run.Review.Attempt)
				}
				return run.Review.Attempt == 3
			})
			state := workflow.NeedsAttention
			if success {
				state = workflow.WaitingForCI
			}
			after := testkit.Finish(t, s, state, workflow.Review)
			if after.Review.Attempt != 3 || after.ReviewRound != 1 || len(after.Handbacks) != 1 || after.Review.SessionID == after.Implementer.SessionID {
				t.Fatalf("review budget or independent identity lost: %+v", after)
			}
			if !success {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				after, _ = runtime.Workflow.Get(context.Background(), before.ID)
				if after.Review.Attempt != 3 {
					t.Fatal("handback budget authorized an extra attempt")
				}
			}
		})
	}
}

func TestHandbackCountsOrdinaryReplacementWhenMissingStartupPrecedesWindow(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				s, runtime, api, _, cfg, r := testkit.RecoveryFlow(t, agent, phase, "success")
				var before workflow.Run
				testkit.WaitForWithin(t, 30*time.Second, func() bool {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
					runs, err := s.Runs(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if len(runs) != 1 || runs[0].Phase != phase {
						return false
					}
					before = runs[0]
					if phase == workflow.Implement {
						return before.Implementer.Status == "failed" && strings.HasPrefix(before.Implementer.Error, "harness.session_resume_failed:")
					}
					return before.Review != nil && before.Review.Status == "failed" && strings.HasPrefix(before.Review.Error, "harness.session_resume_failed:")
				})
				if before.State != workflow.Active || len(before.SessionRecoveries) != 0 {
					t.Fatalf("replacement already launched: %+v", before)
				}
				// The missing startup is outside the handback window. Every subsequent
				// invocation completes startup and fails ordinarily, so max_attempts=1
				// must count the replacement and forbid a second ordinary execution.
				executable, script := cfg.Agents.Claude.Executable, `echo 'temporary execution failure' >&2; exit 1`
				if agent == "codex" {
					executable = cfg.Agents.Codex.Executable
					script = `printf '%s\n' '{"type":"thread.started","thread_id":"` + testkit.ReplacementID + `"}'
` + script
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Takeover(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				selected, err := s.Handback(context.Background(), before.ID)
				if err != nil || selected.Handbacks[0].AttemptFrom != 2 {
					t.Fatalf("new window: %+v %v", selected, err)
				}
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				after := testkit.Finish(t, s, workflow.NeedsAttention, phase)
				attempt := after.Implementer.Attempt
				if phase == workflow.Review {
					attempt = after.Review.Attempt
				}
				if attempt != 2 || len(after.SessionRecoveries) != 1 {
					t.Fatalf("max_attempts=1 authorized two ordinary executions after handback: physical attempt=%d recoveries=%d", attempt, len(after.SessionRecoveries))
				}
				for range 2 {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				saved, err := runtime.Workflow.Get(context.Background(), before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.State != workflow.NeedsAttention || len(saved.Handbacks) != 1 || len(saved.SessionRecoveries) != 1 {
					t.Fatalf("budget exhaustion lost on restart/reconcile: %+v", saved)
				}
			})
		}
	}
}

func TestHandbackDiscountsMissingStartupInsideFreshAttemptWindow(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				_, runtime, api, _, cfg, r := testkit.RecoveryFlow(t, agent, phase, "success")
				executable := cfg.Agents.Claude.Executable
				if agent == "codex" {
					executable = cfg.Agents.Codex.Executable
				}
				round := 0
				if phase == workflow.Review {
					round = 2
				}
				first := fmt.Sprintf("%s-%d-1", phase, round)
				second := fmt.Sprintf("%s-%d-2", phase, round)
				data, err := os.ReadFile(executable)
				if err != nil {
					t.Fatal(err)
				}
				// Exhaust the old one-attempt window with an ordinary failure.
				prefix := `case "$*" in *` + first + `*) echo 'temporary execution failure' >&2; exit 1;; esac
`
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+prefix+strings.TrimPrefix(string(data), "#!/bin/sh\n")), 0700); err != nil {
					t.Fatal(err)
				}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				before := testkit.Finish(t, s, workflow.NeedsAttention, phase)
				session := before.Implementer.SessionID
				if phase == workflow.Review {
					session = before.Review.SessionID
				}
				missing := `printf 'No conversation found with session ID: %s\n' "$3" >&2`
				identity := ""
				if agent == "codex" {
					missing = `echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + session + ` (code -32600)' >&2`
					identity = `printf '%s\n' '{"type":"thread.started","thread_id":"` + testkit.ReplacementID + `"}'`
				}
				script := `case "$*" in *` + second + `*) ` + missing + `; exit 1;; esac
` + identity + `
echo 'temporary execution failure' >&2; exit 1
`
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Repositories[0].Implementer.MaxAttempts = 2
				cfg.Repositories[0].Reviewer.MaxAttempts = 2
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Takeover(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				selected, err := s.Handback(context.Background(), before.ID)
				if err != nil || selected.Handbacks[0].AttemptFrom != 2 {
					t.Fatalf("new window: %+v %v", selected, err)
				}
				after := testkit.Finish(t, s, workflow.NeedsAttention, phase)
				attempt := after.Implementer.Attempt
				if phase == workflow.Review {
					attempt = after.Review.Attempt
				}
				// Startup 2 is discounted; ordinary executions 3 and 4 spend the two
				// configured attempts, while the next ordinary execution is forbidden.
				if attempt != 4 || len(after.SessionRecoveries) != 1 {
					t.Fatalf("in-window missing startup changed max_attempts=2: attempt=%d recoveries=%d", attempt, len(after.SessionRecoveries))
				}
			})
		}
	}
}
