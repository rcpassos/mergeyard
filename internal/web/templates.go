package web

import (
	"html/template"

	webassets "github.com/rcpassos/mergeyard/web"
)

// templateFiles lists the dashboard template groups: the layout, one file
// per page, and the partials pages share.
var templateFiles = []string{"templates/*.html", "templates/pages/*.html", "templates/partials/*.html"}

// templateHelpers is the function registry available to every dashboard
// template. Presentation helpers belong here, not in the page data.
func templateHelpers() template.FuncMap {
	return template.FuncMap{
		"outcomeAlert": outcomeAlert,
		"statusBadge":  statusBadge, "stepLabel": stepLabel, "phaseLabel": phaseLabel,
		"since": since, "until": until,
	}
}

// outcomeVariants maps the outcome words agents and CI report (criteria
// verdicts, finding severities and outcomes, fix resolutions, verification
// and check states) to the daisyUI alert variant that shows them.
var outcomeVariants = map[string]string{
	"met": "alert-success", "passed": "alert-success", "fixed": "alert-success",
	"not_met": "alert-error", "failed": "alert-error", "not_fixed": "alert-error", "blocking": "alert-error",
	"pending": "alert-info", "note": "alert-info", "no_longer_applies": "alert-info",
}

// outcomeAlert returns the alert variant for a reported outcome. A boolean is
// an acceptance criterion's met flag; anything unrecognised is a warning.
func outcomeAlert(outcome any) string {
	switch value := outcome.(type) {
	case bool:
		if value {
			return "alert-success"
		}
		return "alert-error"
	case string:
		if variant, ok := outcomeVariants[value]; ok {
			return variant
		}
	}
	return "alert-warning"
}

// parseTemplates builds the dashboard's template set from the embedded files.
func parseTemplates() (*template.Template, error) {
	return template.New("dashboard").Funcs(templateHelpers()).ParseFS(webassets.Files, templateFiles...)
}
