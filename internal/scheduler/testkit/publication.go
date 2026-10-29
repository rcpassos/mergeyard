package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
)

type ReportGitHub struct {
	BranchGitHub
	Bodies    []string
	Failure   bool
	Code      string
	Calls     int
	Ambiguous bool
	Before    func()
}

func (f *ReportGitHub) EnsureReportComment(_ context.Context, _ string, _ int, body string) (*github.Comment, error) {
	f.Calls++
	if f.Before != nil {
		f.Before()
	}
	if f.Failure {
		code := f.Code
		if code == "" {
			code = "github.unavailable"
		}
		return nil, &fault.Error{Code: code, Message: "secret token <script> unsafe upstream diagnostic"}
	}
	for i, old := range f.Bodies {
		if old == body {
			return &github.Comment{ID: int64(i + 1), URL: fmt.Sprintf("https://github.com/owner/repo/pull/101#issuecomment-%d", i+1), Body: old}, nil
		}
	}
	f.Bodies = append(f.Bodies, body)
	if f.Ambiguous {
		f.Ambiguous = false
		f.Failure = true
		return nil, &fault.Error{Code: "github.unavailable", Message: "Lost success response"}
	}
	return &github.Comment{ID: int64(len(f.Bodies)), URL: fmt.Sprintf("https://github.com/owner/repo/pull/101#issuecomment-%d", len(f.Bodies)), Body: body}, nil
}

// Retain the publishing boundary while exercising the CI boundary on the same PR.
type PublishingCIGitHub struct {
	*ReportGitHub
	Checks *CiGitHub
}

func (f PublishingCIGitHub) PullRequestEvidence(ctx context.Context, repo string, pr github.PullRequest) (ci.Evidence, error) {
	return f.Checks.PullRequestEvidence(ctx, repo, pr)
}
func (f PublishingCIGitHub) MarkReady(ctx context.Context, repo string, n int) error {
	return f.Checks.MarkReady(ctx, repo, n)
}

func SeedReportPublication(t *testing.T, rt *app.Runtime, id, state string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,pr_number) VALUES(?,'owner/repo',7,?,?)`, id, state, pr); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES(?,?,'review','reviewer',1,1,'claude','succeeded','')`, id+"-attempt", id); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("<!-- mergeyard:report:%s -->", strings.ReplaceAll(id, "-", ""))
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body) VALUES(?,?,'owner/repo',?,'review',1,1,?)`, id+"-attempt", id, pr, marker+"\nReport"); err != nil {
		t.Fatal(err)
	}
}

func SeedAttentionPublication(t *testing.T, rt *app.Runtime, id, state string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,pr_number) VALUES(?,'owner/repo',7,?,?)`, id, state, pr); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"run":{"id":%q,"repository":"owner/repo","issue_number":7,"pr_number":%d,"state":%q,"human_action":{"decision":"Choose","files":[],"options":["Retry"]}}}`, id, pr, state)
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO events(run_id,type,payload_json) VALUES(?,'run.needs_attention',?)`, id, payload); err != nil {
		t.Fatal(err)
	}
}

func CommentFlow(t *testing.T, publisher *ReportGitHub, now func() time.Time) (*scheduler.Scheduler, *app.Runtime, config.Config) {
	t.Helper()
	api := &FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {{Number: 7, State: github.Open}}}, Prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open},
	}, Head: func(string) string { return "" }}
	_, rt := Fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PRComments = true
	publisher.BranchGitHub = BranchGitHub{api}
	s, err := TestScheduler(cfg, SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt, cfg
}

// ghRefusal is what gh reports for a refused request: the status line and a
// summary on stderr, and the response body on stdout.
type GhRefusal struct {
	Stderr string
	Stdout string
}

// ghComments answers the gh calls that publish a report comment: it lists the
// comments of an issue and creates one. A create can be refused with GitHub's
// own answer, so the scheduler classifies what GitHub actually said.
type GhComments struct {
	Mu       sync.Mutex
	Posts    int
	Bodies   []string
	Refusals []GhRefusal
}

func (g *GhComments) Run(_ context.Context, input []byte, args ...string) ([]byte, []byte, error) {
	method, endpoint := GhCall(args)
	switch {
	case method == "GET" && strings.HasSuffix(endpoint, "/comments"):
		return []byte("[]"), nil, nil
	case method == "POST" && strings.HasSuffix(endpoint, "/comments"):
		g.Mu.Lock()
		defer g.Mu.Unlock()
		g.Posts++
		if len(g.Refusals) > 0 {
			refusal := g.Refusals[0]
			g.Refusals = g.Refusals[1:]
			return []byte(refusal.Stdout), []byte(refusal.Stderr), errors.New("exit status 1")
		}
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(input, &payload); err != nil {
			return nil, nil, err
		}
		g.Bodies = append(g.Bodies, payload.Body)
		comment, err := json.Marshal(map[string]any{
			"id":       len(g.Bodies),
			"html_url": fmt.Sprintf("https://github.com/owner/repo/pull/101#issuecomment-%d", len(g.Bodies)),
			"body":     payload.Body,
		})
		return comment, nil, err
	}
	return nil, nil, fmt.Errorf("unexpected gh call: %v", args)
}

// published reports how many comments were created and their bodies.
func (g *GhComments) Published() (int, []string) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	return g.Posts, append([]string(nil), g.Bodies...)
}

// ghCall reads the method and the endpoint of one gh invocation.
func GhCall(args []string) (string, string) {
	method, endpoint := "", ""
	for i, arg := range args {
		switch {
		case arg == "--method" && i+1 < len(args):
			method = args[i+1]
		case strings.HasPrefix(arg, "repos/"):
			endpoint = strings.SplitN(arg, "?", 2)[0]
		}
	}
	return method, endpoint
}

// ghPublisher publishes report comments through the gh adapter, so the
// scheduler sees GitHub's own answers instead of a prepared code.
type GhPublisher struct {
	BranchGitHub
	Client *github.Client
}

func (p *GhPublisher) EnsureReportComment(ctx context.Context, repository string, number int, body string) (*github.Comment, error) {
	return p.Client.EnsureReportComment(ctx, repository, number, body)
}

// ghCommentFlow runs a scheduler whose report comments travel through the gh
// adapter and the given runner.
func GhCommentFlow(t *testing.T, runner github.CommandRunner, now func() time.Time) (*scheduler.Scheduler, *app.Runtime) {
	t.Helper()
	api := &FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {{Number: 7, State: github.Open}}}, Prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open},
	}, Head: func(string) string { return "" }}
	_, rt := Fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PRComments = true
	publisher := &GhPublisher{BranchGitHub: BranchGitHub{api}, Client: github.New(runner)}
	s, err := TestScheduler(cfg, SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt
}
