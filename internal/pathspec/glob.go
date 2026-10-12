// Package pathspec validates repository-relative Git glob patterns. Matching
// remains Git's responsibility so staging and diagnostics use identical rules.
package pathspec

import (
	"fmt"
	"strings"
)

func Validate(pattern string) error {
	if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "/") || strings.ContainsRune(pattern, 0) {
		return fmt.Errorf("expected a nonempty repository-relative Git glob without NUL")
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == ".." {
			return fmt.Errorf("pattern must stay inside the repository")
		}
	}
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
			if i == len(pattern) {
				return fmt.Errorf("unfinished escape")
			}
		case '[':
			i++
			if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
				i++
			}
			// A closing bracket in the first position is a literal member.
			if i < len(pattern) && pattern[i] == ']' {
				i++
			}
			for ; i < len(pattern) && pattern[i] != ']'; i++ {
				if pattern[i] == '\\' {
					i++
				} else if strings.HasPrefix(pattern[i:], "[:") {
					end := strings.Index(pattern[i+2:], ":]")
					if end < 0 {
						return fmt.Errorf("unfinished POSIX character class")
					}
					class := pattern[i+2 : i+2+end]
					if !strings.Contains("|alnum|alpha|blank|cntrl|digit|graph|lower|print|punct|space|upper|xdigit|", "|"+class+"|") {
						return fmt.Errorf("unknown POSIX character class %q", class)
					}
					i += end + 3
				}
			}
			if i >= len(pattern) {
				return fmt.Errorf("unfinished character class")
			}
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if (i > 0 && pattern[i-1] != '/') || (i+2 < len(pattern) && pattern[i+2] != '/') {
					return fmt.Errorf("** must occupy a complete path component")
				}
				i++
			}
		}
	}
	return nil
}

// Glob treats a trailing slash as the whole directory subtree, including when
// the directory name itself contains wildcards.
func Glob(pattern string) string {
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return pattern
}
