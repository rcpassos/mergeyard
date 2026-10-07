package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/rcpassos/mergeyard/internal/repository"
)

// PullRequest contains the identity, current body, and actual draft state from GitHub.
type PullRequest struct {
	Number         int    `json:"number"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	URL   string `json:"html_url"`
	Body  string `json:"body"`
	State State  `json:"state"`
	Draft bool   `json:"draft"`
	Head  struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// GetPullRequest reads a persisted PR even after it has closed or merged.
func (c *Client) GetPullRequest(ctx context.Context, repo string, number int) (*PullRequest, error) {
	if number <= 0 {
		return nil, codedError("github.invalid_input", "PR number must be positive", nil)
	}
	path, err := pullRequestPath(repo, number)
	if err != nil {
		return nil, err
	}
	data, err := c.request(ctx, nil, "GET", path, false)
	if err != nil {
		return nil, err
	}
	return decodePullRequest(data)
}

func pullRequestPath(repo string, number int) (string, error) {
	if !repository.ValidName(repo) || number < 0 {
		return "", codedError("github.invalid_input", "expected owner/repo and a positive PR number", nil)
	}
	path := "repos/" + repo + "/pulls"
	if number > 0 {
		path += fmt.Sprintf("/%d", number)
	}
	return path, nil
}

// FindOpenPullRequest returns nil when no PR exists for this repository's branch.
// Every page is inspected so multiple open PRs cannot be silently selected.
func (c *Client) FindOpenPullRequest(ctx context.Context, repo, branch string) (*PullRequest, error) {
	return c.findBranchPullRequest(ctx, repo, branch, true)
}

// FindPullRequest inspects every state before manual work can be published.
// Multiple matching PRs, including closed history, require human reconciliation.
func (c *Client) FindPullRequest(ctx context.Context, repo, branch string) (*PullRequest, error) {
	return c.findBranchPullRequest(ctx, repo, branch, false)
}

func (c *Client) findBranchPullRequest(ctx context.Context, repo, branch string, openOnly bool) (*PullRequest, error) {
	path, err := pullRequestPath(repo, 0)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(branch) == "" {
		return nil, codedError("github.invalid_input", "head branch must be nonempty", nil)
	}
	state := "all"
	if openOnly {
		state = "open"
	}
	query := url.Values{"state": {state}, "head": {strings.SplitN(repo, "/", 2)[0] + ":" + branch}, "per_page": {"100"}}
	data, err := c.request(ctx, nil, "GET", path+"?"+query.Encode(), true)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var found *PullRequest
	pages := 0
	for {
		var page []PullRequest
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return nil, codedError("github.invalid_response", "expected a PR array", err)
		}
		pages++
		for _, pr := range page {
			if err := validatePullRequest(pr); err != nil {
				return nil, err
			}
			if (openOnly && pr.State != Open) || pr.Head.Ref != branch || !strings.EqualFold(pr.Head.Repo.FullName, repo) {
				continue
			}
			if found != nil {
				if openOnly {
					return nil, codedError("pr.multiple_open", "more than one open PR exists for the branch", nil)
				}
				return nil, codedError("pr.multiple_matches", "more than one PR exists for the branch across states", nil)
			}
			found = &pr
		}
	}
	if pages == 0 {
		return nil, codedError("github.invalid_response", "missing PR array", nil)
	}
	return found, nil
}

func validatePullRequest(pr PullRequest) error {
	if pr.Number <= 0 || pr.URL == "" || (pr.State != Open && pr.State != Closed) || pr.Head.Ref == "" || !repository.ValidName(pr.Head.Repo.FullName) {
		return codedError("github.invalid_response", "PR identity, state, or head is invalid", nil)
	}
	return nil
}

// CreateDraftPullRequest creates a PR for an already pushed branch. Draft reports
// GitHub's actual state; callers can persist it alongside the PR number and URL.
func (c *Client) CreateDraftPullRequest(ctx context.Context, repo, branch, base string, content PullRequestContent) (*PullRequest, error) {
	path, err := pullRequestPath(repo, 0)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(base) == "" {
		return nil, codedError("github.invalid_input", "head and base branches must be nonempty", nil)
	}
	body, err := GeneratePullRequestBody(content)
	if err != nil {
		return nil, err
	}
	payload := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}{fmt.Sprintf("#%d %s", content.IssueNumber, content.IssueTitle), branch, base, body, true}
	input, err := json.Marshal(payload)
	if err != nil {
		return nil, codedError("github.invalid_input", "cannot encode PR", err)
	}
	data, err := c.requestWithAttempts(ctx, input, "POST", path, false, 1)
	var failure *Error
	if errors.As(err, &failure) && strings.Contains(strings.ToLower(failure.Error()), "draft pull requests are not supported in this repository") && strings.Contains(failure.Error(), "(HTTP 422)") {
		payload.Draft = false
		input, err = json.Marshal(payload)
		if err != nil {
			return nil, codedError("github.invalid_input", "cannot encode PR", err)
		}
		data, err = c.requestWithAttempts(ctx, input, "POST", path, false, 1)
	}
	if err != nil {
		return nil, err
	}
	return decodePullRequest(data)
}

func decodePullRequest(data []byte) (*PullRequest, error) {
	var pr PullRequest
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, codedError("github.invalid_response", "expected a PR", err)
	}
	if err := validatePullRequest(pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// UpdatePullRequest reads the latest body and updates only Mergeyard's section.
// User-authored text and the PR title, base, and draft state are left intact.
// A failed write may have applied remotely; a later attempt must read again.
func (c *Client) UpdatePullRequest(ctx context.Context, repo string, number int, content PullRequestContent) (*PullRequest, error) {
	if number <= 0 {
		return nil, codedError("github.invalid_input", "PR number must be positive", nil)
	}
	path, err := pullRequestPath(repo, number)
	if err != nil {
		return nil, err
	}
	if _, err := GeneratePullRequestBody(content); err != nil {
		return nil, err
	}
	data, err := c.request(ctx, nil, "GET", path, false)
	if err != nil {
		return nil, err
	}
	pr, err := decodePullRequest(data)
	if err != nil {
		return nil, err
	}
	body, err := UpdatePullRequestBody(pr.Body, content)
	if err != nil {
		return nil, err
	}
	if body == pr.Body {
		return pr, nil
	}
	input, err := json.Marshal(struct {
		Body string `json:"body"`
	}{body})
	if err != nil {
		return nil, codedError("github.invalid_input", "cannot encode PR body", err)
	}
	// Never replay this body snapshot: after an ambiguous failure a human may
	// have edited the body, and retrying the same PATCH would erase their changes.
	data, err = c.requestWithAttempts(ctx, input, "PATCH", path, false, 1)
	if err != nil {
		return nil, err
	}
	return decodePullRequest(data)
}

const (
	generatedStart = "<!-- mergeyard:generated:start -->"
	generatedEnd   = "<!-- mergeyard:generated:end -->"
)

// PullRequestContent is the portion of a PR owned by Mergeyard.
type PullRequestContent struct {
	IssueNumber int
	IssueTitle  string
	Summary     string
	RunID       string
}

// GeneratePullRequestBody encloses the generated text in stable ownership markers.
func GeneratePullRequestBody(content PullRequestContent) (string, error) {
	if content.IssueNumber <= 0 || strings.TrimSpace(content.IssueTitle) == "" || strings.TrimSpace(content.Summary) == "" || strings.TrimSpace(content.RunID) == "" || strings.ContainsAny(content.RunID, "\r\n`") {
		return "", codedError("github.invalid_input", "expected an issue number, title, summary, and single-line run ID", nil)
	}
	for _, text := range []string{content.Summary, content.RunID} {
		if strings.Contains(text, generatedStart) || strings.Contains(text, generatedEnd) {
			return "", codedError("github.invalid_input", "generated content contains reserved ownership markers", nil)
		}
	}
	return fmt.Sprintf("%s\nCloses #%d\n\n%s\n\n---\nGenerated by Mergeyard. Run ID: `%s`.\n%s", generatedStart, content.IssueNumber, content.Summary, content.RunID, generatedEnd), nil
}

// UpdatePullRequestBody replaces only the owned section, preserving all other
// bytes. Without an owned section, it appends one after the existing user text.
func UpdatePullRequestBody(body string, content PullRequestContent) (string, error) {
	generated, err := GeneratePullRequestBody(content)
	if err != nil {
		return "", err
	}
	start, end := strings.Index(body, generatedStart), strings.Index(body, generatedEnd)
	if start < 0 && end < 0 {
		if body == "" {
			return generated, nil
		}
		return body + "\n\n" + generated, nil
	}
	if start < 0 || end < start || strings.Count(body, generatedStart) != 1 || strings.Count(body, generatedEnd) != 1 {
		return "", codedError("pr.invalid_generated_section", "PR body has incomplete or ambiguous ownership markers", nil)
	}
	return body[:start] + generated + body[end+len(generatedEnd):], nil
}
