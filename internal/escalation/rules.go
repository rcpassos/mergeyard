// Package escalation evaluates advisory review flags from a pinned Git diff.
package escalation

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type Rules struct {
	MaxChangedLines int               `yaml:"max_changed_lines"`
	Paths           map[string]string `yaml:"paths"`
	NewDependencies bool              `yaml:"new_dependencies"`
}

func Defaults() Rules {
	return Rules{MaxChangedLines: 400, NewDependencies: true, Paths: map[string]string{
		".github/**": "ci/deploy", "**/Dockerfile*": "ci/deploy", "**/*.tf": "ci/deploy", "**/migrations/**": "migration",
	}}
}

type FileChange struct {
	Path      string
	OldPath   string // present for a rename; line counts belong to the change once
	Additions int
	Deletions int
}

type Flag struct {
	Reason string   `json:"reason"`
	Files  []string `json:"files"`
}

// ValidateGlob accepts slash-separated path globs. ** as a complete segment
// matches zero or more directories; other segments use path.Match syntax.
func ValidateGlob(pattern string) error {
	if pattern == "" || strings.HasPrefix(pattern, "/") || strings.ContainsRune(pattern, 0) {
		return fmt.Errorf("expected a relative path glob")
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" || segment == ".." {
			return fmt.Errorf("empty and parent path segments are unsupported")
		}
		if segment != "**" {
			if _, err := path.Match(segment, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func matches(pattern, name string) bool {
	parts, names := strings.Split(pattern, "/"), strings.Split(name, "/")
	// Dynamic programming keeps repeated ** segments bounded by path length.
	matched := make([]bool, len(names)+1)
	matched[0] = true
	for _, part := range parts {
		next := make([]bool, len(names)+1)
		for j := 0; j <= len(names); j++ {
			if part == "**" {
				next[j] = matched[j] || (j > 0 && next[j-1])
			} else if j > 0 {
				ok, _ := path.Match(part, names[j-1])
				next[j] = matched[j-1] && ok
			}
		}
		matched = next
	}
	return matched[len(names)]
}

// Evaluate groups overlapping rules by reason and sorts reasons and files.
// Manifest edits count; lockfiles alone do not. Binary files have zero lines
// but still participate in path and manifest rules.
func Evaluate(rules Rules, changes []FileChange) []Flag {
	byReason := map[string]map[string]bool{}
	add := func(reason, file string) {
		if byReason[reason] == nil {
			byReason[reason] = map[string]bool{}
		}
		byReason[reason][file] = true
	}
	lines := 0
	for _, file := range changes {
		lines += file.Additions + file.Deletions
		for _, name := range []string{file.OldPath, file.Path} {
			if name == "" {
				continue
			}
			for pattern, reason := range rules.Paths {
				if matches(pattern, name) {
					add(reason, name)
				}
			}
			if rules.NewDependencies {
				switch path.Base(name) {
				case "go.mod", "package.json", "pyproject.toml", "Cargo.toml", "Gemfile":
					add("dependency manifests", name)
				}
			}
		}
	}
	if rules.MaxChangedLines > 0 && lines > rules.MaxChangedLines {
		for _, file := range changes {
			add(fmt.Sprintf("size: %d changed lines exceeds %d", lines, rules.MaxChangedLines), file.Path)
			if file.OldPath != "" {
				add(fmt.Sprintf("size: %d changed lines exceeds %d", lines, rules.MaxChangedLines), file.OldPath)
			}
		}
	}
	flags := []Flag{}
	for reason, names := range byReason {
		files := make([]string, 0, len(names))
		for file := range names {
			files = append(files, file)
		}
		sort.Strings(files)
		flags = append(flags, Flag{Reason: reason, Files: files})
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].Reason < flags[j].Reason })
	return flags
}
