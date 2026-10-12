package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestRetryGuidanceSurvivesRestartAndReachesLaterPhases(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `if [ ! -f retry-marker ]; then printf preserved > retry-marker; exit 1; fi
`+loopScript(`printf fixed > feature.txt; `+fixedReport))
	deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	note := "Use the revised behaviour; $(touch injected); `touch injected`\n<script>alert('guidance')</script>"
	server, err := web.NewWithOperations(rt.Events, rt.Scheduler, s, rt.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+before.ID+"/retry", strings.NewReader(url.Values{"note": {note}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	statusResponse := httptest.NewRecorder()
	server.ServeHTTP(statusResponse, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-CSRF-Token", status.Token)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var selected map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &selected); err != nil || response.Code != 200 {
		t.Fatalf("retry: %d %s %v", response.Code, response.Body, err)
	}
	if !strings.Contains(response.Body.String(), "guidance") {
		t.Fatalf("retry did not store guidance: %s", response.Body)
	}
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	s, err = scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	for _, phase := range []string{"implement-0-2", "review-1-1", "fix-1-1", "review-2-1"} {
		path := filepath.Join(root, "runs", after.ID, "phases", phase, "input.md")
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "## Human guidance") || !strings.Contains(string(data), note) {
			t.Fatalf("%s lost guidance: %s %v", phase, data, err)
		}
		wrapper, err := os.ReadFile(filepath.Join(filepath.Dir(path), "wrapper.sh"))
		if err != nil || strings.Contains(string(wrapper), "touch injected") || strings.Contains(string(wrapper), "alert('guidance')") {
			t.Fatalf("%s interpolated guidance into commands: %s %v", phase, wrapper, err)
		}
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Name() == "injected" {
			t.Fatalf("guidance executed: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(runs)
	if err != nil || !strings.Contains(string(snapshot), `"author":"user"`) || !strings.Contains(string(snapshot), `"created_at":`) {
		t.Fatalf("history lost attributed guidance: %s %v", snapshot, err)
	}
}

func TestDashboardRetryGuidanceIsOptionalAndEscapedInHistory(t *testing.T) {
	s, rt, _, _, cfg, _ := localFlow(t, `exit 1`)
	before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), `name="note"`) || !strings.Contains(page.Body.String(), "Human guidance (optional)") {
		t.Fatalf("missing guidance field: %d %s", page.Code, page.Body)
	}
	statusResponse := httptest.NewRecorder()
	server.ServeHTTP(statusResponse, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	note := "Keep the intended behaviour\n<script>alert('guidance')</script>"
	request := httptest.NewRequest("POST", "http://127.0.0.1:7331/runs/"+before.ID+"/retry", strings.NewReader(url.Values{"note": {note}, "csrf_token": {status.Token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("HX-Request", "true")
	page = httptest.NewRecorder()
	server.ServeHTTP(page, request)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Human guidance · user") || !strings.Contains(page.Body.String(), html.EscapeString(note)) || strings.Contains(page.Body.String(), "<script>alert") {
		t.Fatalf("guidance missing or unescaped: %d %s", page.Code, page.Body)
	}
	run, err := rt.Workflow.Get(context.Background(), before.ID)
	if err != nil || len(run.Retries) != 1 || run.Retries[0].Guidance == nil || run.Retries[0].Guidance.Text != note {
		t.Fatalf("dashboard guidance not persisted: %+v %v", run, err)
	}
}

func TestRetryKeepsAllGuidanceWithoutDuplicatingOrReplacingNotes(t *testing.T) {
	s, rt, _, _, _, _ := localFlow(t, `exit 1`)
	ctx := context.Background()
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	first := " First decision: preserve existing behaviour.\n"
	if _, err := s.Retry(ctx, run.ID, first); err != nil {
		t.Fatal(err)
	}
	repeated, err := s.Retry(ctx, run.ID, first)
	if err != nil || len(repeated.Retries) != 1 {
		t.Fatalf("duplicate request: %+v %v", repeated, err)
	}
	if _, err := s.Retry(ctx, run.ID, "replacement while active"); err == nil {
		t.Fatal("silently discarded new guidance while active")
	}
	finish(t, s, workflow.NeedsAttention, workflow.Implement)
	second := "Second decision: use a narrower change."
	if _, err := s.Retry(ctx, run.ID, second); err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if _, err := s.Retry(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if len(after.Retries) != 3 || after.Retries[0].Guidance.Text != first || after.Retries[1].Guidance.Text != second || after.Retries[2].Guidance != nil {
		t.Fatalf("history replaced or duplicated notes: %+v", after.Retries)
	}
	path := filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", "implement-0-4", "input.md")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), first) || !strings.Contains(string(data), second) || strings.Index(string(data), first) >= strings.Index(string(data), second) {
		t.Fatalf("no-note Retry lost ordered guidance: %s %v", data, err)
	}
}

func TestPendingRetryGuidanceSurvivesRestartWithoutReplacement(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `if [ ! -f retry-marker ]; then printf preserved > retry-marker; exit 1; fi
`+successfulScript)
	git := &retryCancelGit{Manager: managedgit.New(rt.Workspace)}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Git: git, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	git.cancel = cancel
	note := "Continue with the user’s saved decision."
	if _, err := s.Retry(ctx, run.ID, note); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted reconciliation: %v", err)
	}
	if _, err := s.Retry(context.Background(), run.ID, "replace the pending note"); err == nil {
		t.Fatal("replaced pending guidance")
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.PendingRetry() == nil || saved.PendingRetry().Guidance.Text != note {
		t.Fatalf("lost pending guidance: %+v %v", saved, err)
	}
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if len(after.Retries) != 1 || after.Retries[0].Pending || after.Retries[0].Guidance.Text != note {
		t.Fatalf("restart duplicated or lost intent: %+v", after.Retries)
	}
	data, err := os.ReadFile(filepath.Join(root, "runs", run.ID, "phases", "review-1-1", "input.md"))
	if err != nil || !strings.Contains(string(data), note) {
		t.Fatalf("review lost pending guidance: %s %v", data, err)
	}
}
