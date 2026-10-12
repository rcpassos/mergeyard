package harness

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rcpassos/mergeyard/internal/github"
)

const issueCommentTextLimit = 32 * 1024

// Keep a chronological suffix of complete comments. If even the newest comment
// exceeds the budget, keep its trailing text without splitting a UTF-8 rune.
func writeIssueComments(text *strings.Builder, input github.IssueComments) {
	comments := input.Comments
	start, size := len(comments), 0
	clipped := ""
	for start > 0 {
		body := comments[start-1].Body
		if len(body) > issueCommentTextLimit-size {
			if start == len(comments) {
				cut := len(body) - issueCommentTextLimit
				for cut < len(body) && !utf8.RuneStart(body[cut]) {
					cut++
				}
				clipped = body[cut:]
				start--
			}
			break
		}
		size += len(body)
		start--
	}
	text.WriteString("\n## Issue comments\n\n")
	if input.OmittedUntrusted > 0 {
		fmt.Fprintf(text, "%d untrusted comments omitted.\n\n", input.OmittedUntrusted)
	}
	if start > 0 || clipped != "" {
		fmt.Fprintf(text, "Truncated at the %d-byte comment text cap, retaining the newest comments; %d older comments omitted.", issueCommentTextLimit, start)
		if clipped != "" {
			text.WriteString(" The beginning of the newest comment was omitted.")
		}
		text.WriteString("\n\n")
	}
	if len(comments) == 0 {
		text.WriteString("No trusted human comments.\n")
	}
	for i := start; i < len(comments); i++ {
		comment := comments[i]
		fmt.Fprintf(text, "### @%s — %s\n\n", comment.User.Login, comment.CreatedAt.UTC().Format(time.RFC3339))
		body := comment.Body
		if clipped != "" {
			body = clipped
		}
		text.WriteString(body + "\n\n")
	}
}
