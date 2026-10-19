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

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/repository"
	"github.com/rcpassos/mergeyard/internal/verification"
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
	if len(body) > MaxBodyBytes {
		return nil, codedError("github.invalid_input", "PR body exceeds GitHub's size limit", nil)
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
	Verification []verification.Entry
	IssueNumber  int
	IssueTitle   string
	Summary      string
	RunID        string
	RoleSettings *config.RunRoleSettings
}

// GeneratePullRequestBody encloses the generated text in stable ownership markers.
// Verification evidence is truncated first when the result would exceed
// GitHub's size limit, then the summary, while the ownership markers stay whole.
func GeneratePullRequestBody(content PullRequestContent) (string, error) {
	if content.IssueNumber <= 0 || strings.TrimSpace(content.IssueTitle) == "" || strings.TrimSpace(content.Summary) == "" || strings.TrimSpace(content.RunID) == "" || strings.ContainsAny(content.RunID, "\r\n`") {
		return "", codedError("github.invalid_input", "expected an issue number, title, summary, and single-line run ID", nil)
	}
	for _, text := range []string{content.Summary, content.RunID} {
		for _, marker := range []string{generatedStart, generatedEnd, escalationStart, escalationEnd} {
			if strings.Contains(text, marker) {
				return "", codedError("github.invalid_input", "generated content contains reserved ownership markers", nil)
			}
		}
	}
	entries := content.Verification
	if entries != nil {
		if err := verification.Validate(entries); err != nil {
			return "", codedError("github.invalid_input", "invalid verification evidence", err)
		}
		entries = verification.Capped(entries)
	}
	summary := content.Summary
	body, err := renderPullRequestBody(content, summary, entries)
	if err != nil {
		return "", err
	}
	for len(body) > MaxBodyBytes {
		if len(entries) > 0 {
			trimmed := false
			for i := range entries {
				if entries[i].Excerpt == nil {
					continue
				}
				runes := []rune(*entries[i].Excerpt)
				if len(runes) <= 64 {
					continue
				}
				excerpt := string(runes[:len(runes)/2])
				entries[i].Excerpt = &excerpt
				trimmed = true
			}
			if trimmed {
				if body, err = renderPullRequestBody(content, summary, entries); err != nil {
					return "", err
				}
				continue
			}
			entries = entries[:len(entries)-1]
			if body, err = renderPullRequestBody(content, summary, entries); err != nil {
				return "", err
			}
			continue
		}
		const summarySuffix = "\n\n[truncated]"
		overflow := len(body) - MaxBodyBytes
		target := len(summary) - overflow - len(summarySuffix)
		if target <= 1 {
			summary = truncateUTF8(summary, 1) + summarySuffix
			if body, err = renderPullRequestBody(content, summary, entries); err != nil {
				return "", err
			}
			if len(body) > MaxBodyBytes {
				return "", codedError("github.invalid_input", "generated PR body exceeds GitHub's size limit", nil)
			}
			return body, nil
		}
		summary = truncateUTF8(summary, target) + summarySuffix
		if body, err = renderPullRequestBody(content, summary, entries); err != nil {
			return "", err
		}
	}
	return body, nil
}

func renderPullRequestBody(content PullRequestContent, summary string, entries []verification.Entry) (string, error) {
	evidence := ""
	if entries != nil {
		evidence = "\n\n<details>\n<summary>Verification evidence</summary>\n\n"
		if len(entries) == 0 {
			evidence += "no verification reported\n"
		} else {
			encoded, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return "", err
			}
			literal := strings.NewReplacer("`", `\u0060`, "@", `\u0040`).Replace(string(encoded))
			evidence += "```json\n" + literal + "\n```\n"
		}
		evidence += "\n</details>"
	}
	return fmt.Sprintf("%s\nCloses #%d\n\n%s%s\n\n---\nGenerated by Mergeyard. Run ID: `%s`.\n%s%s", generatedStart, content.IssueNumber, summary, evidence, content.RunID, roleSettingsFooter(content.RoleSettings), generatedEnd), nil
}

func roleSettingsFooter(settings *config.RunRoleSettings) string {
	if settings == nil {
		return ""
	}
	var footer strings.Builder
	footer.WriteString("\nRole settings at claim:\n\n")
	for _, role := range []struct {
		name     string
		settings config.RoleSettings
	}{{"Implementer", settings.Implementer}, {"Reviewer", settings.Reviewer}} {
		saved := role.settings
		fmt.Fprintf(&footer, "- %s: harness %s (source %s); model %s; effort %s; tier %s; labels %s.\n", role.name,
			roleSettingLiteral(saved.Agent, "none"), roleSettingLiteral(saved.HarnessSource, "config"),
			roleSettingLiteral(saved.Model, "harness default"), roleSettingLiteral(saved.Effort, "harness default"),
			roleSettingLiteral(saved.Tier, "none"), roleSettingLiteral(strings.Join(saved.TierLabels, ", "), "none"))
	}
	return footer.String()
}

// JSON escaping keeps settings on one line and neutralizes HTML/ownership
// markers. Escape Markdown delimiters and mentions before enclosing in code.
func roleSettingLiteral(value, fallback string) string {
	if value == "" {
		value = fallback
	}
	encoded, _ := json.Marshal(value)
	literal := strings.NewReplacer("`", `\u0060`, "@", `\u0040`).Replace(string(encoded[1 : len(encoded)-1]))
	return "`" + literal + "`"
}

// UpdatePullRequestBody replaces only the owned section, preserving all other
// bytes and the separately published escalation subsection. Without an owned
// section, it appends one after the existing user text.
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
	flagStart, flagEnd := strings.Index(body, escalationStart), strings.Index(body, escalationEnd)
	if flagStart >= 0 || flagEnd >= 0 {
		if flagStart < 0 || flagEnd < flagStart || strings.Count(body, escalationStart) != 1 || strings.Count(body, escalationEnd) != 1 {
			return "", codedError("pr.invalid_generated_section", "PR body has ambiguous escalation ownership markers", nil)
		}
		flagEnd += len(escalationEnd)
		if flagStart >= start+len(generatedStart) && flagEnd <= end {
			// Verification can be delivered after escalation's saved publication
			// receipt. Keep its independently owned bytes inside the new block.
			generated = strings.TrimSuffix(generated, generatedEnd) + body[flagStart:flagEnd] + "\n" + generatedEnd
		} else if flagStart < end+len(generatedEnd) && flagEnd > start {
			return "", codedError("pr.invalid_generated_section", "PR body has overlapping ownership markers", nil)
		}
	}
	return body[:start] + generated + body[end+len(generatedEnd):], nil
}
