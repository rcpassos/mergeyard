package doctor

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
)

type repositoryFileType uint8

const (
	fileMissing repositoryFileType = iota
	fileRegular
	fileDirectory
	fileGitlink
	fileUnverifiable
)

func repositoryFilePaths(repo config.Repository) []string {
	paths := []string{"CLAUDE.md", "AGENTS.md"}
	seen := map[string]bool{"CLAUDE.md": true, "AGENTS.md": true}
	for _, role := range []config.Role{repo.Implementer, repo.Reviewer} {
		for _, skill := range role.Skills {
			if !skillName.MatchString(skill) || strings.Contains(skill, ":") {
				continue
			}
			path := repositorySkillPath(role.Agent, skill)
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// cat-file's symlink failures have a length-framed payload, which can contain
// newlines. Consume its byte count rather than treating it as another result.
func repositoryFileTypes(output []byte, paths []string) (map[string]repositoryFileType, error) {
	files := make(map[string]repositoryFileType, len(paths))
	for _, path := range paths {
		line, rest, ok := bytes.Cut(output, []byte{'\n'})
		if !ok {
			return nil, fmt.Errorf("missing result for %s", path)
		}
		output = rest
		switch string(line) {
		case "blob":
			files[path] = fileRegular
		case "tree":
			files[path] = fileDirectory
		case "commit":
			files[path] = fileGitlink
		case "refs/heads/doctor:" + path + " missing":
			files[path] = fileMissing
		default:
			fields := strings.Fields(string(line))
			if len(fields) != 2 {
				return nil, fmt.Errorf("unexpected result for %s", path)
			}
			kind := fileMissing
			switch fields[0] {
			case "dangling", "loop", "notdir":
			case "symlink":
				kind = fileUnverifiable
			default:
				return nil, fmt.Errorf("unexpected object type for %s", path)
			}
			size, err := strconv.Atoi(fields[1])
			if err != nil || size < 0 || size >= len(output) || output[size] != '\n' {
				return nil, fmt.Errorf("invalid symlink response for %s", path)
			}
			output = output[size+1:]
			files[path] = kind
		}
	}
	if len(output) != 0 {
		return nil, fmt.Errorf("unexpected trailing repository file results")
	}
	return files, nil
}
