package testkit

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type FakeGitHub struct {
	GetIssueHook     func(string, int) (github.Issue, error)
	Comments         func(string, int, []string) (github.IssueComments, error)
	Trust            func(github.Issue) (github.TrustDecision, error)
	TrustComments    map[int]string
	OverrideComments map[int]string
	LeftoverComments map[int]LeftoverComment
	Issues           map[string][]github.Issue
	Blockers         map[int][]github.Issue
	Prs              map[string]*github.PullRequest
	Creations        int
	Head             func(string) string
	Mutate           func(action, repo string, n int, label string) error
	ListHook         func()             // runs inside each label-filtered listing request
	ListError        func(string) error // fails one repository's label-filtered listing
	Calls            GithubCalls
	Listings         []ListingRequest
}

// leftoverComment records the explanation a stopped preparation left on an
// issue.
type LeftoverComment struct {
	RunID, Branch, Worktree, Reason string
	Unverified                      bool
}

// listingRequest records one label-filtered listing request.
type ListingRequest struct {
	Repo   string
	Labels []string
}

// githubCalls records boundary traffic so tests can assert what a tick spends.
type GithubCalls struct {
	IssueListings, IssueReads, BlockerReads int
	TrustVerifications, TrustRechecks       int
}

func (f *FakeGitHub) ListIssueComments(_ context.Context, repo string, n int, trusted []string) (github.IssueComments, error) {
	if f.Comments != nil {
		return f.Comments(repo, n, trusted)
	}
	return github.IssueComments{}, nil
}

func (f *FakeGitHub) VerifyIssueTrust(_ context.Context, _ string, issue github.Issue, _ string, _ []string) (github.TrustDecision, error) {
	f.Calls.TrustVerifications++
	return f.TrustDecision(issue)
}
func (f *FakeGitHub) RecheckIssueTrust(_ context.Context, _ string, issue github.Issue, _ string, _ []string) (github.TrustDecision, error) {
	f.Calls.TrustRechecks++
	return f.TrustDecision(issue)
}
func (f *FakeGitHub) TrustDecision(issue github.Issue) (github.TrustDecision, error) {
	if f.Trust != nil {
		return f.Trust(issue)
	}
	return github.TrustDecision{Allowed: true}, nil
}
func (f *FakeGitHub) EnsureTrustRefusalComment(_ context.Context, _ string, n int, reason string) error {
	if f.TrustComments == nil {
		f.TrustComments = make(map[int]string)
	}
	if _, exists := f.TrustComments[n]; !exists {
		f.TrustComments[n] = reason
	}
	return nil
}

// EnsureStopLeftoversComment records the explanation of the leftovers a
// stopped preparation left behind.
func (f *FakeGitHub) EnsureStopLeftoversComment(_ context.Context, _ string, n int, runID string, leftovers github.StoppedLeftovers) error {
	if f.LeftoverComments == nil {
		f.LeftoverComments = make(map[int]LeftoverComment)
	}
	if _, exists := f.LeftoverComments[n]; !exists {
		f.LeftoverComments[n] = LeftoverComment{RunID: runID, Branch: leftovers.Branch, Worktree: leftovers.Worktree, Reason: leftovers.Reason, Unverified: leftovers.Unverified}
	}
	return nil
}

// ListLabeledIssues mirrors GitHub's label filter: the repository's open
// issues carrying any requested label, in adapter-owned order.
func (f *FakeGitHub) ListLabeledIssues(_ context.Context, repo string, labels []string) ([]github.Issue, error) {
	if f.ListHook != nil {
		f.ListHook()
	}
	if f.ListError != nil {
		if err := f.ListError(repo); err != nil {
			return nil, err
		}
	}
	f.Calls.IssueListings++
	f.Listings = append(f.Listings, ListingRequest{Repo: repo, Labels: append([]string(nil), labels...)})
	var issues []github.Issue
	for _, issue := range f.Issues[repo] {
		if issue.State != github.Open {
			continue
		}
		for _, label := range labels {
			if Has(issue, label) {
				issues = append(issues, issue)
				break
			}
		}
	}
	return issues, nil
}
func (f *FakeGitHub) GetIssue(_ context.Context, repo string, n int) (github.Issue, error) {
	f.Calls.IssueReads++
	if f.GetIssueHook != nil {
		return f.GetIssueHook(repo, n)
	}
	for _, issue := range f.Issues[repo] {
		if issue.Number == n {
			return issue, nil
		}
	}
	return github.Issue{}, &fault.Error{Code: "github.not_found", Message: "Issue not found"}
}
func (f *FakeGitHub) GetPullRequest(_ context.Context, _ string, n int) (*github.PullRequest, error) {
	for _, pr := range f.Prs {
		if pr.Number == n {
			return pr, nil
		}
	}
	return nil, &fault.Error{Code: "github.not_found", Message: "PR not found"}
}
func (f *FakeGitHub) UnresolvedBlockers(_ context.Context, _ string, n int) ([]github.Issue, error) {
	f.Calls.BlockerReads++
	return f.Blockers[n], nil
}
func (f *FakeGitHub) AddLabel(_ context.Context, repo string, n int, label string) error {
	if f.Mutate != nil {
		if err := f.Mutate("add", repo, n, label); err != nil {
			return err
		}
	}
	for i := range f.Issues[repo] {
		if f.Issues[repo][i].Number == n {
			if !Has(f.Issues[repo][i], label) {
				f.Issues[repo][i].Labels = append(f.Issues[repo][i].Labels, github.Label{Name: label})
			}
		}
	}
	return nil
}
func (f *FakeGitHub) RemoveLabel(_ context.Context, repo string, n int, label string) error {
	if f.Mutate != nil {
		if err := f.Mutate("remove", repo, n, label); err != nil {
			return err
		}
	}
	for i := range f.Issues[repo] {
		if f.Issues[repo][i].Number == n {
			var labels []github.Label
			for _, l := range f.Issues[repo][i].Labels {
				if l.Name != label {
					labels = append(labels, l)
				}
			}
			f.Issues[repo][i].Labels = labels
		}
	}
	return nil
}
func (f *FakeGitHub) FindOpenPullRequest(_ context.Context, _ string, branch string) (*github.PullRequest, error) {
	return f.Prs[branch], nil
}
func (f *FakeGitHub) FindPullRequest(_ context.Context, _ string, branch string) (*github.PullRequest, error) {
	return f.Prs[branch], nil
}
func (f *FakeGitHub) CreateDraftPullRequest(_ context.Context, repo, branch, base string, content github.PullRequestContent) (*github.PullRequest, error) {
	f.Creations++
	if f.Prs == nil {
		f.Prs = map[string]*github.PullRequest{}
	}
	pr := &github.PullRequest{Number: 100 + f.Creations, URL: "https://github.com/" + repo + "/pull/101", Draft: true, State: github.Open}
	pr.Base.Ref = base
	pr.Head.Ref = branch
	if f.Head != nil {
		pr.Head.SHA = f.Head(branch)
	}
	pr.Head.Repo.FullName = repo
	body, err := github.GeneratePullRequestBody(content)
	if err != nil {
		return nil, err
	}
	pr.Body = body
	f.Prs[branch] = pr
	return pr, nil
}
func Ready(n int) github.Issue {
	return github.Issue{Number: n, Title: "Implement this", State: github.Open, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Labels: []github.Label{{Name: "ready-for-agent"}}}
}
func Fixture(t *testing.T, api *FakeGitHub) (*scheduler.Scheduler, *app.Runtime) {
	t.Helper()
	runtime, err := app.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime
}

func GitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), TestGitEnvironment(t, dir)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

const SuccessfulScript = `printf '%s\n' 'implemented' > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Added feature"}}'`

func Finish(t *testing.T, s *scheduler.Scheduler, state workflow.State, phase workflow.Phase) workflow.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].State == state && runs[0].Phase == phase {
			return runs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("flow did not reach %s/%s: %+v", state, phase, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func Has(issue github.Issue, label string) bool {
	for _, l := range issue.Labels {
		if l.Name == label {
			return true
		}
	}
	return false
}

type RejectingGit struct {
	PrepareHook func(managedgit.PrepareRequest)
}

func (RejectingGit) Inspect(context.Context, managedgit.Run) error   { return nil }
func (RejectingGit) ListWorktrees(context.Context) ([]string, error) { return nil, nil }
func (RejectingGit) ConfirmOwnership(context.Context, managedgit.Run) error {
	return nil
}

func (RejectingGit) DiscardPreparation(context.Context, managedgit.PrepareRequest) (managedgit.Leftovers, error) {
	return managedgit.Leftovers{}, nil
}

func (g RejectingGit) Prepare(_ context.Context, req managedgit.PrepareRequest) (managedgit.Run, error) {
	if g.PrepareHook != nil {
		g.PrepareHook(req)
	}
	return managedgit.Run{}, &fault.Error{Code: "git.branch_conflict", Message: "Branch already belongs to someone else"}
}
func (RejectingGit) CommitAndPush(context.Context, managedgit.Run, managedgit.Phase) (managedgit.CommitResult, error) {
	panic("unexpected push")
}
func Configured(t *testing.T, api scheduler.GitHub, yaml string, g scheduler.Git) (*scheduler.Scheduler, *app.Runtime) {
	return ConfiguredAt(t, api, yaml, g, nil)
}

// configuredAt additionally controls the scheduler clock, so tests can age
// discovery snapshots without waiting.
func ConfiguredAt(t *testing.T, api scheduler.GitHub, yaml string, g scheduler.Git, now func() time.Time) (*scheduler.Scheduler, *app.Runtime) {
	t.Helper()
	runtime, err := app.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Git: g, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime
}

type GatedGitHub struct {
	*FakeGitHub
	Entered chan struct{}
	Release chan struct{}
}

func (g GatedGitHub) UnresolvedBlockers(ctx context.Context, repo string, n int) ([]github.Issue, error) {
	g.Entered <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.Release:
	}
	return g.FakeGitHub.UnresolvedBlockers(ctx, repo, n)
}

type ObservingGitHub struct {
	*FakeGitHub
	Polls chan struct{}
}

func (o ObservingGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	select {
	case o.Polls <- struct{}{}:
	default:
	}
	return o.FakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

func SchedulerResources(runtime *app.Runtime) scheduler.Resources {
	return scheduler.Resources{DB: runtime.DB, Events: runtime.Events, Workflow: runtime.Workflow, Workspace: runtime.Workspace, Control: runtime.Scheduler}
}

// blockedReport retains the current contract when a success fixture becomes blocked.
func BlockedReport(report string) string {
	report = strings.Replace(report, `"status":"success"`, `"status":"blocked"`, 1)
	return strings.Replace(report, `"human_action":null`, `"human_action":{"decision":"Clarify the required behavior","files":[],"options":[]}`, 1)
}

func (f *FakeGitHub) EnsureHarnessOverrideComment(_ context.Context, _ string, n int, reason string) error {
	if f.OverrideComments == nil {
		f.OverrideComments = make(map[int]string)
	}
	if _, exists := f.OverrideComments[n]; !exists {
		f.OverrideComments[n] = reason
	}
	return nil
}
