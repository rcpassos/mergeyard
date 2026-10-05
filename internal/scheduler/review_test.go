package scheduler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const approvedReview = `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":1,"status":"approved","summary":"Safe <script>review</script>","findings":[{"id":"R1-F1","severity":"warning","title":"Optional","details":"Later","file":null,"line":null}]}}'`

func reviewScript(review string) string {
	return `case "$*" in *review-*)
` + review + `
;; *)
` + successfulScript + `
;; esac`
}

func TestFirstReviewApprovesPinnedHeadWithIndependentSettings(t *testing.T) {
	s, runtime, api, remote, cfg, r := localFlow(t, reviewScript(approvedReview))
	cfg.Repositories[0].Reviewer.Model = "review-model"
	cfg.Repositories[0].Reviewer.Effort = "high"
	cfg.Repositories[0].Reviewer.Skills = []string{"review-skill"}
	var err error
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	sha := gitCommand(t, remote, "rev-parse", "refs/heads/mergeyard/issue-7")
	if run.ApprovedSHA != sha || run.Review == nil || !run.Review.Accepted || run.Review.Report.Status != "approved" || len(run.Review.Report.Findings) != 1 {
		t.Fatalf("approval=%+v review=%+v", run, run.Review)
	}
	if run.Review.Model != "review-model" || run.Review.Effort != "high" || strings.Join(run.Review.Skills, ",") != "review-skill" {
		t.Fatalf("review settings=%+v", run.Review)
	}
	var implementer string
	if err := runtime.DB.QueryRow("SELECT implementer_session_id FROM runs WHERE id=?", run.ID).Scan(&implementer); err != nil {
		t.Fatal(err)
	}
	if run.Review.SessionID == implementer || run.Review.SessionID == "" {
		t.Fatal("reviewer shared implementer identity")
	}
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var attempts, runs int
	runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts").Scan(&attempts)
	runtime.DB.QueryRow("SELECT count(*) FROM runs").Scan(&runs)
	if attempts != 2 || runs != 1 || api.creations != 1 || !api.prs["mergeyard/issue-7"].Draft {
		t.Fatalf("duplicate work or premature readiness: %d %d %+v", attempts, runs, api)
	}
	input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "review-1-1", "input.md"))
	if err != nil || !strings.Contains(string(input), sha) || !strings.Contains(string(input), "feature.txt") || !strings.Contains(string(input), "Implement this") {
		t.Fatalf("review context=%s err=%v", input, err)
	}
}

func TestReviewChangesRequiredPreparesFixWithoutLaunchingIt(t *testing.T) {
	report := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"changes_required","summary":"Fix bug","findings":[{"id":"R1-F1","severity":"blocking","title":"Bug","details":"Broken","file":"feature.txt","line":1}]}}'`
	s, runtime, api, _, _, _ := localFlow(t, reviewScript(report))
	run := finish(t, s, workflow.Active, workflow.Fix)
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var attempts int
	runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts").Scan(&attempts)
	if attempts != 2 || run.ApprovedSHA != "" || run.Review.Report.Status != "changes_required" || !api.prs["mergeyard/issue-7"].Draft {
		t.Fatalf("fix path launched incomplete work: %+v attempts=%d", run, attempts)
	}
}

func TestReviewerMutationsRestoreAndRetryWithinSameRound(t *testing.T) {
	for _, mutation := range []struct{ name, script string }{
		{"working files", `printf 'tampered' > feature.txt; printf 'new' > reviewer.txt`},
		{"commit", `printf 'tampered' > feature.txt; git add .; git commit -m 'reviewer commit'`},
		{"reset commit", `old=$(git rev-parse HEAD); git -c user.name=Reviewer -c user.email=reviewer@example.invalid commit --allow-empty -m 'reviewer commit'; git reset --soft "$old"`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			script := `case "$*" in *review-1-1*) ` + mutation.script + `;; esac
` + approvedReview
			_, runtime, api, remote, cfg, r := localFlow(t, reviewScript(script))
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"PATH": os.Getenv("PATH"), "GIT_CONFIG_GLOBAL": os.Getenv("GIT_CONFIG_GLOBAL"), "GIT_CONFIG_NOSYSTEM": "1"}})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if run.Review.Attempt != 2 || run.ReviewRound != 1 || run.ApprovedSHA != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") {
				t.Fatalf("retry identity=%+v review=%+v", run, run.Review)
			}
			var firstContaminated, firstRestored, firstAccepted bool
			if err := runtime.DB.QueryRow("SELECT contaminated,restored,accepted FROM review_attempts v JOIN phase_attempts a ON a.id=v.attempt_id WHERE a.phase='review' AND a.attempt=1").Scan(&firstContaminated, &firstRestored, &firstAccepted); err != nil {
				t.Fatal(err)
			}
			if !firstContaminated || !firstRestored || firstAccepted {
				t.Fatal("contaminated attempt accepted or not restored")
			}
			paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
			if got := gitCommand(t, paths[0], "show", "HEAD:feature.txt"); got != "implemented" {
				t.Fatal("reviewer commit retained")
			}
			if data, err := os.ReadFile(filepath.Join(paths[0], "feature.txt")); err != nil || string(data) != "implemented\n" {
				t.Fatal("reviewer source edit retained")
			}
			if _, err := os.Stat(filepath.Join(paths[0], "reviewer.txt")); !os.IsNotExist(err) {
				t.Fatal("reviewer nonignored file retained")
			}
		})
	}
}

func TestInvalidAndFailedReviewAttemptsAreBoundedAndBlockedDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name, report, code string
		attempts           int
	}{
		{"invalid", `{"schema_version":1,"status":"approved","summary":"Missing findings"}`, "phase.result_invalid", 2},
		{"failed", `{"schema_version":1,"status":"failed","summary":"Failure","findings":[]}`, "phase.failed", 2},
		{"blocked", `{"schema_version":1,"status":"blocked","summary":"Need input","findings":[]}`, "phase.blocked", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, runtime, api, _, cfg, r := localFlow(t, reviewScript(`printf '%s\n' '{"type":"result","is_error":false,"structured_output":`+tc.report+`}'`))
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.NeedsAttention, workflow.Review)
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if run.LastErrorCode != tc.code || run.Review.Attempt != tc.attempts || run.ReviewRound != 1 || run.ApprovedSHA != "" {
				t.Fatalf("bounded review=%+v %+v", run, run.Review)
			}
		})
	}
}

func TestExternalHeadChangeStopsReviewAndDiscardsApproval(t *testing.T) {
	s, runtime, api, _, _, _ := localFlow(t, reviewScript(`while :; do /bin/sleep 0.02; done
`+approvedReview))
	run := finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.prs["mergeyard/issue-7"].Head.SHA = strings.Repeat("a", 40)
	run = finish(t, s, workflow.NeedsAttention, workflow.Review)
	if run.LastErrorCode != "review.head_changed" || run.ApprovedSHA != "" || run.Review.Accepted || !run.Review.Restored {
		t.Fatalf("stale approval=%+v %+v", run, run.Review)
	}
	var count int
	runtime.DB.QueryRow("SELECT count(*) FROM review_attempts").Scan(&count)
	if count != 1 {
		t.Fatal("head change relaunched review")
	}
}

func TestStopAndWatchReviewUseSharedOperations(t *testing.T) {
	s, runtime, _, _, _, r := localFlow(t, reviewScript(`printf tamper > feature.txt
while :; do /bin/sleep 0.02; done`))
	run := finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ref.Name, "review-1-1") {
		t.Fatalf("watch=%+v", ref)
	}
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Stopped || saved.ApprovedSHA != "" {
		t.Fatalf("stop=%+v %v", saved, err)
	}
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil || status.State == "running" {
		t.Fatalf("review survived stop: %+v %v", status, err)
	}
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
	if err != nil || string(data) != "implemented\n" {
		t.Fatal("stop lost implemented code")
	}
}

func TestReviewHTTPDetailStatusAndLiveUpdates(t *testing.T) {
	s, runtime, _, _, cfg, _ := localFlow(t, reviewScript(approvedReview))
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, hx := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil)
		if hx {
			request.Header.Set("HX-Request", "true")
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		body := response.Body.String()
		for _, text := range []string{"Independent review", "Verdict: approved", "R1-F1", "Optional", run.Review.SessionID, run.ApprovedSHA, "review.completed", "Safe &lt;script&gt;review&lt;/script&gt;"} {
			if response.Code != 200 || !strings.Contains(body, text) {
				t.Fatalf("missing %q in HTTP %d: %s", text, response.Code, body)
			}
		}
		if strings.Contains(body, "<script>review</script>") {
			t.Fatal("review text was not escaped")
		}
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Runs) != 1 || status.Runs[0].Review.Report.Status != "approved" {
		t.Fatalf("status lost review: %s", response.Body.String())
	}
	// Review notifications must refresh the existing page over its SSE connection.
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/static/app.js", nil))
	if !strings.Contains(response.Body.String(), "review.completed") || !strings.Contains(response.Body.String(), "review.restored") {
		t.Fatal("live page does not subscribe to review updates")
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "http://evil.example/runs/"+run.ID, nil),
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/runs/"+run.ID+"/stop", nil),
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatal("review pages bypassed host/CSRF checks")
		}
	}
}

func TestReviewPreservesPreexistingWorkAndPermitsIgnoredBuildOutput(t *testing.T) {
	s, runtime, _, _, _, _ := localFlow(t, reviewScript(`/bin/mkdir -p build-output; printf cache > build-output/cache
`+approvedReview))
	run := finish(t, s, workflow.Active, workflow.Review)
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("build-output/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".gitignore")
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("preexisting work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("existing untracked"), 0600); err != nil {
		t.Fatal(err)
	}
	before := gitCommand(t, path, "status", "--porcelain", "--untracked-files=all")
	run = finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Review.Contaminated || before != gitCommand(t, path, "status", "--porcelain", "--untracked-files=all") {
		t.Fatal("preexisting work contaminated or changed")
	}
	if data, err := os.ReadFile(filepath.Join(path, "build-output", "cache")); err != nil || string(data) != "cache" {
		t.Fatal("test execution/build output was not permitted")
	}
}
