package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
)

type CiContractRunner struct {
	Api        *FakeGitHub
	MergeState string
	ReadyCalls int
}

func (r *CiContractRunner) Run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	pr := r.Api.Prs["mergeyard/issue-7"]
	if len(args) > 1 && args[0] == "pr" && args[1] == "ready" {
		r.ReadyCalls++
		pr.Draft = false
		return nil, nil, nil
	}
	endpoint := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "repos/") {
			endpoint = arg
			break
		}
	}
	var body any
	switch {
	case strings.Contains(endpoint, "/git/commits/"):
		body = map[string]any{"sha": pr.MergeCommitSHA, "parents": []map[string]string{{"sha": pr.Base.SHA}, {"sha": pr.Head.SHA}}}
	case strings.Contains(endpoint, "/check-runs"):
		body = map[string]any{"total_count": 0, "check_runs": []any{}}
	case strings.Contains(endpoint, "/statuses"):
		state := "success"
		if strings.Contains(endpoint, "/commits/test-merge/") {
			state = r.MergeState
		}
		body = []map[string]string{{"context": "ci", "state": state, "target_url": "https://example.com/" + state}}
	case strings.Contains(endpoint, "/rules/branches/"):
		body = []any{}
	case strings.Contains(endpoint, "/branches/"):
		body = map[string]bool{"protected": false}
	default:
		return nil, nil, fmt.Errorf("unexpected CI fixture endpoint %q", endpoint)
	}
	out, err := json.Marshal(body)
	return out, nil, err
}

type ContractCIGitHub struct {
	*FakeGitHub
	Client *github.Client
}

func (c ContractCIGitHub) PullRequestEvidence(ctx context.Context, repo string, pr github.PullRequest) (ci.Evidence, error) {
	return c.Client.PullRequestEvidence(ctx, repo, pr)
}
func (c ContractCIGitHub) MarkReady(ctx context.Context, repo string, n int) error {
	return c.Client.MarkReady(ctx, repo, n)
}
