// Package repository defines shared GitHub repository naming rules.
package repository

import (
	"regexp"
	"strings"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`)

// ValidName checks an owner/repo name using the same rules for configuration
// and GitHub API paths: at most 39 owner characters and 100 repository characters.
func ValidName(repo string) bool {
	_, name, _ := strings.Cut(repo, "/")
	return namePattern.MatchString(repo) && name != "." && name != ".."
}
