// Package acceptance defines the issue criteria shared by all agent phases.
package acceptance

import (
	"fmt"
	"regexp"
	"strings"
)

type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

var heading = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.+?)\s*#*\s*$`)
var item = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]+[.)])[ \t]+(.+)$`)

// Extract reads list items only within an explicit level-two acceptance heading.
// IDs follow source order, regardless of whether a checklist box is checked.
func Extract(body string) []Criterion {
	criteria := []Criterion{}
	active := false
	fence := ""
	current := -1
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			size := 0
			for size < len(trimmed) && trimmed[size] == trimmed[0] {
				size++
			}
			marker := trimmed[:size]
			if fence == "" {
				fence = marker
			} else if marker[0] == fence[0] && len(marker) >= len(fence) && strings.TrimSpace(trimmed[size:]) == "" {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if match := heading.FindStringSubmatch(line); match != nil {
			current = -1
			if len(match[1]) <= 2 {
				active = match[1] == "##" && strings.EqualFold(strings.TrimSpace(match[2]), "Acceptance criteria")
			}
			continue
		}
		if !active {
			continue
		}
		if match := item.FindStringSubmatch(line); match != nil {
			content := strings.TrimSpace(match[1])
			if len(content) >= 3 && content[0] == '[' && content[2] == ']' && strings.ContainsRune(" xX", rune(content[1])) && (len(content) == 3 || content[3] == ' ' || content[3] == '\t') {
				content = strings.TrimSpace(content[3:])
			}
			if content == "" {
				current = -1
				continue
			}
			criteria = append(criteria, Criterion{ID: fmt.Sprintf("AC%d", len(criteria)+1), Text: content})
			current = len(criteria) - 1
		} else if current >= 0 && trimmed != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			criteria[current].Text += "\n" + trimmed
		} else if trimmed != "" {
			current = -1
		}
	}
	return criteria
}

// Input renders the same numbered contract in every phase's context file.
func Input(body string, reviewing bool) string {
	var text strings.Builder
	text.WriteString("\n## Acceptance criteria contract\n\n")
	criteria := Extract(body)
	if len(criteria) == 0 {
		text.WriteString("no acceptance criteria found. Return criteria: [].\n")
	} else {
		for _, c := range criteria {
			fmt.Fprintf(&text, "%s: %s\n", c.ID, c.Text)
		}
		text.WriteString("\nReturn exactly one criteria entry for each listed ID. Do not invent IDs.\n")
	}
	if reviewing {
		text.WriteString("Independently spot-check at least one criterion when criteria exist, and describe the command/result or observation in its note. Use met, not_met, or unverified. Every not_met verdict must be backed by a blocking finding that identifies the criterion ID.\n")
	} else {
		text.WriteString("For each criterion report met and evidence: the command run and its result, or what was observed. Describe missing verification explicitly.\n")
	}
	return text.String()
}
