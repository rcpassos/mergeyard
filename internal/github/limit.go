package github

import "unicode/utf8"

// MaxBodyBytes is GitHub's maximum length for a pull request body or comment.
// Mergeyard measures bytes, which is at least as strict as GitHub's character
// limit and keeps multibyte verification excerpts safe.
const MaxBodyBytes = 65536

const bodyTruncatedSuffix = "\n\n[truncated by Mergeyard to fit GitHub's size limit]"

// CapCommentBody truncates a comment before it reaches GitHub. The opening
// marker line stays intact so an ambiguous or repeated write is still
// reconcilable, and truncation never splits a UTF-8 rune.
func CapCommentBody(body string) string {
	if len(body) <= MaxBodyBytes {
		return body
	}
	return truncateUTF8(body, MaxBodyBytes-len(bodyTruncatedSuffix)) + bodyTruncatedSuffix
}

func truncateUTF8(text string, limit int) string {
	if limit >= len(text) {
		return text
	}
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
