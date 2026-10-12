package github

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Comment struct {
	ID        int64     `json:"id"`
	URL       string    `json:"html_url"`
	Body      string    `json:"body"`
	User      *Actor    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

var reportMarker = regexp.MustCompile(`^<!-- mergeyard:report:[a-zA-Z0-9]+ -->$`)
var mergeyardMarker = regexp.MustCompile(`<!--\s*mergeyard:[\s\S]*?-->`)

// IssueComments contains trusted human comments in chronological order. Marked
// Mergeyard comments are excluded independently of author permissions.
type IssueComments struct {
	Comments         []Comment
	OmittedUntrusted int
}

// ListIssueComments fetches current text from every page and uses the same actor
// policy as ready-label authorization. Permission decisions are cached only for
// this fetch, so a later attempt observes changes to repository access.
func (c *Client) ListIssueComments(ctx context.Context, repo string, number int, trusted []string) (IssueComments, error) {
	var result IssueComments
	if number <= 0 {
		return result, codedError("github.invalid_input", "expected a positive issue number", nil)
	}
	path, err := issuePath(repo, number)
	if err != nil {
		return result, err
	}
	data, err := c.request(ctx, nil, "GET", path+"/comments?per_page=100", true)
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	actors := map[string]bool{}
	pages := 0
	for {
		var page []Comment
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return IssueComments{}, codedError("github.invalid_response", "expected a comment array", err)
		}
		pages++
		for _, comment := range page {
			if mergeyardMarker.MatchString(comment.Body) {
				continue
			}
			login := ""
			if comment.User != nil {
				login = comment.User.Login
			}
			key := strings.ToLower(login)
			allowed, ok := actors[key]
			if !ok {
				allowed, err = c.IsTrustedActor(ctx, repo, login, trusted)
				if err != nil {
					return IssueComments{}, err
				}
				actors[key] = allowed
			}
			if !allowed {
				result.OmittedUntrusted++
				continue
			}
			if comment.ID <= 0 || comment.CreatedAt.IsZero() {
				return IssueComments{}, codedError("github.invalid_response", "missing comment identity or creation time", nil)
			}
			result.Comments = append(result.Comments, comment)
		}
	}
	if pages == 0 {
		return IssueComments{}, codedError("github.invalid_response", "missing comment array", nil)
	}
	sort.Slice(result.Comments, func(i, j int) bool {
		a, b := result.Comments[i], result.Comments[j]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	return result, nil
}

// EnsureReportComment inspects every page before a single create attempt. A
// transport or decoding failure may have applied remotely; callers must replay
// through this method. Matching comments are never edited, preserving human text.
func (c *Client) EnsureReportComment(ctx context.Context, repo string, number int, body string) (*Comment, error) {
	marker, _, ok := strings.Cut(body, "\n")
	if number <= 0 || !ok || !reportMarker.MatchString(marker) {
		return nil, codedError("github.invalid_input", "expected a report marker and positive PR number", nil)
	}
	return c.ensureComment(ctx, repo, number, body, nil)
}

func (c *Client) ensureComment(ctx context.Context, repo string, number int, body string, match func(Comment) bool) (*Comment, error) {
	marker, _, _ := strings.Cut(body, "\n")
	if number <= 0 {
		return nil, codedError("github.invalid_input", "expected a positive issue number", nil)
	}
	path, err := issuePath(repo, number)
	if err != nil {
		return nil, err
	}
	path += "/comments"
	data, err := c.request(ctx, nil, "GET", path+"?per_page=100", true)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var found *Comment
	pages := 0
	for {
		var page []Comment
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return nil, codedError("github.invalid_response", "expected a comment array", err)
		}
		pages++
		for _, comment := range page {
			if comment.ID <= 0 || comment.URL == "" {
				return nil, codedError("github.invalid_response", "missing comment identity", nil)
			}
			if (comment.Body == marker || strings.HasPrefix(comment.Body, marker+"\n")) && (match == nil || match(comment)) {
				if found != nil {
					return nil, codedError("publication.multiple_matches", "more than one comment has the report identity", nil)
				}
				copy := comment
				found = &copy
			}
		}
	}
	if pages == 0 {
		return nil, codedError("github.invalid_response", "missing comment array", nil)
	}
	if found != nil {
		return found, nil
	}
	input, err := json.Marshal(struct {
		Body string `json:"body"`
	}{body})
	if err != nil {
		return nil, err
	}
	data, err = c.requestWithAttempts(ctx, input, "POST", path, false, 1)
	if err != nil {
		return nil, err
	}
	var comment Comment
	if err := json.Unmarshal(data, &comment); err != nil {
		return nil, codedError("github.invalid_response", "expected a comment", err)
	}
	if comment.ID <= 0 || comment.URL == "" || comment.Body != body || (match != nil && !match(comment)) {
		return nil, codedError("github.invalid_response", "created comment identity or body is invalid", nil)
	}
	return &comment, nil
}
