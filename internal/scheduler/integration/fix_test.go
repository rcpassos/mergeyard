package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestFixAndDisputeResumeIndependentReview(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, fix, Commits string }{
		{"fixed and disputed", `printf fixed > feature.txt; printf added > new.txt; ` + testkit.FixedReport, "2"},
		{"no change dispute", testkit.DisputedReport, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(tc.fix))
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if run.ReviewRound != 2 || run.Review.Attempt != 1 || run.ApprovedSHA != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") {
				t.Fatalf("run=%+v", run)
			}
			if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != tc.Commits {
				t.Fatalf("commits=%s", got)
			}
			var implementer, firstReviewer, lastReviewer string
			var resumed bool
			runtime.DB.QueryRow("SELECT implementer_session_id FROM runs WHERE id=?", run.ID).Scan(&implementer)
			runtime.DB.QueryRow("SELECT v.session_id FROM review_attempts v JOIN phase_attempts a ON a.id=v.attempt_id WHERE a.round=1").Scan(&firstReviewer)
			runtime.DB.QueryRow("SELECT v.session_id,resumed_session FROM review_attempts v JOIN phase_attempts a ON a.id=v.attempt_id WHERE a.round=2").Scan(&lastReviewer, &resumed)
			if !resumed || firstReviewer != lastReviewer || implementer == lastReviewer {
				t.Fatal("review did not resume its independent conversation")
			}
			for _, phase := range []string{"fix-1-1", "review-2-1"} {
				input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", phase, "input.md"))
				for _, want := range []string{"Implement this", "F1", "F2", "Broken"} {
					if err != nil || !strings.Contains(string(input), want) {
						t.Fatalf("%s missing %s: %s %v", phase, want, input, err)
					}
				}
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts").Scan(&count)
			if count != 4 || !api.Prs["mergeyard/issue-7"].Draft {
				t.Fatalf("attempts=%d or prematurely ready", count)
			}
		})
	}
}

func TestFixFailuresAndAttemptsStayWithinRound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, fix, Code string
		attempts        int
	}{
		{"invalid schema", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Missing"}}'`, "phase.result_invalid", 2},
		{"unknown finding", strings.ReplaceAll(testkit.FixedReport, `"F1"`, `"unknown"`), "phase.result_invalid", 2},
		{"missing response", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Incomplete","responses":[]}}'`, "phase.result_invalid", 2},
		{"fixed without changes", testkit.FixedReport, "phase.result_invalid", 2},
		{"blocked", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":{"decision":"Clarify the required behavior","files":[],"options":[]},"status":"blocked","summary":"Need human input","responses":[]}}'`, "phase.blocked", 1},
		{"failed", `exit 1`, "phase.retries_exhausted", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(tc.fix))
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != tc.Code || run.ReviewRound != 1 || run.Fix == nil || run.Fix.Attempt != tc.attempts || run.ApprovedSHA != "" || len(run.FixHistory) != tc.attempts {
				t.Fatalf("run=%+v fix=%+v", run, run.Fix)
			}
			if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
				t.Fatalf("invalid fix pushed commits: %s", got)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFixAttemptRetryDoesNotSpendReviewRound(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				t.Parallel()

				fix := `case "$*" in *fix-1-1*) exit 1;; esac
 printf fixed > feature.txt
 ` + testkit.FixedReport
				_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, fix)
				cfg.Repositories[0].Implementer.MaxAttempts = 2
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || run.Fix.Attempt != 2 || run.Review.Attempt != 1 {
					t.Fatalf("round or attempt=%+v", run)
				}
			})
		}
	}
}

func TestBoundedReviewLoopDoesNotLaunchUnreviewableFix(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, max := range []int{1, 2} {
				t.Run(implementer+"/"+reviewer+"/"+string(rune('0'+max)), func(t *testing.T) {
					t.Parallel()

					remaining := strings.Replace(testkit.ChangesReview, `"prior_findings":[]`, `"prior_findings":[{"finding_id":"F1","outcome":"not_fixed","note":"Dispute rejected"},{"finding_id":"F2","outcome":"not_fixed","note":"Dispute rejected"}]`, 1)
					script := strings.Replace(testkit.LoopScript(testkit.DisputedReport), testkit.ApprovedAfterDisputesReview, remaining, 1)
					_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
					testkit.ReplacePhaseScript(t, cfg, workflow.Review, script)
					if implementer != reviewer {
						testkit.ReplacePhaseScript(t, cfg, workflow.Fix, script)
					}
					cfg.MaxRounds = max
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
					if run.ReviewRound != max || run.LastErrorCode != "review.max_rounds_exceeded" || !run.Review.Accepted || run.Review.Report.Status != "changes_required" {
						t.Fatalf("exhaustion=%+v", run)
					}
					if len(run.FixHistory) != max-1 {
						t.Fatalf("fixes=%d max=%d", len(run.FixHistory), max)
					}
				})
			}
		}
	}
}

func TestStopFixPreservesEditsAndPreventsPush(t *testing.T) {
	t.Parallel()

	gate := filepath.Join(t.TempDir(), "fix-started")
	script := `printf 'unfinished fix' > feature.txt
 printf ready > '` + gate + `'
 while :; do /bin/sleep 0.02; done`
	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(script))
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, func() bool { _, err := os.Stat(gate); return err == nil })
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil || !strings.Contains(ref.Name, "fix-1-1") {
		t.Fatalf("watch=%+v %v", ref, err)
	}
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run, err = runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || run.State != workflow.Stopped || run.ReviewRound != 1 || run.Fix.Status != "stopped" {
		t.Fatalf("stopped=%+v %v", run, err)
	}
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
	if err != nil || string(data) != "unfinished fix" {
		t.Fatalf("lost fix=%s %v", data, err)
	}
	if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "implemented" {
		t.Fatal("stopped fix published")
	}
}

func TestFixReportsVisibleInDetailStatusAndEvents(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport))
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Fix.Report == nil || len(run.Fix.Report.Responses) != 2 || run.FixHistory[0].Report.Responses[1].Resolution != "disputed" {
		t.Fatalf("responses=%+v", run.Fix)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, hx := range []bool{false, true} {
		request := testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil)
		if hx {
			request.Header.Set("HX-Request", "true")
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		for _, want := range []string{"Implementer fix", "F1 · fixed", "F2 · disputed", "Changed source", "Already safe", "Fixed &lt;script&gt;bug&lt;/script&gt;", "fix.completed", run.Fix.CommitSHA, "Prior findings: 1 fixed / 0 not fixed / 1 no longer applies.", "F1 · fixed", "F2 · no_longer_applies", "Dispute accepted"} {
			if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
				t.Fatalf("missing %s: %s", want, response.Body.String())
			}
		}
		if strings.Contains(response.Body.String(), "<script>bug</script>") {
			t.Fatal("unescaped fix text")
		}
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.ApiRequest(t, runtime.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Runs) != 1 || len(status.Runs[0].FixHistory) != 1 || status.Runs[0].Fix.Report.Responses[1].Resolution != "disputed" {
		t.Fatalf("status=%s", response.Body.String())
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range history {
		if event.Type == "phase.completed" && strings.Contains(string(event.Payload), `"resolution":"disputed"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("fix responses missing from durable events")
	}
}

func TestReviewerAdjudicatesDisputesAndSuppliesRemainingBlockers(t *testing.T) {
	t.Parallel()

	remaining := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[{"finding_id":"F1","outcome":"no_longer_applies","note":"Dispute accepted"},{"finding_id":"F2","outcome":"not_fixed","note":"Dispute rejected"}],"status":"changes_required","summary":"F1 dismissed, F2 remains","findings":[{"id":"F2","severity":"blocking","title":"Still broken","details":"Reviewer rejects dispute","file":null,"line":null}]}}'`
	lastFix := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Explain F2","responses":[{"finding_id":"F2","resolution":"disputed","note":"More evidence"}]}}'`
	script := `case "$*" in
 *review-1-*) ` + testkit.ChangesReview + `;;
 *review-2-*) ` + remaining + `;;
 *review-*) ` + strings.Replace(testkit.ApprovedReview, `"prior_findings":[]`, `"prior_findings":[{"finding_id":"F2","outcome":"no_longer_applies","note":"New evidence accepts dispute"}]`, 1) + `;;
 *fix-1-*) ` + testkit.DisputedReport + `;;
 *fix-2-*) ` + lastFix + `;;
 *) ` + testkit.SuccessfulScript + `;; esac`
	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, script)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.ReviewRound != 3 || len(run.FixHistory) != 2 || len(run.Fix.Findings) != 1 || run.Fix.Findings[0].ID != "F2" {
		t.Fatalf("adjudication=%+v", run)
	}
	if count := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); count != "1" {
		t.Fatalf("manufactured commits=%s", count)
	}
}

func TestExternalPushDuringFixRequiresAttentionAndPreservesBothBranches(t *testing.T) {
	t.Parallel()

	for _, stage := range []string{"running", "before commit", "before push", "after push"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()

			fix := `printf fixed > feature.txt; ` + testkit.FixedReport
			gate := filepath.Join(t.TempDir(), "started")
			if stage == "running" {
				fix = `printf fixed > feature.txt; printf started > '` + gate + `'; while :; do /bin/sleep 0.02; done`
			}
			_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(fix))
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
			if stage == "running" {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				testkit.WaitFor(t, func() bool { _, err := os.Stat(gate); return err == nil })
			} else {
				run = testkit.AdvanceFixUntil(t, s, runtime, run.ID, func(v workflow.Run) bool {
					if v.Fix == nil {
						return false
					}
					switch stage {
					case "before commit":
						return v.Fix.Status == "succeeded" && v.Fix.CommitSHA == ""
					case "before push":
						return v.Fix.CommitSHA != "" && !v.Fix.Pushed
					default:
						return v.Fix.Pushed
					}
				})
			}
			external := filepath.Join(t.TempDir(), "external")
			testkit.GitCommand(t, t.TempDir(), "clone", remote, external)
			testkit.GitCommand(t, external, "checkout", "-b", "human", "origin/mergeyard/issue-7")
			if err := os.WriteFile(filepath.Join(external, "human.txt"), []byte("human change"), 0600); err != nil {
				t.Fatal(err)
			}
			testkit.GitCommand(t, external, "add", ".")
			testkit.GitCommand(t, external, "commit", "-m", "human change")
			testkit.GitCommand(t, external, "push", "origin", "HEAD:refs/heads/mergeyard/issue-7")
			humanSHA := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			run = testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != "review.head_changed" || run.ApprovedSHA != "" || run.ReviewRound != 1 {
				t.Fatalf("external change=%+v", run)
			}
			if testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != humanSHA {
				t.Fatal("external push overwritten")
			}
			var path string
			runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
			data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil || string(data) != "fixed" {
				t.Fatalf("lost fix: %q %v", data, err)
			}
		})
	}
}

func TestFixRetainsImplementerCommitsAndIgnoredArtifacts(t *testing.T) {
	t.Parallel()

	fix := `printf 'cache.ignored\n' > .gitignore; printf cache > cache.ignored
 printf fixed > feature.txt
 git add feature.txt
 git -c user.name=Implementer -c user.email=implementer@example.invalid commit -m 'legitimate implementer fix' >&2
 printf new > added.txt
 ` + testkit.FixedReport
	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(fix))
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Env: map[string]string{"PATH": os.Getenv("PATH"), "GIT_CONFIG_GLOBAL": testkit.TestGitConfig(t), "GIT_CONFIG_NOSYSTEM": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "3" || !strings.Contains(testkit.GitCommand(t, remote, "log", "--format=%s", "main..mergeyard/issue-7"), "legitimate implementer fix") {
		t.Fatal("implementer commit lost or duplicated")
	}
	if strings.Contains(testkit.GitCommand(t, remote, "ls-tree", "-r", "--name-only", "mergeyard/issue-7"), "cache.ignored") {
		t.Fatal("ignored artifact published")
	}
	if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:added.txt") != "new" || run.ReviewRound != 2 {
		t.Fatalf("incomplete fix=%+v", run)
	}
}

func TestStopSerializesWithFixPushAndPreventsNextReview(t *testing.T) {
	t.Parallel()

	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport))
	g := testkit.HeldFixPush{Manager: testkit.TestGitManager(runtime.Workspace), Entered: make(chan struct{}), Release: make(chan struct{})}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Git: g, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
	testkit.AdvanceFixUntil(t, s, runtime, run.ID, func(v workflow.Run) bool { return v.Fix != nil && v.Fix.CommitSHA != "" })
	tick := make(chan error, 1)
	go func() { tick <- s.Tick(context.Background()) }()
	select {
	case <-g.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("push not entered")
	}
	stop := make(chan error, 1)
	go func() { stop <- s.Stop(context.Background(), run.ID) }()
	select {
	case err := <-stop:
		t.Fatalf("Stop bypassed shared gate: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(g.Release)
	if err := <-tick; err != nil {
		t.Fatal(err)
	}
	if err := <-stop; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Stopped || saved.ReviewRound != 1 || len(saved.FixHistory) != 1 {
		t.Fatalf("stop lost=%+v %v", saved, err)
	}
	if testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
		t.Fatal("commits duplicated")
	}
	var reviews int
	runtime.DB.QueryRow("SELECT count(*) FROM review_attempts").Scan(&reviews)
	if reviews != 1 {
		t.Fatal("Stop launched next review")
	}
}

func TestFixMissingSessionRecoveryIsBoundedAndReappliesFullInput(t *testing.T) {
	t.Parallel()

	fix := `case "$*" in *fix-1-1*) printf 'No conversation found with session ID: %s\n' "$3" >&2; exit 1;; esac
 case "$*" in *--resume*) exit 2;; esac
 printf fixed > feature.txt
 ` + testkit.FixedReport
	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(fix))
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Fix.Attempt != 2 || run.ReviewRound != 2 || len(run.FixHistory) != 2 || run.FixHistory[0].SessionID == run.Fix.SessionID || run.Fix.SessionID == run.Review.SessionID {
		t.Fatalf("session recovery=%+v", run)
	}
	input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "fix-1-2", "input.md"))
	if err != nil || !strings.Contains(string(input), "F1") || !strings.Contains(string(input), "Implement this") {
		t.Fatalf("fallback lost context: %s %v", input, err)
	}
}

func TestFixCompletionRoundAndEventRollBackTogether(t *testing.T) {
	t.Parallel()

	_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport))
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
	testkit.AdvanceFixUntil(t, s, runtime, run.ID, func(v workflow.Run) bool { return v.Fix != nil && v.Fix.Pushed })
	if _, err := runtime.DB.Exec(`CREATE TRIGGER reject_fix_completion BEFORE INSERT ON events WHEN NEW.type='fix.completed' BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("event rejection did not roll back")
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.Phase != workflow.Fix || saved.ReviewRound != 1 || !saved.Fix.Pushed {
		t.Fatalf("rollback leaked=%+v %v", saved, err)
	}
	if _, err := runtime.DB.Exec("DROP TRIGGER reject_fix_completion"); err != nil {
		t.Fatal(err)
	}
	saved = testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if saved.ReviewRound != 2 || saved.Fix.Attempt != 1 || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
		t.Fatalf("replayed=%+v", saved)
	}
}

func TestUnpublishedEditsAfterFixPushRequireAttention(t *testing.T) {
	t.Parallel()

	for _, stage := range []string{"after push", "before review"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport))
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
			testkit.AdvanceFixUntil(t, s, runtime, run.ID, func(v workflow.Run) bool { return v.Fix != nil && v.Fix.Pushed })
			phase := workflow.Fix
			if stage == "before review" {
				run = testkit.Finish(t, s, workflow.Active, workflow.Review)
				phase = workflow.Review
			}
			var path string
			runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
			if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("unpublished edit"), 0600); err != nil {
				t.Fatal(err)
			}
			run = testkit.Finish(t, s, workflow.NeedsAttention, phase)
			if run.LastErrorCode != "review.head_changed" || run.ApprovedSHA != "" {
				t.Fatalf("dirty approval=%+v", run)
			}
			data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil || string(data) != "unpublished edit" {
				t.Fatal("lost human edit")
			}
			var reviews int
			runtime.DB.QueryRow("SELECT count(*) FROM review_attempts").Scan(&reviews)
			if reviews != 1 {
				t.Fatal("launched review on unpublished work")
			}
		})
	}
}

func TestUnchangedTreeCannotClaimFindingFixed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, Script, localCommits string }{
		{"empty commit", `git -c user.name=Implementer -c user.email=implementer@example.invalid commit --allow-empty -m 'empty fix' >&2`, "2"},
		{"commit then revert", `printf temporary > feature.txt
 git -c user.name=Implementer -c user.email=implementer@example.invalid commit -am 'temporary fix' >&2
 git -c user.name=Implementer -c user.email=implementer@example.invalid revert --no-edit HEAD >&2`, "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fix := tc.Script + "\n" + testkit.FixedReport
			_, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(fix))
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Env: map[string]string{"PATH": os.Getenv("PATH"), "GIT_CONFIG_GLOBAL": testkit.TestGitConfig(t), "GIT_CONFIG_NOSYSTEM": "1"}})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != "phase.result_invalid" || run.ReviewRound != 1 || run.Fix.Attempt != 1 || run.Fix.Pushed || run.Fix.CommitSHA != "" {
				t.Fatalf("empty fix accepted: %+v fix=%+v", run, run.Fix)
			}
			if run.Fix.Report == nil || run.Fix.Report.Responses[0].Resolution != "fixed" {
				t.Fatal("fix report not preserved")
			}
			// The rejection rewrites the attempt's error and status; the frozen
			// publication eligibility must survive so publication never falls
			// back to an older report.
			var eligible int
			if err := runtime.DB.QueryRow("SELECT report_eligible FROM phase_attempts WHERE run_id=? AND phase='fix'", run.ID).Scan(&eligible); err != nil || eligible != 1 {
				t.Fatalf("rejected fix lost its publication eligibility: %d %v", eligible, err)
			}
			if testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
				t.Fatal("empty fix published")
			}
			var path string
			if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if testkit.GitCommand(t, path, "rev-list", "--count", "main..HEAD") != tc.localCommits {
				t.Fatal("implementer commit not preserved locally")
			}
			var reviews int
			if err := runtime.DB.QueryRow("SELECT count(*) FROM review_attempts").Scan(&reviews); err != nil {
				t.Fatal(err)
			}
			if reviews != 1 {
				t.Fatal("invalid fix consumed another review")
			}
		})
	}
}
