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
		"layout", "content",
		"queue-page", "run-detail", "settings", "boot", "locked-content",
		"scheduler", "queue-group", "run-row", "human-action", "escalation", "verification", "phase-output",
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

func TestDashboardTemplatesExposeTheHelperRegistry(t *testing.T) {
	set, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	// Every registered helper must be callable from any dashboard template.
	for name := range templateHelpers() {
		if _, err := set.New("probe").Parse(`{{if false}}{{` + name + `}}{{end}}`); err != nil {
			t.Errorf("helper %q is not registered on the template set: %v", name, err)
		}
	}
}
