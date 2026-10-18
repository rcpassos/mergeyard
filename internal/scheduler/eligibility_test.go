package scheduler_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
)

// limitedHarness records an account-wide usage limit with a reported reset.
func limitedHarness(t *testing.T, runtime *app.Runtime, agent string, reset time.Time) {
	t.Helper()
	if _, err := runtime.DB.Exec("INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,detected_at,phase_attempt_id,signal_source,notified_until) VALUES(?,?,'reported',?,NULL,'fixture','')", agent, reset.Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// A gated candidate is never claimed, so its per-issue reads are waste that a
// multi-day limit repeats on every tick.
func TestLimitedImplementerHarnessSpendsNoPerIssueCalls(t *testing.T) {
	// Each candidate costs a trust verification, blockers and the fresh pre-claim
	// issue, plus the uncached trust recheck that guards a real claim.
	perCandidate := githubCalls{trustVerifications: 1, blockerReads: 1, issueReads: 1, trustRechecks: 1}
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "limited"}[limited], func(t *testing.T) {
			ctx := context.Background()
			api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7), ready(8)}}}
			s, runtime := configured(t, api, "concurrency: 2\nrepositories: [{repo: owner/repo}]", rejectingGit{})
			// Two candidates after one listing.
			wantDecision, wantCalls := "would_claim", githubCalls{
				issueListings:      1,
				trustVerifications: 2 * perCandidate.trustVerifications,
				blockerReads:       2 * perCandidate.blockerReads,
				issueReads:         2 * perCandidate.issueReads,
				trustRechecks:      2 * perCandidate.trustRechecks,
			}
			wantReason := ""
			if limited {
				limitedHarness(t, runtime, "claude", time.Now().Add(time.Hour))
				wantDecision, wantReason, wantCalls = "waiting", "harness_limited", githubCalls{issueListings: 1}
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
			if !reflect.DeepEqual(api.calls, wantCalls) {
				t.Fatalf("plan calls = %+v, want %+v", api.calls, wantCalls)
			}

			api.calls = githubCalls{}
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
			// A tick still lists the repository once to reconcile and once to dispatch.
			tickCalls := wantCalls
			tickCalls.issueListings++
			if !reflect.DeepEqual(api.calls, tickCalls) {
				t.Fatalf("tick calls = %+v, want %+v", api.calls, tickCalls)
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
	ctx := context.Background()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	api.getIssue = func(repo string, n int) (github.Issue, error) {
		issue := api.issues[repo][0]
		issue.Labels = append(issue.Labels, github.Label{Name: "mergeyard:implementer=codex"})
		return issue, nil
	}
	s, runtime := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
	limitedHarness(t, runtime, "codex", time.Now().Add(time.Hour))
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
	want := githubCalls{issueListings: 3, trustVerifications: 2, blockerReads: 2, issueReads: 2, trustRechecks: 2}
	if !reflect.DeepEqual(api.calls, want) {
		t.Fatalf("calls = %+v, want %+v", api.calls, want)
	}
}

// An override the early gate cannot resolve is still refused, so a limit never
// hides a misconfigured issue from the maintainer who can fix it.
func TestLimitedHarnessStillRefusesAnUnresolvableOverride(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "limited"}[limited], func(t *testing.T) {
			ctx := context.Background()
			issue := ready(7)
			issue.Labels = append(issue.Labels, github.Label{Name: "mergeyard:implementer=invalid"})
			api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {issue}}}
			s, runtime := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
			if limited {
				limitedHarness(t, runtime, "claude", time.Now().Add(time.Hour))
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
			if !strings.Contains(api.overrideComments[7], "mergeyard:implementer=") {
				t.Fatalf("override refusal explanation: %q", api.overrideComments[7])
			}
			if !has(api.issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("override refusal labels: %+v", api.issues["owner/repo"][0].Labels)
			}
		})
	}
}

// A refused issue relabelled during a limit is never claimed either, but its
// refusal is still settled from cached trust so the dashboard does not keep
// showing a stale refusal for the whole limit.
func TestLimitedHarnessStillSettlesARelabelledRefusal(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		t.Run(map[bool]string{true: "trusted", false: "untrusted"}[trusted], func(t *testing.T) {
			ctx := context.Background()
			api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
				return github.TrustDecision{Reason: "Untrusted label actor."}, nil
			}}
			s, runtime := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if has(api.issues["owner/repo"][0], "ready-for-agent") {
				t.Fatal("untrusted issue was not refused")
			}

			limitedHarness(t, runtime, "claude", time.Now().Add(time.Hour))
			if trusted {
				api.trust = func(github.Issue) (github.TrustDecision, error) { return github.TrustDecision{Allowed: true}, nil }
			}
			if err := api.AddLabel(ctx, "owner/repo", 7, "ready-for-agent"); err != nil {
				t.Fatal(err)
			}
			api.calls = githubCalls{}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			// Only the cached trust verification is spent on the refused issue.
			if want := (githubCalls{issueListings: 2, trustVerifications: 1}); !reflect.DeepEqual(api.calls, want) {
				t.Fatalf("tick calls = %+v, want %+v", api.calls, want)
			}
			runs, err := s.Runs(ctx)
			if err != nil || len(runs) != 0 {
				t.Fatalf("limited implementer was claimed: %+v %v", runs, err)
			}
			if got := has(api.issues["owner/repo"][0], "ready-for-agent"); got != trusted {
				t.Fatalf("ready label kept=%t, want %t", got, trusted)
			}
			queue, err := s.Queue(ctx)
			if err != nil || len(queue) != 1 {
				t.Fatalf("queue=%+v err=%v", queue, err)
			}
			if refused := queue[0].RefusalReason != ""; refused == trusted || queue[0].Attention == trusted {
				t.Fatalf("dashboard entry = %+v, trusted=%t", queue[0], trusted)
			}
			if len(api.trustComments) != 1 {
				t.Fatalf("refusal explanations = %+v, want one", api.trustComments)
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
