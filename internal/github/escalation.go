package github

import (
	"context"
	"encoding/json"
	"html"
	"strings"

	"github.com/rcpassos/mergeyard/internal/escalation"
)

const escalationStart = "<!-- mergeyard:escalation:start -->"
const escalationEnd = "<!-- mergeyard:escalation:end -->"

// UpdateEscalationBody changes only the escalation section, leaving the saved
// summary and all user-authored content byte-for-byte intact.
func UpdateEscalationBody(body string, snapshot escalation.Snapshot) (string, error) {
	section := ""
	if len(snapshot.Reasons) > 0 {
		section = escalationStart + "\n### Needs careful review\n\nApproved commit: <code>" + html.EscapeString(snapshot.HeadSHA) + "</code>\n"
		for _, flag := range snapshot.Reasons {
			section += "\n- <code>" + html.EscapeString(flag.Reason) + "</code>\n"
			for _, file := range flag.Files {
				section += "  - <code>" + html.EscapeString(file) + "</code>\n"
			}
		}
		section += escalationEnd
	}
	start, end := strings.Index(body, escalationStart), strings.Index(body, escalationEnd)
	if start >= 0 || end >= 0 {
		if start < 0 || end < start || strings.Count(body, escalationStart) != 1 || strings.Count(body, escalationEnd) != 1 {
			return "", codedError("pr.invalid_generated_section", "PR body has ambiguous escalation ownership markers", nil)
		}
		return body[:start] + section + body[end+len(escalationEnd):], nil
	}
	if section == "" {
		return body, nil
	}
	start, end = strings.Index(body, generatedStart), strings.Index(body, generatedEnd)
	if start >= 0 || end >= 0 {
		if start < 0 || end < start || strings.Count(body, generatedStart) != 1 || strings.Count(body, generatedEnd) != 1 {
			return "", codedError("pr.invalid_generated_section", "PR body has ambiguous ownership markers", nil)
		}
		return body[:end] + section + "\n" + body[end:], nil
	}
	return body + "\n\n" + section, nil
}

// UpdateEscalation reads the latest body on every attempt. A failed PATCH is
// never replayed from a stale body snapshot that could erase a human's edits.
func (c *Client) UpdateEscalation(ctx context.Context, repo string, number int, snapshot escalation.Snapshot) error {
	pr, err := c.GetPullRequest(ctx, repo, number)
	if err != nil {
		return err
	}
	body, err := UpdateEscalationBody(pr.Body, snapshot)
	if err != nil || body == pr.Body {
		return err
	}
	if len(body) > MaxBodyBytes {
		return codedError("github.invalid_input", "PR body exceeds GitHub's size limit", nil)
	}
	input, err := json.Marshal(struct {
		Body string `json:"body"`
	}{body})
	if err != nil {
		return err
	}
	endpoint, err := pullRequestPath(repo, number)
	if err != nil {
		return err
	}
	_, err = c.requestWithAttempts(ctx, input, "PATCH", endpoint, false, 1)
	return err
}
