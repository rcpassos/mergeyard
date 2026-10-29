package scheduler_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
)

// A gated candidate is never claimed, so its per-issue reads are waste that a
// multi-day limit repeats on every tick.
func TestLimitedImplementerHarnessSpendsNoPerIssueCalls(t *testing.T) {
	t.Parallel()

	// Each candidate costs a trust verification, blockers and the fresh pre-claim
	// issue, plus the uncached trust recheck that guards a real claim.
	perCandidate := testkit.GithubCalls{TrustVerifications: 1, BlockerReads: 1, IssueReads: 1, TrustRechecks: 1}
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "limited"}[limited], func(t *testing.T) {
			ctx := context.Background()
			api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7), testkit.Ready(8)}}}
			s, runtime := testkit.Configured(t, api, "concurrency: 2\nrepositories: [{repo: owner/repo}]", testkit.RejectingGit{})
			// Two candidates after one listing.
			wantDecision, wantCalls := "would_claim", testkit.GithubCalls{
				IssueListings:      1,
				TrustVerifications: 2 * perCandidate.TrustVerifications,
				BlockerReads:       2 * perCandidate.BlockerReads,
				IssueReads:         2 * perCandidate.IssueReads,
				TrustRechecks:      2 * perCandidate.TrustRechecks,
			}
			wantReason := ""
			if limited {
				testkit.LimitedHarness(t, runtime, "claude", time.Now().Add(time.Hour))
				wantDecision, wantReason, wantCalls = "waiting", "harness_limited", testkit.GithubCalls{IssueListings: 1}
			}

			plan, err := s.Plan(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Issues) != 2 {
				t.Fatalf("plan=%+v", plan)
			}
			for _, issue := range plan.Issues {
				if issue.Decision != wantDecision || issue.Reason != wantReason {
					t.Fatalf("plan issue %d = %s:%s, want %s:%s", issue.Number, issue.Decision, issue.Reason, wantDecision, wantReason)
				}
			}
			if !reflect.DeepEqual(api.Calls, wantCalls) {
				t.Fatalf("plan calls = %+v, want %+v", api.Calls, wantCalls)
			}

			api.Calls = testkit.GithubCalls{}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if claimed := len(runs) != 0; claimed == limited {
				t.Fatalf("limited=%t claimed %d runs: %+v", limited, len(runs), runs)
			}
			// One poll lists the repository once for reconcile and dispatch; a
			// gated candidate spends no per-issue calls beyond that listing.
			tickCalls := testkit.GithubCalls{IssueListings: 1}
			if !limited {
				tickCalls = testkit.GithubCalls{
					IssueListings:      1,
					TrustVerifications: 2 * perCandidate.TrustVerifications,
					BlockerReads:       2 * perCandidate.BlockerReads,
					IssueReads:         2 * perCandidate.IssueReads,
					TrustRechecks:      2 * perCandidate.TrustRechecks,
				}
			}
			if !reflect.DeepEqual(api.Calls, tickCalls) {
				t.Fatalf("tick calls = %+v, want %+v", api.Calls, tickCalls)
			}
			// The dashboard's availability panel still reports the same wait.
			availability, err := s.HarnessAvailability(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, harness := range availability {
				if harness.Harness == "claude" && harness.Available == limited {
					t.Fatalf("claude availability = %+v, limited=%t", harness, limited)
				}
			}
		})
	}
}

// The listed labels can name an available harness while the fresh pre-claim read
// names a limited one, so the gate is rechecked against the snapshot being claimed.
func TestClaimRechecksTheGateOnTheFreshSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	api.GetIssueHook = func(repo string, n int) (github.Issue, error) {
		issue := api.Issues[repo][0]
		issue.Labels = append(issue.Labels, github.Label{Name: "mergeyard:implementer=codex"})
		return issue, nil
	}
	s, runtime := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
	testkit.LimitedHarness(t, runtime, "codex", time.Now().Add(time.Hour))
	plan, err := s.Plan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Decision != "waiting" || plan.Issues[0].Reason != "harness_limited" {
		t.Fatalf("fresh snapshot override bypassed the gate: %+v", plan)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("restricted implementer was claimed: %+v", runs)
	}
	// The recheck cost the same claim reads a candidate without an override costs,
	// so the second decision came from the snapshot rather than from the listing.
	want := testkit.GithubCalls{IssueListings: 2, TrustVerifications: 2, BlockerReads: 2, IssueReads: 2, TrustRechecks: 2}
	if !reflect.DeepEqual(api.Calls, want) {
		t.Fatalf("calls = %+v, want %+v", api.Calls, want)
	}
}

// An override the early gate cannot resolve is still refused, so a limit never
// hides a misconfigured issue from the maintainer who can fix it.
func TestLimitedHarnessStillRefusesAnUnresolvableOverride(t *testing.T) {
	t.Parallel()

	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "limited"}[limited], func(t *testing.T) {
			ctx := context.Background()
			issue := testkit.Ready(7)
			issue.Labels = append(issue.Labels, github.Label{Name: "mergeyard:implementer=invalid"})
			api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {issue}}}
			s, runtime := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
			if limited {
				testkit.LimitedHarness(t, runtime, "claude", time.Now().Add(time.Hour))
			}
			plan, err := s.Plan(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Issues) != 1 || plan.Issues[0].Reason != "invalid_harness_override" {
				t.Fatalf("plan=%+v", plan)
			}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(ctx)
			if err != nil || len(runs) != 0 {
				t.Fatalf("misconfigured override was claimed: %+v %v", runs, err)
			}
			if !strings.Contains(api.OverrideComments[7], "mergeyard:implementer=") {
				t.Fatalf("override refusal explanation: %q", api.OverrideComments[7])
			}
			if !testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("override refusal labels: %+v", api.Issues["owner/repo"][0].Labels)
			}
		})
	}
}

// A refused issue relabelled during a limit is never claimed either, but its
// refusal is still settled from cached trust so the dashboard does not keep
// showing a stale refusal for the whole limit.
func TestLimitedHarnessStillSettlesARelabelledRefusal(t *testing.T) {
	t.Parallel()

	for _, trusted := range []bool{true, false} {
		t.Run(map[bool]string{true: "trusted", false: "untrusted"}[trusted], func(t *testing.T) {
			ctx := context.Background()
			api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
				return github.TrustDecision{Reason: "Untrusted label actor."}, nil
			}}
			s, runtime := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") {
				t.Fatal("untrusted issue was not refused")
			}

			testkit.LimitedHarness(t, runtime, "claude", time.Now().Add(time.Hour))
			if trusted {
				api.Trust = func(github.Issue) (github.TrustDecision, error) { return github.TrustDecision{Allowed: true}, nil }
			}
			if err := api.AddLabel(ctx, "owner/repo", 7, "ready-for-agent"); err != nil {
				t.Fatal(err)
			}
			api.Calls = testkit.GithubCalls{}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			// Only the cached trust verification is spent on the refused issue,
			// and the tick shares one listing between reconcile and dispatch.
			if want := (testkit.GithubCalls{IssueListings: 1, TrustVerifications: 1}); !reflect.DeepEqual(api.Calls, want) {
				t.Fatalf("tick calls = %+v, want %+v", api.Calls, want)
			}
			runs, err := s.Runs(ctx)
			if err != nil || len(runs) != 0 {
				t.Fatalf("limited implementer was claimed: %+v %v", runs, err)
			}
			if got := testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent"); got != trusted {
				t.Fatalf("ready label kept=%t, want %t", got, trusted)
			}
			queue, err := s.Queue(ctx)
			if err != nil || len(queue) != 1 {
				t.Fatalf("queue=%+v err=%v", queue, err)
			}
			if refused := queue[0].RefusalReason != ""; refused == trusted || queue[0].Attention == trusted {
				t.Fatalf("dashboard entry = %+v, trusted=%t", queue[0], trusted)
			}
			if len(api.TrustComments) != 1 {
				t.Fatalf("refusal explanations = %+v, want one", api.TrustComments)
			}
			if !trusted {
				return
			}
			plan, err := s.Plan(ctx, "")
			if err != nil || len(plan.Issues) != 1 || plan.Issues[0].Decision != "waiting" || plan.Issues[0].Reason != "harness_limited" {
				t.Fatalf("plan=%+v err=%v, want waiting:harness_limited", plan, err)
			}
		})
	}
}
