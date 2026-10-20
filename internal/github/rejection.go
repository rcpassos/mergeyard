package github

import (
	"regexp"
	"strings"
)

// GitHub refuses a request with 400 or 422 and a body that explains the
// refusal. Only some of those refusals are permanent: a body over the size
// limit and a validation error naming a field cannot be fixed by waiting,
// while the secondary rate limit for content created too quickly clears on its
// own and the refusal carries no permanent cause.

// throttleMarkers identify GitHub's secondary rate limit for content that was
// created too quickly. The issue-comments API answers that limit with 422
// instead of the 403 and 429 that other endpoints use.
var throttleMarkers = []string{
	"spammed",
	"too quickly",
	"rate limit",
}

// sizeMarkers identify a body that is over GitHub's size limit.
var sizeMarkers = []string{
	"body is too long",
	"body too long",
	"body is too large",
	"body too large",
}

// fieldPattern identifies a validation error that names the field it rejects.
var fieldPattern = regexp.MustCompile(`"field"\s*:\s*"[^"]+"`)

// containsAny reports whether text carries one of the markers.
func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// throttledRejection reports whether GitHub refused the request because the
// content was created too quickly. The answer clears on its own.
func throttledRejection(text string) bool {
	return containsAny(text, throttleMarkers)
}

// permanentRejection reports whether GitHub refused the request for a reason
// that waiting cannot fix: a body over the size limit, or a validation error
// naming a field.
func permanentRejection(text string) bool {
	return containsAny(text, sizeMarkers) || fieldPattern.MatchString(text)
}
