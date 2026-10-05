package github

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

type Comment struct {
	ID   int64  `json:"id"`
	URL  string `json:"html_url"`
	Body string `json:"body"`
}

var reportMarker = regexp.MustCompile(`^<!-- mergeyard:report:[a-zA-Z0-9]+ -->$`)

// EnsureReportComment inspects every page before a single create attempt. A
// transport or decoding failure may have applied remotely; callers must replay
// through this method. Matching comments are never edited, preserving human text.
func (c *Client) EnsureReportComment(ctx context.Context, repo string, number int, body string) (*Comment, error) {
	marker, _, ok := strings.Cut(body, "\n")
	if number <= 0 || !ok || !reportMarker.MatchString(marker) {
		return nil, codedError("github.invalid_input", "expected a report marker and positive PR number", nil)
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
			if comment.Body == marker || strings.HasPrefix(comment.Body, marker+"\n") {
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
	if comment.ID <= 0 || comment.URL == "" || comment.Body != body {
		return nil, codedError("github.invalid_response", "created comment identity or body is invalid", nil)
	}
	return &comment, nil
}
