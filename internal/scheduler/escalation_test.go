package scheduler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type escalationGitHub struct {
	*ciGitHub
	failBody  bool
	bodyCalls int
}

func (f *escalationGitHub) UpdateEscalation(_ context.Context, _ string, number int, snapshot escalation.Snapshot) error {
	f.bodyCalls++
	if f.failBody {
		return errors.New("GitHub unavailable")
	}
	for _, pr := range f.prs {
		if pr.Number == number {
			body, err := github.UpdateEscalationBody(pr.Body, snapshot)
			pr.Body = body
			return err
		}
	}
	return errors.New("PR missing")
}

type escalationGit struct {
	*managedgit.Manager
	unavailable bool
	diffCalls   int
}

func (g *escalationGit) EscalationDiff(ctx context.Context, run managedgit.Run, head string) ([]escalation.FileChange, error) {
	g.diffCalls++
	if g.unavailable {
		return nil, errors.New("diff unavailable")
	}
	return g.Manager.EscalationDiff(ctx, run, head)
}

func TestEscalationReadinessRestartAndSurfaces(t *testing.T) {
	ctx := context.Background()
	script := "mkdir -p .github/workflows; printf 'workflow\\n' > .github/workflows/ci.yml; printf '{}\\n' > package.json; " + successfulScript
	_, rt, base, _, cfg, r := localFlow(t, script)
	api := &escalationGitHub{ciGitHub: &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}, failBody: true}
	g := &escalationGit{Manager: managedgit.New(rt.Workspace)}
	deps := scheduler.Dependencies{GitHub: api, Git: g, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	pr := api.prs["mergeyard/issue-7"]
	pr.Body = "User prefix\n" + pr.Body + "\nUser suffix"
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := rt.Workflow.Get(ctx, run.ID)
	if err != nil || saved.State != workflow.ReadyToMerge || saved.Escalation == nil || len(saved.Escalation.Reasons) != 2 || saved.Escalation.Published || saved.Escalation.PublicationWarning == "" || saved.LastErrorCode != "" || api.readyCalls != 1 {
		t.Fatalf("advisory readiness: %+v %v", saved, err)
	}
	want := saved.Escalation
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	saved, err = rt.Workflow.Get(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(saved.Escalation, want) {
		t.Fatalf("lost persisted flags: %+v %v", saved.Escalation, err)
	}
	// Config edits do not change the saved flags for an already evaluated head.
	cfg.Repositories[0].Escalation = escalation.Rules{}
	api.failBody = false
	pr.Body += "\nHuman edit while offline"
	s, err = scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _ = rt.Workflow.Get(ctx, run.ID)
	if !saved.Escalation.Published || saved.Escalation.PublicationWarning != "" || g.diffCalls != 1 {
		t.Fatalf("restart reevaluated or failed publication: %+v calls=%d", saved.Escalation, g.diffCalls)
	}
	for _, text := range []string{"User prefix", "User suffix", "Human edit while offline", "Added feature", "Needs careful review", ".github/workflows/ci.yml", "package.json", run.ApprovedSHA} {
		if !strings.Contains(pr.Body, text) {
			t.Fatalf("PR lost %q: %s", text, pr.Body)
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/runs/" + run.ID, "/api/status"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
		for _, text := range []string{"ci/deploy", ".github/workflows/ci.yml", "dependency manifests", "package.json"} {
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), text) {
				t.Fatalf("%s missing %q: %s", path, text, response.Body.String())
			}
		}
		if path != "/api/status" && !strings.Contains(response.Body.String(), "Needs careful review") {
			t.Fatal("missing badge")
		}
	}
	// A newly approved head refreshes reasons and clears the old PR section.
	worktree := filepath.Join(root, "worktrees", "owner-repo", "issue-7")
	paths, _ := filepath.Glob(filepath.Join(root, "worktrees", "owner-repo", "*"))
	if len(paths) == 1 {
		worktree = paths[0]
	}
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("next approved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, worktree, "add", ".")
	gitCommand(t, worktree, "commit", "-m", "new approval")
	newHead := gitCommand(t, worktree, "rev-parse", "HEAD")
	if _, err := rt.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "ci.head_changed", Message: "New head requires approval"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: workflow.ReadyToMerge, Metadata: workflow.MetadataPatch{ApprovedSHA: &newHead}}); err != nil {
		t.Fatal(err)
	}
	pr.Head.SHA = newHead
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _ = rt.Workflow.Get(ctx, run.ID)
	if saved.Escalation.HeadSHA != newHead || len(saved.Escalation.Reasons) != 0 || strings.Contains(pr.Body, "Needs careful review") || g.diffCalls != 2 {
		t.Fatalf("stale flags: %+v", saved.Escalation)
	}
}

func TestEscalationUnavailableDiffDoesNotBlockAndRetries(t *testing.T) {
	_, rt, base, _, cfg, r := localFlow(t, successfulScript)
	api := &escalationGitHub{ciGitHub: &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}}
	g := &escalationGit{Manager: managedgit.New(rt.Workspace), unavailable: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Git: g, Runner: r, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.ReadyToMerge, workflow.Review)
	if run.Escalation == nil || run.Escalation.EvaluationWarning == "" || run.LastErrorCode != "" || api.bodyCalls != 0 {
		t.Fatalf("failed evaluation blocked run: %+v", run)
	}
	g.unavailable = false
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := rt.Workflow.Get(context.Background(), run.ID)
	if saved.Escalation.EvaluationWarning != "" || !saved.Escalation.Published || len(saved.Escalation.Reasons) != 0 {
		t.Fatalf("retry: %+v", saved.Escalation)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "Needs careful review") || strings.Contains(api.prs["mergeyard/issue-7"].Body, "Needs careful review") {
		t.Fatal("unflagged run shows badge")
	}
}
