package github

import (
	"regexp"
	"strconv"
	"strings"
)

// GitHub's own "(HTTP NNN)" statuses and Git transport's "returned error: NNN".
var (
	ghStatusPattern  = regexp.MustCompile(`\(http ([0-9]{3})\)`)
	gitStatusPattern = regexp.MustCompile(`returned error: ([0-9]{3})`)
)

// unavailableMarkers identify transport failures reported by gh, Git, curl,
// and TLS or HTTP/2 libraries. An outage can surface as a DNS or connect
// failure, a dropped TLS or HTTP/2 connection, or an empty or truncated reply.
var unavailableMarkers = []string{
	"error connecting to", "dial tcp", "i/o timeout", "tls handshake timeout",
	"connection reset", "connection refused", "unexpected eof", "network is unreachable",
	"could not resolve host", "failed to connect", "connection timed out",
	"operation timed out", "the remote end hung up", "unexpected disconnect",
	"ssl_error_syscall", "empty reply from server", "early eof",
	"gnutls_handshake() failed", ": eof", "was not closed cleanly", "server sent goaway",
}

// statusOf reads the HTTP status GitHub or Git reported, or 0 when the output
// carries none. It is used for lowercased failure text.
func statusOf(text string) int {
	for _, pattern := range []*regexp.Regexp{ghStatusPattern, gitStatusPattern} {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		status, err := strconv.Atoi(match[1])
		if err != nil {
			return 0
		}
		return status
	}
	return 0
}

// rateLimited reports GitHub's rate limit or secondary abuse-detection answer,
// which recovers on its own.
func rateLimited(text string) bool {
	switch statusOf(text) {
	case 429:
		return true
	case 403:
		return strings.Contains(text, "rate limit") || strings.Contains(text, "abuse")
	default:
		return false
	}
}

// Unavailable reports whether failure output shows that GitHub could not be
// reached: a transport failure, a rate limit, or a server error. GitHub
// answered every other failure, including rejected credentials, missing
// repositories, and refused permissions, and an answered status decides even
// when the text also mentions retrying. Callers defer the unavailable kind
// instead of reporting a setup problem.
func Unavailable(output []byte) bool {
	text := strings.ToLower(string(output))
	if status := statusOf(text); status != 0 {
		return status == 408 || (status >= 500 && status < 600) || rateLimited(text)
	}
	for _, marker := range unavailableMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
