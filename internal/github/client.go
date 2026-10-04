package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// State is GitHub's issue state.
type State string

const (
	Open   State = "open"
	Closed State = "closed"
)

type Label struct {
	Name string `json:"name"`
}

// DependencySummary counts unresolved native blockers, not references in the body.
type DependencySummary struct {
	BlockedBy int `json:"blocked_by"`
}

type Issue struct {
	Number       int                `json:"number"`
	Title        string             `json:"title"`
	Body         string             `json:"body"`
	State        State              `json:"state"`
	CreatedAt    time.Time          `json:"created_at"`
	URL          string             `json:"html_url"`
	Labels       []Label            `json:"labels"`
	Dependencies *DependencySummary `json:"issue_dependencies_summary"`
}

// CommandRunner is the process boundary. Implementations must execute gh directly,
// without a shell, and pass input through stdin. A runner may be called concurrently.
type CommandRunner interface {
	Run(context.Context, []byte, ...string) (stdout, stderr []byte, err error)
}

type ghRunner struct{}

func (ghRunner) Run(ctx context.Context, input []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Client wraps the authenticated gh CLI. It holds no issue or repository state.
type Client struct {
	runner CommandRunner
}

// New uses the installed gh executable when runner is nil.
func New(runner CommandRunner) *Client {
	if runner == nil {
		runner = ghRunner{}
	}
	return &Client{runner: runner}
}

// Error provides a stable github.* code and preserves its underlying cause.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Err }

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`)

func issuePath(repo string, number int) (string, error) {
	if !repositoryPattern.MatchString(repo) || strings.HasSuffix(repo, "/.") || strings.HasSuffix(repo, "/..") || number < 0 {
		return "", &Error{Code: "github.invalid_input", Message: "expected owner/repo and a positive issue number"}
	}
	path := "repos/" + repo + "/issues"
	if number > 0 {
		path += fmt.Sprintf("/%d", number)
	}
	return path, nil
}

// ListOpenIssues reads every page; GitHub's issues endpoint also returns PRs,
// which are excluded from the work queue.
func (c *Client) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	path, err := issuePath(repo, 0)
	if err != nil {
		return nil, err
	}
	data, err := c.request(ctx, nil, "GET", path+"?state=open&per_page=100", true)
	if err != nil {
		return nil, err
	}
	return decodeOpenIssues(data)
}

type apiIssue struct {
	Issue
	PullRequest json.RawMessage `json:"pull_request"`
}

// IssueState reads the current issue state from GitHub.
func (c *Client) IssueState(ctx context.Context, repo string, number int) (State, error) {
	if number <= 0 {
		return "", &Error{Code: "github.invalid_input", Message: "issue number must be positive"}
	}
	path, err := issuePath(repo, number)
	if err != nil {
		return "", err
	}
	data, err := c.request(ctx, nil, "GET", path, false)
	if err != nil {
		return "", err
	}
	var issue apiIssue
	if err := json.Unmarshal(data, &issue); err != nil {
		return "", &Error{Code: "github.invalid_response", Message: "expected an issue", Err: err}
	}
	if err := validateIssue(issue.Issue); err != nil {
		return "", err
	}
	if len(issue.PullRequest) != 0 && string(issue.PullRequest) != "null" {
		return "", &Error{Code: "github.invalid_response", Message: "expected an issue, received a pull request"}
	}
	return issue.State, nil
}

// AddLabel adds one literal label name without replacing existing labels.
func (c *Client) AddLabel(ctx context.Context, repo string, number int, label string) error {
	path, err := labelPath(repo, number, label)
	if err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		Labels []string `json:"labels"`
	}{[]string{label}})
	if err != nil {
		return &Error{Code: "github.invalid_input", Message: "cannot encode label", Err: err}
	}
	_, err = c.request(ctx, input, "POST", path, false)
	return err
}

// RemoveLabel removes one literal label name without touching other labels.
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	path, err := labelPath(repo, number, label)
	if err != nil {
		return err
	}
	_, err = c.request(ctx, nil, "DELETE", path+"/"+url.PathEscape(label), false)
	return err
}

func labelPath(repo string, number int, label string) (string, error) {
	if number <= 0 || label == "" {
		return "", &Error{Code: "github.invalid_input", Message: "issue number must be positive and label nonempty"}
	}
	path, err := issuePath(repo, number)
	return path + "/labels", err
}

// UnresolvedBlockers reads native dependencies, including cross-repository issues,
// and returns only those still open. Body text is never interpreted as a dependency.
func (c *Client) UnresolvedBlockers(ctx context.Context, repo string, number int) ([]Issue, error) {
	if number <= 0 {
		return nil, &Error{Code: "github.invalid_input", Message: "issue number must be positive"}
	}
	path, err := issuePath(repo, number)
	if err != nil {
		return nil, err
	}
	data, err := c.request(ctx, nil, "GET", path+"/dependencies/blocked_by?per_page=100", true)
	if err != nil {
		return nil, err
	}
	return decodeOpenIssues(data)
}

// gh api --paginate emits consecutive JSON arrays, one per page.
func decodeOpenIssues(data []byte) ([]Issue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	issues := make([]Issue, 0)
	pages := 0
	for {
		var page []apiIssue
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return nil, &Error{Code: "github.invalid_response", Message: "expected an issue array", Err: err}
		}
		pages++
		for _, issue := range page {
			if len(issue.PullRequest) != 0 && string(issue.PullRequest) != "null" {
				continue
			}
			if err := validateIssue(issue.Issue); err != nil {
				return nil, err
			}
			if issue.State == Open {
				issues = append(issues, issue.Issue)
			}
		}
	}
	if pages == 0 {
		return nil, &Error{Code: "github.invalid_response", Message: "missing issue array"}
	}
	return issues, nil
}

func validateIssue(issue Issue) error {
	if issue.Number <= 0 || issue.CreatedAt.IsZero() || (issue.State != Open && issue.State != Closed) || (issue.Dependencies != nil && issue.Dependencies.BlockedBy < 0) {
		return &Error{Code: "github.invalid_response", Message: "issue number, creation time, state, or dependency summary is invalid"}
	}
	return nil
}

func (c *Client) request(ctx context.Context, input []byte, method, endpoint string, paginate bool) ([]byte, error) {
	args := []string{"api", "--method", method, "--header", "Accept: application/vnd.github+json", endpoint}
	if paginate {
		args = append(args, "--paginate")
	}
	if input != nil {
		args = append(args, "--input", "-")
	}
	// Three attempts total, with 250ms then 500ms backoff. Label additions and
	// removals are idempotent, so retrying after an ambiguous network failure is safe.
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, canceled(err)
		}
		stdout, stderr, err := c.runner.Run(ctx, input, args...)
		if ctx.Err() != nil {
			return nil, canceled(ctx.Err())
		}
		if err == nil {
			return stdout, nil
		}
		failure, transient := commandError(stderr, err)
		if !transient || attempt == 2 {
			return nil, failure
		}
		timer := time.NewTimer(250 * time.Millisecond << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, canceled(ctx.Err())
		case <-timer.C:
		}
	}
	panic("unreachable")
}

func canceled(err error) *Error {
	return &Error{Code: "github.canceled", Message: err.Error(), Err: err}
}

var httpStatusPattern = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)

// gh reports API statuses on stderr as "(HTTP NNN)". Other command failures
// stay permanent unless they match a known transport failure.
func commandError(stderr []byte, cause error) (*Error, bool) {
	message := strings.TrimSpace(string(stderr))
	if message == "" {
		message = cause.Error()
	}
	code, transient := "github.command_failed", false
	status := 0
	if match := httpStatusPattern.FindStringSubmatch(message); match != nil {
		status, _ = strconv.Atoi(match[1])
	}
	var exit interface{ ExitCode() int }
	lower := strings.ToLower(message)
	switch {
	case status == 401 || (errors.As(cause, &exit) && exit.ExitCode() == 4) || strings.Contains(lower, "gh auth login"):
		code = "github.not_logged_in"
	case status == 429 || (status == 403 && (strings.Contains(lower, "rate limit") || strings.Contains(lower, "abuse"))):
		code, transient = "github.rate_limited", true
	case status == 403:
		code = "github.forbidden"
	case status == 404 || status == 410:
		code = "github.not_found"
	case status == 408 || (status >= 500 && status < 600):
		code, transient = "github.unavailable", true
	case status == 0:
		for _, marker := range []string{"error connecting to", "dial tcp", "i/o timeout", "tls handshake timeout", "connection reset", "connection refused", "unexpected eof", "network is unreachable"} {
			if strings.Contains(lower, marker) {
				code, transient = "github.unavailable", true
				break
			}
		}
	}
	return &Error{Code: code, Message: message, Err: cause}, transient
}
