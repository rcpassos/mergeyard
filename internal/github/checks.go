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

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/repository"
)

// CheckEvidence successfully reads all evidence sources or returns unknown.
// Requirements include classic protection and active inherited rulesets.
func (c *Client) CheckEvidence(ctx context.Context, repo, sha, base string) (ci.Evidence, error) {
	e := ci.Evidence{SHA: sha}
	if !repository.ValidName(repo) || sha == "" || base == "" {
		return e, codedError("github.invalid_input", "checks require repository, SHA and base branch", nil)
	}
	prefix := "repos/" + repo
	data, err := c.request(ctx, nil, "GET", prefix+"/commits/"+url.PathEscape(sha)+"/check-runs?filter=latest&per_page=100", true)
	if err != nil {
		return e, err
	}
	type checkRun struct {
		Name       string `json:"name"`
		SHA        string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		URL        string `json:"html_url"`
		App        struct {
			ID int64 `json:"id"`
		} `json:"app"`
	}
	var runs []checkRun
	decoder := json.NewDecoder(bytes.NewReader(data))
	pages, total := 0, 0
	for {
		var page struct {
			Total  *int        `json:"total_count"`
			Checks *[]checkRun `json:"check_runs"`
		}
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page.Total == nil || page.Checks == nil || *page.Total < 0 {
			return e, codedError("github.invalid_response", "missing check-run page", err)
		}
		if pages > 0 && total != *page.Total {
			return e, codedError("github.invalid_response", "check count changed during pagination", nil)
		}
		total = *page.Total
		pages++
		runs = append(runs, (*page.Checks)...)
	}
	if pages == 0 || len(runs) != total {
		return e, codedError("github.invalid_response", "incomplete check-run evidence", nil)
	}
	for _, r := range runs {
		if r.Name == "" || r.SHA == "" || r.Status == "" {
			return e, codedError("github.invalid_response", "invalid check-run identity", nil)
		}
		e.Checks = append(e.Checks, ci.Check{Name: r.Name, SHA: r.SHA, Source: "check", AppID: r.App.ID, Status: r.Status, Conclusion: r.Conclusion, URL: r.URL})
	}
	data, err = c.request(ctx, nil, "GET", prefix+"/commits/"+url.PathEscape(sha)+"/statuses?per_page=100", true)
	if err != nil {
		return e, err
	}
	type status struct {
		Name  string `json:"context"`
		State string `json:"state"`
		URL   string `json:"target_url"`
	}
	statuses, err := decodePages[status](data)
	if err != nil {
		return e, err
	}
	seen := map[string]bool{}
	for _, s := range statuses {
		if s.Name == "" || s.State == "" {
			return e, codedError("github.invalid_response", "invalid commit status", nil)
		}
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		e.Checks = append(e.Checks, ci.Check{Name: s.Name, SHA: sha, Source: "status", Status: s.State, Conclusion: s.State, URL: s.URL})
	}
	branchPath := prefix + "/branches/" + url.PathEscape(base)
	data, err = c.request(ctx, nil, "GET", branchPath, false)
	if err != nil {
		return e, err
	}
	var branch struct {
		Protected *bool `json:"protected"`
	}
	if err = json.Unmarshal(data, &branch); err != nil || branch.Protected == nil {
		return e, codedError("github.invalid_response", "missing branch protection state", err)
	}
	if *branch.Protected {
		data, err = c.request(ctx, nil, "GET", branchPath+"/protection", false)
		if err != nil {
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "github.not_found" {
				return e, err
			}
			// Rulesets can protect a branch without any classic protection.
			// A 404 alone is not proof of absence: require a successful query
			// that explicitly returns no classic rule before reading rulesets.
			absent, queryErr := c.classicProtectionAbsent(ctx, repo, base)
			if queryErr != nil {
				return e, queryErr
			}
			if !absent {
				return e, err
			}
			data = []byte(`{"required_status_checks":null}`)
		}
		var protection struct {
			Checks json.RawMessage `json:"required_status_checks"`
		}
		if err = json.Unmarshal(data, &protection); err != nil || len(protection.Checks) == 0 {
			return e, codedError("github.invalid_response", "missing protection requirements", err)
		}
		if string(protection.Checks) != "null" {
			var checks struct {
				Contexts *[]string `json:"contexts"`
				Checks   []struct {
					Name  string `json:"context"`
					AppID int64  `json:"app_id"`
				} `json:"checks"`
			}
			if err = json.Unmarshal(protection.Checks, &checks); err != nil || checks.Contexts == nil {
				return e, codedError("github.invalid_response", "invalid protection checks", err)
			}
			names := map[string]bool{}
			for _, r := range checks.Checks {
				if r.Name == "" {
					return e, codedError("github.invalid_response", "missing required check name", nil)
				}
				names[r.Name] = true
				e.Required = append(e.Required, ci.Requirement{Name: r.Name, AppID: r.AppID})
			}
			for _, name := range *checks.Contexts {
				if name == "" {
					return e, codedError("github.invalid_response", "empty required context", nil)
				}
				if !names[name] {
					e.Required = append(e.Required, ci.Requirement{Name: name})
				}
			}
		}
	}
	data, err = c.request(ctx, nil, "GET", prefix+"/rules/branches/"+url.PathEscape(base)+"?per_page=100", true)
	if err != nil {
		return e, err
	}
	type rule struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks *[]struct {
				Name  string `json:"context"`
				AppID int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	rules, err := decodePages[rule](data)
	if err != nil {
		return e, err
	}
	for _, r := range rules {
		if r.Type == "" {
			return e, codedError("github.invalid_response", "missing rule type", nil)
		}
		if r.Type == "workflows" {
			return e, codedError("github.requirements_unknown", "required workflow rules need human inspection", nil)
		}
		if r.Type != "required_status_checks" {
			continue
		}
		if r.Parameters.Checks == nil {
			return e, codedError("github.invalid_response", "missing ruleset checks", nil)
		}
		for _, check := range *r.Parameters.Checks {
			if check.Name == "" {
				return e, codedError("github.invalid_response", "missing ruleset context", nil)
			}
			e.Required = append(e.Required, ci.Requirement{Name: check.Name, AppID: check.AppID})
		}
	}
	// GitHub's native required-status policy accepts success, skipped and neutral.
	// https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks
	e.AllowSkippedNeutral = true
	e.Gate()
	return e, nil
}

func decodePages[T any](data []byte) ([]T, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var result []T
	pages := 0
	for {
		var page []T
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return nil, codedError("github.invalid_response", "expected paginated array", err)
		}
		pages++
		result = append(result, page...)
	}
	if pages == 0 {
		return nil, codedError("github.invalid_response", "missing array", nil)
	}
	return result, nil
}

// MarkReady makes exactly one external write. Its caller journals intent first
// and reconciles the actual draft state before any subsequent action.
func (c *Client) MarkReady(ctx context.Context, repo string, number int) error {
	if !repository.ValidName(repo) || number <= 0 {
		return codedError("github.invalid_input", "invalid PR identity", nil)
	}
	_, stderr, err := c.runner.Run(ctx, nil, "pr", "ready", fmt.Sprint(number), "--repo", repo)
	if err != nil {
		failure, _ := commandError(stderr, err)
		return failure
	}
	return nil
}

// classicProtectionAbsent establishes explicit absence without treating a failed
// REST query or GraphQL partial/error response as an empty requirement set.
func (c *Client) classicProtectionAbsent(ctx context.Context, repo, base string) (bool, error) {
	parts := strings.SplitN(repo, "/", 2)
	payload, _ := json.Marshal(map[string]any{"query": `query($owner:String!,$repo:String!,$ref:String!){repository(owner:$owner,name:$repo){ref(qualifiedName:$ref){branchProtectionRule{id}}}}`, "variables": map[string]string{"owner": parts[0], "repo": parts[1], "ref": "refs/heads/" + base}})
	data, err := c.request(ctx, payload, "POST", "graphql", false)
	if err != nil {
		return false, err
	}
	var result struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Repository *struct {
				Ref *struct {
					Rule json.RawMessage `json:"branchProtectionRule"`
				} `json:"ref"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Errors) > 0 || result.Data.Repository == nil || result.Data.Repository.Ref == nil || len(result.Data.Repository.Ref.Rule) == 0 {
		return false, codedError("github.requirements_unknown", "classic protection absence could not be established", err)
	}
	return string(result.Data.Repository.Ref.Rule) == "null", nil
}
