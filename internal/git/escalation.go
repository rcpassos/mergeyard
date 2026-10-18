package git

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/rcpassos/mergeyard/internal/escalation"
)

// EscalationDiff reads exact paths and line counts for the approved commit.
// Pin line counting, heuristic, submodule and rename policies instead of
// inheriting machine or repository diff settings.
// User and system attributes are excluded; repository .gitattributes still apply.
// Pin Git's 512 MiB binary cutoff as well, so local limits cannot hide text lines.
// Renames retain both paths, but count only the actual changed lines once.
func (m *Manager) EscalationDiff(ctx context.Context, run Run, head string) ([]escalation.FileChange, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.Metadata)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := m.inspectReview(ctx, run); err != nil {
		return nil, err
	}
	if !shaPattern.MatchString(head) || !shaPattern.MatchString(run.BaseSHA) {
		return nil, failure("git.escalation_diff", run.Path, errors.New("invalid diff target"))
	}
	out, err := commandWithEnv(ctx, run.Path, "git.escalation_diff", []string{"GIT_ATTR_NOSYSTEM=1"}, "-c", "core.attributesFile=", "-c", "core.bigFileThreshold=512m", "diff", "--numstat", "-z", "--diff-algorithm=myers", "--no-indent-heuristic", "--ignore-submodules=none", "--find-renames=50%", "-l1000", "--no-ext-diff", "--no-textconv", run.BaseSHA+"..."+head, "--")
	if err != nil {
		return nil, err
	}
	changes := []escalation.FileChange{}
	records := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\t", 3)
		if len(parts) != 3 {
			return nil, failure("git.escalation_diff", run.Path, errors.New("invalid numstat record"))
		}
		change := escalation.FileChange{Path: parts[2]}
		if change.Path == "" {
			if i+2 >= len(records) || records[i+1] == "" || records[i+2] == "" {
				return nil, failure("git.escalation_diff", run.Path, errors.New("invalid rename record"))
			}
			change.OldPath, change.Path = records[i+1], records[i+2]
			i += 2
		}
		if parts[0] != "-" || parts[1] != "-" {
			change.Additions, err = strconv.Atoi(parts[0])
			if err != nil || change.Additions < 0 {
				return nil, failure("git.escalation_diff", run.Path, errors.New("invalid addition count"))
			}
			change.Deletions, err = strconv.Atoi(parts[1])
			if err != nil || change.Deletions < 0 {
				return nil, failure("git.escalation_diff", run.Path, errors.New("invalid deletion count"))
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}
