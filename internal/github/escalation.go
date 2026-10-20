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
// summary and all user-authored content byte-for-byte intact. The section is
// capped to the reserved headroom so a nearly full body can still publish its
// flags, with a note when details are omitted.
func UpdateEscalationBody(body string, snapshot escalation.Snapshot) (string, error) {
	if len(snapshot.Reasons) == 0 {
		start, end := strings.Index(body, escalationStart), strings.Index(body, escalationEnd)
		if start >= 0 || end >= 0 {
			if start < 0 || end < start || strings.Count(body, escalationStart) != 1 || strings.Count(body, escalationEnd) != 1 {
				return "", codedError("pr.invalid_generated_section", "PR body has ambiguous escalation ownership markers", nil)
			}
			return body[:start] + body[end+len(escalationEnd):], nil
		}
		return body, nil
	}
	start, end := strings.Index(body, escalationStart), strings.Index(body, escalationEnd)
	if start >= 0 || end >= 0 {
		if start < 0 || end < start || strings.Count(body, escalationStart) != 1 || strings.Count(body, escalationEnd) != 1 {
			return "", codedError("pr.invalid_generated_section", "PR body has ambiguous escalation ownership markers", nil)
		}
		available := MaxBodyBytes - (len(body) - (end + len(escalationEnd) - start))
		section, ok := escalationSection(snapshot, available)
		if !ok {
			return "", codedError("github.invalid_input", "PR body has no room for escalation details", nil)
		}
		return body[:start] + section + body[end+len(escalationEnd):], nil
	}
	start, end = strings.Index(body, generatedStart), strings.Index(body, generatedEnd)
	if start >= 0 || end >= 0 {
		if start < 0 || end < start || strings.Count(body, generatedStart) != 1 || strings.Count(body, generatedEnd) != 1 {
			return "", codedError("pr.invalid_generated_section", "PR body has ambiguous ownership markers", nil)
		}
		available := MaxBodyBytes - len(body) - 1
		section, ok := escalationSection(snapshot, available)
		if !ok {
			return "", codedError("github.invalid_input", "PR body has no room for escalation details", nil)
		}
		return body[:end] + section + "\n" + body[end:], nil
	}
	available := MaxBodyBytes - len(body) - 2
	section, ok := escalationSection(snapshot, available)
	if !ok {
		return "", codedError("github.invalid_input", "PR body has no room for escalation details", nil)
	}
	return body + "\n\n" + section, nil
}

func escalationSection(snapshot escalation.Snapshot, maxBytes int) (string, bool) {
	const omittedNote = "\n- Additional escalation details omitted to fit GitHub's size limit.\n"
	head := escalationStart + "\n### Needs careful review\n\nApproved commit: <code>" + html.EscapeString(snapshot.HeadSHA) + "</code>\n"
	if maxBytes < len(head)+len(escalationEnd)+len(omittedNote) {
		return "", false
	}
	budget := maxBytes - len(escalationEnd) - len(omittedNote)
	var section strings.Builder
	section.WriteString(head)
	omitted := false
	for _, flag := range snapshot.Reasons {
		var addition strings.Builder
		addition.WriteString("\n- <code>" + html.EscapeString(flag.Reason) + "</code>\n")
		for _, file := range flag.Files {
			addition.WriteString("  - <code>" + html.EscapeString(file) + "</code>\n")
		}
		if section.Len()+addition.Len() > budget {
			omitted = true
			break
		}
		section.WriteString(addition.String())
	}
	if omitted {
		section.WriteString(omittedNote)
	}
	section.WriteString(escalationEnd)
	return section.String(), true
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
