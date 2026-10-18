package github_test

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

func TestUnavailableClassifiesTransportRateLimitAndServerFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{"gh transport", "error connecting to api.github.com", true},
		{"gh timeout", "dial tcp 140.82.121.6:443: i/o timeout", true},
		{"gh rate limit", "API rate limit exceeded (HTTP 429)", true},
		{"gh secondary rate limit", "You have exceeded a secondary rate limit (HTTP 403)", true},
		{"gh abuse detection", "You have triggered an abuse detection mechanism (HTTP 403)", true},
		{"gh server error", "gh: Server Error (HTTP 503)", true},
		{"gh request timeout", "gh: Request Timeout (HTTP 408)", true},
		{"gh eof", `Get "https://api.github.com/rate_limit": EOF`, true},
		{"git dns failure", "fatal: unable to access 'https://github.com/octo/repo.git/': Could not resolve host: github.com", true},
		{"git connection failure", "fatal: unable to access 'https://github.com/octo/repo.git/': Failed to connect to github.com port 443", true},
		{"git server error", "fatal: unable to access 'https://github.com/octo/repo.git/': The requested URL returned error: 502", true},
		{"git disconnect", "fatal: the remote end hung up unexpectedly", true},
		{"curl empty reply", "error: RPC failed; curl 52 Empty reply from server", true},
		{"curl early eof", "fatal: early EOF", true},
		{"openssl syscall", "error:1404C2E5:SSL routines:SSL_ERROR_SYSCALL:system lib", true},
		{"gnutls handshake", "fatal: unable to access 'https://github.com/octo/repo.git/': gnutls_handshake() failed: Error in the pull function.", true},
		{"http2 goaway", "curl 92 HTTP/2 stream 5 was not closed cleanly: GOAWAY", true},
		{"http2 server goaway", `Get "https://api.github.com/rate_limit": http2: server sent GOAWAY and closed the connection`, true},
		{"not logged in", "gh: To get started with GitHub CLI, please run: gh auth login", false},
		{"missing repository", "gh: Not Found (HTTP 404)", false},
		{"forbidden", "gh: Resource not accessible by personal access token (HTTP 403)", false},
		{"missing label", "gh: Label does not exist (HTTP 404)", false},
		{"authentication failed", "fatal: Authentication failed for 'https://github.com/octo/repo.git/'", false},
		{"missing base branch", "fatal: unable to access 'https://github.com/octo/repo.git/': The requested URL returned error: 404", false},
		{"repository not found", "fatal: repository 'https://github.com/octo/repo.git/' not found", false},
		{"empty output", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := github.Unavailable([]byte(tc.output)); got != tc.want {
				t.Fatalf("Unavailable(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}
