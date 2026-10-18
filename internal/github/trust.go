package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

// TrustDecision authorizes the observed issue snapshot, not subsequent edits.
type TrustDecision struct {
	Allowed      bool
	Reason       string
	LabelEventID int64
	LastEditedAt *time.Time
}

type trustCacheEntry struct {
	fingerprint [32]byte
	decision    TrustDecision
}

type Actor struct {
	Login string `json:"login"`
}
type labelEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	Actor     *Actor    `json:"actor"`
	Label     Label     `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// IsTrustedActor shares the ready-label policy with other issue inputs. An
// explicit nonempty list replaces repository permissions; logins ignore case.
func (c *Client) IsTrustedActor(ctx context.Context, repo, login string, trusted []string) (bool, error) {
	if login == "" || strings.ContainsAny(login, "/\\\x00\n\r\t ") {
		return false, nil
	}
	if _, err := issuePath(repo, 0); err != nil {
		return false, err
	}
	if len(trusted) > 0 {
		for _, name := range trusted {
			if strings.EqualFold(name, login) {
				return true, nil
			}
		}
		return false, nil
	}
	data, err := c.request(ctx, nil, "GET", "repos/"+repo+"/collaborators/"+url.PathEscape(login)+"/permission", false)
	if err != nil {
		return false, err
	}
	var permission struct {
		Permission string `json:"permission"`
	}
	if json.Unmarshal(data, &permission) != nil || permission.Permission == "" {
		return false, codedError("github.invalid_response", "missing collaborator permission", nil)
	}
	switch permission.Permission {
	case "write", "push", "maintain", "admin":
		return true, nil
	case "read", "pull", "triage", "none":
		return false, nil
	default:
		return false, codedError("github.invalid_response", "unknown collaborator permission", nil)
	}
}

// VerifyIssueTrust reads all label events and content edits before claim. The
// list endpoint's updated_at plus the executable snapshot invalidate the cache
// on edits or label changes. Quiet polls require no extra trust API requests.
// A client is scoped to one control-plane lifetime; restart rechecks permissions.
func (c *Client) VerifyIssueTrust(ctx context.Context, repo string, issue Issue, ready string, trusted []string) (TrustDecision, error) {
	return c.issueTrust(ctx, repo, issue, ready, trusted, false)
}

// RecheckIssueTrust bypasses the quiet-poll cache immediately before claim.
// GitHub's updated_at has second precision, so it cannot alone detect a rapid
// remove/reapply of the same label. Permissions may also change independently.
func (c *Client) RecheckIssueTrust(ctx context.Context, repo string, issue Issue, ready string, trusted []string) (TrustDecision, error) {
	return c.issueTrust(ctx, repo, issue, ready, trusted, true)
}

func (c *Client) issueTrust(ctx context.Context, repo string, issue Issue, ready string, trusted []string, fresh bool) (TrustDecision, error) {
	if issue.Number <= 0 || ready == "" {
		return TrustDecision{}, codedError("github.invalid_input", "expected an issue and ready label", nil)
	}
	if _, err := issuePath(repo, issue.Number); err != nil {
		return TrustDecision{}, err
	}
	snapshot, _ := json.Marshal(struct {
		Issue   Issue
		Ready   string
		Trusted []string
	}{issue, ready, trusted})
	fingerprint := sha256.Sum256(snapshot)
	key := strings.ToLower(repo) + fmt.Sprintf("/%d", issue.Number)
	c.trustMu.Lock()
	defer c.trustMu.Unlock()
	if ctx.Err() != nil {
		return TrustDecision{}, contextError(ctx.Err())
	}
	if entry, ok := c.trustCache[key]; !fresh && ok && entry.fingerprint == fingerprint {
		return entry.decision, nil
	}
	decision, err := c.verifyIssueTrust(ctx, repo, issue, ready, trusted)
	if err != nil {
		return TrustDecision{}, err
	}
	if c.trustCache == nil {
		c.trustCache = make(map[string]trustCacheEntry)
	}
	c.trustCache[key] = trustCacheEntry{fingerprint, decision}
	return decision, nil
}

func (c *Client) verifyIssueTrust(ctx context.Context, repo string, issue Issue, ready string, trusted []string) (TrustDecision, error) {
	decision := TrustDecision{}
	refuse := func(reason string) (TrustDecision, error) { decision.Reason = reason; return decision, nil }
	path, _ := issuePath(repo, issue.Number)
	data, err := c.request(ctx, nil, "GET", path+"/events?per_page=100", true)
	if err != nil {
		return decision, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	latest := map[string]labelEvent{}
	var renames []labelEvent
	pages := 0
	for {
		var page []labelEvent
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return refuse("Cannot verify label history: invalid issue events response.")
		}
		pages++
		for _, event := range page {
			if event.Event == "renamed" {
				renames = append(renames, event)
				continue
			}
			if event.Event != "labeled" && event.Event != "unlabeled" {
				continue
			}
			name := strings.ToLower(event.Label.Name)
			if (strings.EqualFold(event.Label.Name, ready) || isRoleSettingsLabel(name)) && (event.ID <= 0 || event.CreatedAt.IsZero()) {
				return refuse("Cannot verify label history: missing label event identity or time.")
			}
			previous := latest[name]
			if event.CreatedAt.After(previous.CreatedAt) || (event.CreatedAt.Equal(previous.CreatedAt) && event.ID > previous.ID) {
				latest[name] = event
			}
		}
	}
	if pages == 0 {
		return refuse("Cannot verify label history: missing issue events.")
	}
	labels := []string{ready}
	for _, label := range issue.Labels {
		name := strings.ToLower(label.Name)
		if isRoleSettingsLabel(name) {
			labels = append(labels, label.Name)
		}
	}
	sort.Strings(labels[1:])
	actors := map[string]bool{}
	isTrusted := func(login string) (bool, error) {
		key := strings.ToLower(login)
		if allowed, ok := actors[key]; ok {
			return allowed, nil
		}
		allowed, err := c.IsTrustedActor(ctx, repo, login, trusted)
		if err == nil {
			actors[key] = allowed
		}
		return allowed, err
	}
	var readyAt time.Time
	for i, label := range labels {
		event, ok := latest[strings.ToLower(label)]
		if !ok || event.Event != "labeled" || event.ID <= 0 || event.CreatedAt.IsZero() || event.Actor == nil || event.Actor.Login == "" {
			return refuse(fmt.Sprintf("Cannot verify who applied label %q.", label))
		}
		if i == 0 {
			decision.LabelEventID, readyAt = event.ID, event.CreatedAt
		}
		allowed, err := isTrusted(event.Actor.Login)
		if err != nil {
			return decision, err
		}
		if !allowed {
			return refuse(fmt.Sprintf("Label %q was applied by untrusted actor @%s.", label, event.Actor.Login))
		}
	}
	for _, event := range renames {
		if event.CreatedAt.IsZero() {
			return refuse("Cannot verify issue title edit history: missing edit time.")
		}
		if event.Actor == nil || event.Actor.Login == "" {
			return refuse("Cannot verify the actor of an issue title edit.")
		}
		if event.CreatedAt.Before(readyAt) {
			continue
		}
		allowed, err := isTrusted(event.Actor.Login)
		if err != nil {
			return decision, err
		}
		if !allowed {
			return refuse(fmt.Sprintf("Issue title was edited after authorization by untrusted actor @%s.", event.Actor.Login))
		}
	}
	owner, name, _ := strings.Cut(repo, "/")
	cursor := ""
	var lastEdited *time.Time
	for {
		query := `query($owner:String!,$name:String!,$number:Int!,$cursor:String) { repository(owner:$owner,name:$name) { issue(number:$number) { title body lastEditedAt editor { login } userContentEdits(first:100,after:$cursor) { nodes { editedAt editor { login } } pageInfo { hasNextPage endCursor } } } } }`
		variables := map[string]any{"owner": owner, "name": name, "number": issue.Number, "cursor": nil}
		if cursor != "" {
			variables["cursor"] = cursor
		}
		input, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
		data, err := c.request(ctx, input, "POST", "graphql", false)
		if err != nil {
			return decision, err
		}
		var result struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Repository *struct {
					Issue *struct {
						Title        *string    `json:"title"`
						Body         *string    `json:"body"`
						LastEditedAt *time.Time `json:"lastEditedAt"`
						Editor       *Actor     `json:"editor"`
						Edits        *struct {
							Nodes []struct {
								EditedAt time.Time `json:"editedAt"`
								Editor   *Actor    `json:"editor"`
							} `json:"nodes"`
							PageInfo *struct {
								HasNextPage *bool  `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"userContentEdits"`
					} `json:"issue"`
				} `json:"repository"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &result) != nil || len(result.Errors) > 0 || result.Data.Repository == nil || result.Data.Repository.Issue == nil {
			return refuse("Cannot verify issue body edit history: GitHub did not return readable content edits.")
		}
		content := result.Data.Repository.Issue
		if content.Title == nil || *content.Title != issue.Title {
			return refuse("Issue title changed during authorization; a trusted maintainer must apply the ready label again.")
		}
		if content.Body == nil || *content.Body != issue.Body {
			return refuse("Issue body changed during authorization; a trusted maintainer must apply the ready label again.")
		}
		if content.Edits == nil || content.Edits.Nodes == nil || content.Edits.PageInfo == nil || content.Edits.PageInfo.HasNextPage == nil {
			return refuse("Cannot verify issue body edit history: GitHub did not return readable content edits.")
		}
		if cursor == "" {
			lastEdited = content.LastEditedAt
			decision.LastEditedAt = lastEdited
		}
		if !sameEditTime(lastEdited, content.LastEditedAt) {
			return refuse("Issue body was edited during authorization.")
		}
		if content.LastEditedAt != nil && !content.LastEditedAt.Before(readyAt) {
			if content.Editor == nil || content.Editor.Login == "" {
				return refuse("Cannot verify the actor of the latest issue body edit.")
			}
			allowed, err := isTrusted(content.Editor.Login)
			if err != nil {
				return decision, err
			}
			if !allowed {
				return refuse(fmt.Sprintf("Issue body was edited after authorization by untrusted actor @%s.", content.Editor.Login))
			}
		}
		for _, edit := range content.Edits.Nodes {
			if edit.EditedAt.IsZero() {
				return refuse("Cannot verify issue body edit history: missing edit time.")
			}
			if edit.EditedAt.Before(readyAt) {
				continue
			}
			if edit.Editor == nil || edit.Editor.Login == "" {
				return refuse("Cannot verify the actor of an issue body edit after authorization.")
			}
			allowed, err := isTrusted(edit.Editor.Login)
			if err != nil {
				return decision, err
			}
			if !allowed {
				return refuse(fmt.Sprintf("Issue body was edited after authorization by untrusted actor @%s.", edit.Editor.Login))
			}
		}
		if !*content.Edits.PageInfo.HasNextPage {
			break
		}
		next := content.Edits.PageInfo.EndCursor
		if next == "" || next == cursor {
			return refuse("Cannot verify issue body edit history: invalid pagination.")
		}
		cursor = next
	}
	decision.Allowed = true
	return decision, nil
}

func sameEditTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func isRoleSettingsLabel(name string) bool {
	return strings.HasPrefix(name, "mergeyard:implementer=") || strings.HasPrefix(name, "mergeyard:reviewer=") ||
		strings.HasPrefix(name, "mergeyard:implementer-tier=") || strings.HasPrefix(name, "mergeyard:reviewer-tier=")
}

const TrustRefusalMarker = "<!-- mergeyard:trust:refused -->"

// EnsureTrustRefusalComment posts once per issue, including after ambiguous
// writes and restarts, using the same paginated read-before-create contract.
func (c *Client) EnsureTrustRefusalComment(ctx context.Context, repo string, number int, reason string) error {
	if number <= 0 {
		return codedError("github.invalid_input", "expected a positive issue number", nil)
	}
	if _, err := issuePath(repo, number); err != nil {
		return err
	}
	login, err := c.authenticatedLogin(ctx)
	if err != nil {
		return err
	}
	prefix := TrustRefusalMarker + "\nMergeyard refused to claim this issue: "
	_, err = c.ensureComment(ctx, repo, number, prefix+reason+"\n\nA trusted maintainer must review the issue title, body, and its harness override and tier labels, then reapply the ready label.", func(comment Comment) bool {
		return comment.User != nil && strings.EqualFold(comment.User.Login, login) && strings.HasPrefix(comment.Body, prefix)
	})
	return err
}

func (c *Client) authenticatedLogin(ctx context.Context) (string, error) {
	c.trustMu.Lock()
	defer c.trustMu.Unlock()
	if c.login != "" {
		return c.login, nil
	}
	data, err := c.request(ctx, nil, "GET", "user", false)
	if err != nil {
		return "", err
	}
	var user Actor
	if json.Unmarshal(data, &user) != nil || user.Login == "" {
		return "", codedError("github.invalid_response", "missing authenticated GitHub login", nil)
	}
	c.login = user.Login
	return c.login, nil
}
