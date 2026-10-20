package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/proc"
	"github.com/rcpassos/mergeyard/internal/repository"
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
	UpdatedAt    time.Time          `json:"updated_at"`
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
	// Never wait on an interactive prompt from an unattended control plane.
	result, err := proc.Run(ctx, proc.Request{
		Executable: "gh",
		Args:       args,
		Env:        append(os.Environ(), "GH_PROMPT_DISABLED=1"),
		Stdin:      bytes.NewReader(input),
	})
	if errors.Is(err, exec.ErrWaitDelay) && result.ExitCode == 0 {
		// gh exited successfully; a credential helper outlived it holding the
		// output pipes. The request itself completed.
		err = nil
	}
	return result.Stdout, result.Stderr, err
}

// Client wraps the authenticated gh CLI and caches issue authorization snapshots.
type Client struct {
	runner     CommandRunner
	timeout    time.Duration
	trustMu    sync.Mutex
	trustCache map[string]trustCacheEntry
	login      string // authenticated comment author; protected by trustMu
}

// Options configures the gh CLI adapter. Zero fields use the defaults.
type Options struct {
	// Timeout bounds one gh invocation, including its internal retries.
	Timeout time.Duration
}

// defaultTimeout bounds a gh invocation when Options leaves Timeout unset.
const defaultTimeout = 2 * time.Minute

// New uses the installed gh executable when runner is nil.
func New(runner CommandRunner) *Client { return NewWithOptions(runner, Options{}) }

// NewWithOptions selects the request deadline.
func NewWithOptions(runner CommandRunner, options Options) *Client {
	if runner == nil {
		runner = ghRunner{}
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	return &Client{runner: runner, timeout: options.Timeout}
}

// Error retains the GitHub adapter API while sharing the runtime error type.
type Error = fault.Error

func codedError(code, message string, cause error) *Error {
	if cause != nil {
		return &Error{Code: code, Err: fmt.Errorf("%s: %w", message, cause)}
	}
	return &Error{Code: code, Err: errors.New(message)}
}

func issuePath(repo string, number int) (string, error) {
	if !repository.ValidName(repo) || number < 0 {
		return "", codedError("github.invalid_input", "expected owner/repo and a positive issue number", nil)
	}
	path := "repos/" + repo + "/issues"
	if number > 0 {
		path += fmt.Sprintf("/%d", number)
	}
	return path, nil
}

// ListLabeledIssues reads every open issue carrying any of the labels. GitHub
// ANDs the labels of one request, so each label is read separately with its own
// pagination and the results merge by issue number. PRs are excluded; requests
// stay proportional to labelled issues instead of every open issue and PR.
func (c *Client) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]Issue, error) {
	path, err := issuePath(repo, 0)
	if err != nil {
		return nil, err
	}
	unique := make([]string, 0, len(labels))
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return nil, codedError("github.invalid_input", "expected nonempty labels", nil)
		}
		name := strings.ToLower(label)
		if seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, label)
	}
	if len(unique) == 0 {
		return nil, codedError("github.invalid_input", "expected at least one label", nil)
	}
	issues := make([]Issue, 0)
	numbers := make(map[int]bool)
	for _, label := range unique {
		data, err := c.request(ctx, nil, "GET", path+"?state=open&per_page=100&labels="+url.QueryEscape(label), true)
		if err != nil {
			return nil, err
		}
		page, err := decodeOpenIssues(data)
		if err != nil {
			return nil, err
		}
		for _, issue := range page {
			if !numbers[issue.Number] {
				numbers[issue.Number] = true
				issues = append(issues, issue)
			}
		}
	}
	return issues, nil
}

type apiIssue struct {
	Issue
	PullRequest json.RawMessage `json:"pull_request"`
}

// IssueState reads the current issue state from GitHub.
func (c *Client) IssueState(ctx context.Context, repo string, number int) (State, error) {
	issue, err := c.GetIssue(ctx, repo, number)
	return issue.State, err
}

// GetIssue reads fresh issue state and labels, including closed issues.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	if number <= 0 {
		return Issue{}, codedError("github.invalid_input", "issue number must be positive", nil)
	}
	path, err := issuePath(repo, number)
	if err != nil {
		return Issue{}, err
	}
	data, err := c.request(ctx, nil, "GET", path, false)
	if err != nil {
		return Issue{}, err
	}
	var issue apiIssue
	if err := json.Unmarshal(data, &issue); err != nil {
		return Issue{}, codedError("github.invalid_response", "expected an issue", err)
	}
	if err := validateIssue(issue.Issue); err != nil {
		return Issue{}, err
	}
	if len(issue.PullRequest) != 0 && string(issue.PullRequest) != "null" {
		return Issue{}, codedError("github.invalid_response", "expected an issue, received a pull request", nil)
	}
	return issue.Issue, nil
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
		return codedError("github.invalid_input", "cannot encode label", err)
	}
	_, err = c.request(ctx, input, "POST", path, false)
	return err
}

// RemoveLabel ensures one literal label is absent without touching other labels.
// An already absent label succeeds; missing issues and repositories still fail.
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	path, err := labelPath(repo, number, label)
	if err != nil {
		return err
	}
	_, err = c.request(ctx, nil, "DELETE", path+"/"+url.PathEscape(label), false)
	var failure *Error
	if errors.As(err, &failure) && failure.Code == "github.label_not_found" {
		return nil
	}
	return err
}

func labelPath(repo string, number int, label string) (string, error) {
	if number <= 0 || label == "" {
		return "", codedError("github.invalid_input", "issue number must be positive and label nonempty", nil)
	}
	path, err := issuePath(repo, number)
	return path + "/labels", err
}

// UnresolvedBlockers reads native dependencies, including cross-repository issues,
// and returns only those still open. Body text is never interpreted as a dependency.
func (c *Client) UnresolvedBlockers(ctx context.Context, repo string, number int) ([]Issue, error) {
	if number <= 0 {
		return nil, codedError("github.invalid_input", "issue number must be positive", nil)
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
			return nil, codedError("github.invalid_response", "expected an issue array", err)
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
		return nil, codedError("github.invalid_response", "missing issue array", nil)
	}
	return issues, nil
}

func validateIssue(issue Issue) error {
	if issue.Number <= 0 || issue.CreatedAt.IsZero() || (issue.State != Open && issue.State != Closed) || (issue.Dependencies != nil && issue.Dependencies.BlockedBy < 0) {
		return codedError("github.invalid_response", "issue number, creation time, state, or dependency summary is invalid", nil)
	}
	return nil
}

func (c *Client) request(ctx context.Context, input []byte, method, endpoint string, paginate bool) ([]byte, error) {
	return c.requestWithAttempts(ctx, input, method, endpoint, paginate, 3)
}

func (c *Client) requestWithAttempts(ctx context.Context, input []byte, method, endpoint string, paginate bool, attempts int) ([]byte, error) {
	args := []string{"api", "--method", method, "--header", "Accept: application/vnd.github+json", endpoint}
	if paginate {
		args = append(args, "--paginate")
	}
	if input != nil {
		args = append(args, "--input", "-")
	}
	// One deadline covers the whole request, including its retries, so a hung
	// gh cannot outlive the budget. A caller-supplied deadline stays earlier.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// Retry-safe operations get three attempts with 250ms then 500ms backoff.
	// PR creation and body writes get one: replaying an ambiguous write may
	// create duplicates or overwrite human edits made after the original request.
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, contextError(err)
		}
		stdout, stderr, err := c.runner.Run(ctx, input, args...)
		if err := ctx.Err(); err != nil {
			return nil, contextError(err)
		}
		if err == nil {
			return stdout, nil
		}
		failure, transient := commandError(stderr, err)
		if !transient || attempt == attempts-1 {
			return nil, failure
		}
		timer := time.NewTimer(250 * time.Millisecond << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, contextError(ctx.Err())
		case <-timer.C:
		}
	}
	panic("unreachable")
}

// contextError classifies how a bounded gh invocation ended: an expired
// deadline is a timeout, any other cancellation is a canceled request.
func contextError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "github.timeout", Err: err}
	}
	return &Error{Code: "github.canceled", Err: err}
}

// gh reports API statuses on stderr as "(HTTP NNN)". A refused request is
// permanent only when GitHub names a permanent cause; a generic refusal
// stays a retryable command failure, like an unrecognized gh error.
func commandError(stderr []byte, cause error) (*Error, bool) {
	message := strings.TrimSpace(string(stderr))
	if message == "" {
		message = cause.Error()
	}
	code, transient := "github.command_failed", false
	lower := strings.ToLower(message)
	status := statusOf(lower)
	var exit interface{ ExitCode() int }
	switch {
	case status == 401 || (errors.As(cause, &exit) && exit.ExitCode() == 4) || strings.Contains(lower, "gh auth login"):
		code = "github.not_logged_in"
	case rateLimited(lower):
		// gh stderr carries no reset/Retry-After headers. Surface the limit
		// immediately so callers can defer polling rather than retry too soon.
		code = "github.rate_limited"
	case status == 403:
		code = "github.forbidden"
	case status == 404 && strings.Contains(lower, "label does not exist"):
		code = "github.label_not_found"
	case status == 404 || status == 410:
		code = "github.not_found"
	case status == 400 || status == 422:
		switch {
		case throttledRejection(lower):
			// The issue-comments API answers its secondary rate limit with 422
			// when content is created too quickly. The limit clears on its own,
			// so this waits like any other rate limit.
			code = "github.rate_limited"
		case permanentRejection(lower):
			// The request itself is invalid: a body over the size limit or a
			// validation error naming a field. Retrying cannot change that.
			code = "github.invalid_input"
		default:
			// GitHub refused the request without naming a permanent cause.
			// Treat it like any other command failure and retry it later.
			code = "github.command_failed"
		}
	case Unavailable([]byte(lower)):
		code, transient = "github.unavailable", true
	}
	return codedError(code, message, cause), transient
}
