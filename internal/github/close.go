package github

import "context"

// CloseIssue closes the source issue as completed. The scheduler observes fresh
// state before each attempt and after success; an ambiguous write is not replayed
// here because GitHub may already have applied it.
func (c *Client) CloseIssue(ctx context.Context, repo string, number int) error {
	if number <= 0 {
		return codedError("github.invalid_input", "issue number must be positive", nil)
	}
	path, err := issuePath(repo, number)
	if err != nil {
		return err
	}
	_, err = c.requestWithAttempts(ctx, []byte(`{"state":"closed","state_reason":"completed"}`), "PATCH", path, false, 1)
	return err
}
