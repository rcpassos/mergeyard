package github

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Bind edit authorization to the REST body we will actually pass to the agent.
// Every edit actor must be trusted; missing or changing history fails closed.
func (c *Client) verifyCommentEdits(ctx context.Context, comment Comment, isTrusted func(string) (bool, error)) (bool, error) {
	invalid := func() (bool, error) {
		return false, codedError("github.invalid_response", "cannot verify issue comment edit history or current text", nil)
	}
	if comment.NodeID == "" {
		return invalid()
	}
	cursor := ""
	seen := map[string]bool{}
	var lastEdited *time.Time
	for {
		query := `query($id:ID!,$cursor:String) { node(id:$id) { ... on IssueComment { id body author { login } lastEditedAt editor { login } userContentEdits(first:100,after:$cursor) { nodes { editedAt deletedAt editor { login } } pageInfo { hasNextPage endCursor } } } } }`
		variables := map[string]any{"id": comment.NodeID, "cursor": nil}
		if cursor != "" {
			variables["cursor"] = cursor
		}
		input, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			return false, err
		}
		data, err := c.request(ctx, input, "POST", "graphql", false)
		if err != nil {
			return false, err
		}
		var result struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Node *struct {
					ID           string     `json:"id"`
					Body         *string    `json:"body"`
					Author       *Actor     `json:"author"`
					Editor       *Actor     `json:"editor"`
					LastEditedAt *time.Time `json:"lastEditedAt"`
					Edits        *struct {
						Nodes []struct {
							EditedAt  time.Time  `json:"editedAt"`
							DeletedAt *time.Time `json:"deletedAt"`
							Editor    *Actor     `json:"editor"`
						} `json:"nodes"`
						PageInfo *struct {
							HasNextPage *bool  `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"userContentEdits"`
				} `json:"node"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &result) != nil || len(result.Errors) > 0 || result.Data.Node == nil {
			return invalid()
		}
		content := result.Data.Node
		if content.ID != comment.NodeID || content.Body == nil || *content.Body != comment.Body || content.Author == nil || !strings.EqualFold(content.Author.Login, comment.User.Login) {
			return invalid()
		}
		if content.Edits == nil || content.Edits.Nodes == nil || content.Edits.PageInfo == nil || content.Edits.PageInfo.HasNextPage == nil {
			return invalid()
		}
		if cursor == "" {
			lastEdited = content.LastEditedAt
		}
		if !sameEditTime(lastEdited, content.LastEditedAt) {
			return invalid()
		}
		if content.LastEditedAt != nil && (content.LastEditedAt.IsZero() || content.Editor == nil) {
			return invalid()
		}
		if content.Editor != nil {
			if content.Editor.Login == "" {
				return invalid()
			}
			allowed, err := isTrusted(content.Editor.Login)
			if err != nil || !allowed {
				return false, err
			}
			if content.LastEditedAt != nil && len(content.Edits.Nodes) == 0 && cursor == "" {
				return invalid()
			}
		}
		for _, edit := range content.Edits.Nodes {
			if edit.EditedAt.IsZero() || edit.DeletedAt != nil || edit.Editor == nil || edit.Editor.Login == "" {
				return invalid()
			}
			allowed, err := isTrusted(edit.Editor.Login)
			if err != nil || !allowed {
				return false, err
			}
		}
		if !*content.Edits.PageInfo.HasNextPage {
			return true, nil
		}
		next := content.Edits.PageInfo.EndCursor
		if next == "" || seen[next] {
			return invalid()
		}
		seen[next] = true
		cursor = next
	}
}
