package web

import (
	"io/fs"
	"testing"

	webassets "github.com/rcpassos/mergeyard/web"
)

func TestDashboardTemplatesSplitIntoLayoutPagesAndPartials(t *testing.T) {
	set, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	for _, name := range []string{
		"layout", "content", "swap", "harness-pills",
		"dashboard-page", "queue-page", "history-page", "run-detail", "settings", "boot", "locked-content",
		"scheduler", "queue-notices", "queue-table", "queue-table-head", "queue-group-row", "run-row", "human-action", "escalation", "verification", "phase-output",
	} {
		if set.Lookup(name) == nil {
			t.Errorf("template %q is not defined", name)
		}
	}
	for _, dir := range []string{"templates/pages", "templates/partials"} {
		files, err := fs.Glob(webassets.Files, dir+"/*.html")
		if err != nil || len(files) == 0 {
			t.Errorf("no embedded templates under %s (err %v)", dir, err)
		}
	}
}
