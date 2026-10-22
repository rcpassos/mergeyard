package web

import (
	"strings"
	"unicode"
)

// timelineCategory owns both the filter's label and its icon. Review and fix
// events describe phases; escalation events describe a run needing attention.
type timelineCategory struct {
	Key, Label, Icon string
}

var timelineCategories = []timelineCategory{
	{"run", "Run", "activity"},
	{"phase", "Phase", "layers"},
	{"pr", "PR", "git-pull-request"},
	{"ci", "CI", "list-checks"},
	{"harness", "Harness", "terminal"},
	{"merge", "Merge", "git-merge"},
	{"publication", "Publication", "send"},
	{"reconcile", "Reconcile", "refresh-cw"},
	{"scheduler", "Scheduler", "calendar-clock"},
	{"other", "Other", "info"},
}

func (entry timelineEntry) Category() timelineCategory {
	prefix, _, _ := strings.Cut(entry.Type, ".")
	switch prefix {
	case "review", "fix":
		prefix = "phase"
	case "escalation":
		prefix = "run"
	}
	for _, category := range timelineCategories {
		if category.Key == prefix {
			return category
		}
	}
	return timelineCategories[len(timelineCategories)-1]
}

// Name derives readable copy rather than maintaining a second event registry.
// Unknown types follow the same rule, so newer runtime events remain readable.
func (entry timelineEntry) Name() string {
	words := strings.FieldsFunc(entry.Type, func(r rune) bool { return r == '.' || r == '_' || r == '-' || unicode.IsSpace(r) })
	if len(words) == 0 {
		return "Unknown event"
	}
	for i, word := range words {
		switch strings.ToLower(word) {
		case "pr", "ci":
			words[i] = strings.ToUpper(word)
		default:
			if i == 0 {
				letters := []rune(word)
				letters[0] = unicode.ToUpper(letters[0])
				words[i] = string(letters)
			}
		}
	}
	return strings.Join(words, " ")
}
